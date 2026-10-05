package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/chenhg5/cc-connect/core"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

func init() {
	core.RegisterPlatform("discord", New)
}

const maxDiscordLen = 1900

type replyContext struct {
	channelID string
	messageID string
	threadID  string
	// sourceChannelID is the channel that owns messageID (parent, when a
	// thread was just created from that message). Reactions must use this,
	// not the send target. Reply references must use the send target —
	// Discord rejects REPLIES_CANNOT_REFERENCE_OTHER_CHANNEL if the
	// reference channel is the parent while the body is posted in the thread.
	sourceChannelID string
	// autoThreadInitialName is the placeholder name used when this bot
	// created the thread. Semantic rename only proceeds while Discord
	// still shows this name (a human rename is left alone).
	autoThreadInitialName string
}

// interactionReplyCtx handles Discord slash command (Application Command)
// responses. The first reply edits the deferred interaction response;
// subsequent replies use followup messages.
type interactionReplyCtx struct {
	interaction *discordgo.Interaction
	channelID   string
	mu          sync.Mutex
	firstDone   bool
}

type progressPlatform struct {
	*Platform
}

type Platform struct {
	token                      string
	allowFrom                  string
	guildID                    string // optional: per-guild registration (instant) vs global (up to 1h propagation)
	progressStyle              string
	groupReplyAllGuilds        []string // guild IDs where groupReplyAll is active; "*" = all guilds
	shareSessionInChannel      bool
	threadIsolation            bool
	threadRequireMention       bool // if true, threads stay gated like channels (multi-bot)
	respondToAtEveryoneAndHere bool
	reactions                  bool   // Hermes-style 👀/✅/❌ on the user's message
	replyToMode                string // off | first | all (Hermes Discord reply_to_mode)
	proxyURL                   *url.URL
	handler                    core.MessageHandler
	botID                      string
	appID                      string
	channelNameCache           sync.Map // channelID -> name
	botRoleIDs                 sync.Map // guildID -> bot managed role ID
	readyCh                    chan struct{}
	seenMsgs                   sync.Map // message ID dedup: prevents duplicate MessageCreate events
	seenInteractions           sync.Map // interaction ID dedup: prevents duplicate slash/button events
	self                       core.Platform
	participatedThreads        participatedThreadSet
	threadStorePath            string
	placeholderStorePath       string
	autoThreadPlaceholders     sync.Map // threadID -> placeholder name used at create

	mu               sync.RWMutex
	session          *discordgo.Session
	cancel           context.CancelFunc
	stopping         bool
	everConnected    bool
	lifecycleHandler core.PlatformLifecycleHandler
}

const (
	discordInitialReconnectBackoff = 5 * time.Second
	discordMaxReconnectBackoff     = 5 * time.Minute
)

func New(opts map[string]any) (core.Platform, error) {
	token, _ := opts["token"].(string)
	if token == "" {
		return nil, fmt.Errorf("discord: token is required")
	}
	allowFrom, _ := opts["allow_from"].(string)
	core.CheckAllowFrom("discord", allowFrom)
	guildID, _ := opts["guild_id"].(string)
	var groupReplyAllGuilds []string
	if guilds, _ := opts["group_reply_all_guilds"].(string); guilds != "" {
		for _, g := range strings.Split(guilds, ",") {
			if g = strings.TrimSpace(g); g != "" {
				groupReplyAllGuilds = append(groupReplyAllGuilds, g)
			}
		}
	} else if all, _ := opts["group_reply_all"].(bool); all {
		groupReplyAllGuilds = []string{"*"}
	}
	shareSessionInChannel, _ := opts["share_session_in_channel"].(bool)
	threadIsolation, _ := opts["thread_isolation"].(bool)
	threadRequireMention, _ := opts["thread_require_mention"].(bool)
	respondToAtEveryoneAndHere, _ := opts["respond_to_at_everyone_and_here"].(bool)
	reactions := true
	if v, ok := opts["reactions"].(bool); ok {
		reactions = v
	}
	replyToMode, err := parseDiscordReplyToMode(opts["reply_to_mode"])
	if err != nil {
		return nil, err
	}
	// Default to "compact" so streaming edits work out of the box (Discord)
	// supports MessageUpdater.UpdateMessage). Users can opt back into the old
	// "send entire reply at once" behavior with progress_style = "legacy".
	progressStyle := "compact"
	if v, ok := opts["progress_style"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "":
			// keep default
		case "legacy":
			progressStyle = "legacy"
		case "compact", "card":
			progressStyle = strings.ToLower(strings.TrimSpace(v))
		default:
			return nil, fmt.Errorf("discord: invalid progress_style %q (want legacy, compact, or card)", v)
		}
	}

	var proxyU *url.URL
	if proxyStr, _ := opts["proxy"].(string); proxyStr != "" {
		u, err := url.Parse(proxyStr)
		if err != nil {
			return nil, fmt.Errorf("discord: invalid proxy URL %q: %w", proxyStr, err)
		}
		if user, _ := opts["proxy_username"].(string); user != "" {
			pass, _ := opts["proxy_password"].(string)
			u.User = url.UserPassword(user, pass)
		}
		proxyU = u
	}

	base := &Platform{
		token:                      token,
		allowFrom:                  allowFrom,
		guildID:                    guildID,
		progressStyle:              progressStyle,
		groupReplyAllGuilds:        groupReplyAllGuilds,
		shareSessionInChannel:      shareSessionInChannel,
		readyCh:                    make(chan struct{}),
		threadIsolation:            threadIsolation,
		threadRequireMention:       threadRequireMention,
		respondToAtEveryoneAndHere: respondToAtEveryoneAndHere,
		reactions:                  reactions,
		replyToMode:                replyToMode,
		proxyURL:                   proxyU,
		threadStorePath:            participatedThreadStorePath(opts),
		placeholderStorePath:       autoThreadPlaceholderStorePath(opts),
	}
	base.loadParticipatedThreads()
	base.loadAutoThreadPlaceholders()
	if progressStyle == "compact" || progressStyle == "card" {
		wrapped := &progressPlatform{Platform: base}
		base.self = wrapped
		return wrapped, nil
	}
	base.self = base
	return base, nil
}

func (p *Platform) Name() string { return "discord" }

func (p *Platform) selfPlatform() core.Platform {
	if p != nil && p.self != nil {
		return p.self
	}
	return p
}

func (p *Platform) dispatchMessage(msg *core.Message) {
	if p == nil || p.handler == nil {
		return
	}
	p.handler(p.selfPlatform(), msg)
}

func (p *progressPlatform) ProgressStyle() string {
	switch strings.ToLower(strings.TrimSpace(p.progressStyle)) {
	case "", "legacy":
		return "legacy"
	case "compact":
		return "compact"
	case "card":
		return "card"
	default:
		return "legacy"
	}
}

func (p *progressPlatform) SupportsProgressCardPayload() bool {
	return p.ProgressStyle() == "card"
}

func (p *Platform) isGroupReplyAllGuild(guildID string) bool {
	for _, g := range p.groupReplyAllGuilds {
		if g == "*" || g == guildID {
			return true
		}
	}
	return false
}

const maxParticipatedThreads = 500

type participatedThreadSet struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
}

func participatedThreadStorePath(opts map[string]any) string {
	dataDir, _ := opts["cc_data_dir"].(string)
	project, _ := opts["cc_project"].(string)
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	name := strings.TrimSpace(project)
	if name == "" {
		name = "default"
	}
	return filepath.Join(dataDir, "discord_participated_threads_"+name+".json")
}

func autoThreadPlaceholderStorePath(opts map[string]any) string {
	dataDir, _ := opts["cc_data_dir"].(string)
	project, _ := opts["cc_project"].(string)
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	name := strings.TrimSpace(project)
	if name == "" {
		name = "default"
	}
	return filepath.Join(dataDir, "discord_auto_thread_placeholders_"+name+".json")
}

func (p *Platform) loadParticipatedThreads() {
	if p == nil {
		return
	}
	p.participatedThreads.ids = make(map[string]struct{})
	if p.threadStorePath == "" {
		return
	}
	data, err := os.ReadFile(p.threadStorePath)
	if err != nil {
		return
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		slog.Debug("discord: load participated threads failed", "path", p.threadStorePath, "error", err)
		return
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := p.participatedThreads.ids[id]; ok {
			continue
		}
		p.participatedThreads.ids[id] = struct{}{}
		p.participatedThreads.order = append(p.participatedThreads.order, id)
	}
	p.trimParticipatedThreadsLocked()
}

func (p *Platform) trimParticipatedThreadsLocked() {
	for len(p.participatedThreads.order) > maxParticipatedThreads {
		old := p.participatedThreads.order[0]
		p.participatedThreads.order = p.participatedThreads.order[1:]
		delete(p.participatedThreads.ids, old)
	}
}

func (p *Platform) markParticipatedThread(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if p == nil || threadID == "" {
		return
	}
	p.participatedThreads.mu.Lock()
	if p.participatedThreads.ids == nil {
		p.participatedThreads.ids = make(map[string]struct{})
	}
	if _, ok := p.participatedThreads.ids[threadID]; ok {
		p.participatedThreads.mu.Unlock()
		return
	}
	p.participatedThreads.ids[threadID] = struct{}{}
	p.participatedThreads.order = append(p.participatedThreads.order, threadID)
	p.trimParticipatedThreadsLocked()
	snapshot := append([]string(nil), p.participatedThreads.order...)
	path := p.threadStorePath
	p.participatedThreads.mu.Unlock()
	p.persistParticipatedThreads(path, snapshot)
}

func (p *Platform) persistParticipatedThreads(path string, ids []string) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Debug("discord: persist participated threads mkdir failed", "path", path, "error", err)
		return
	}
	data, err := json.Marshal(ids)
	if err != nil {
		slog.Debug("discord: persist participated threads marshal failed", "error", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Debug("discord: persist participated threads write failed", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		slog.Debug("discord: persist participated threads rename failed", "path", path, "error", err)
	}
}

func (p *Platform) hasParticipatedInThread(threadID string) bool {
	if p == nil || threadID == "" {
		return false
	}
	p.participatedThreads.mu.Lock()
	defer p.participatedThreads.mu.Unlock()
	_, ok := p.participatedThreads.ids[threadID]
	return ok
}

func (p *Platform) rememberAutoThreadPlaceholder(threadID, name string) {
	threadID = strings.TrimSpace(threadID)
	name = strings.TrimSpace(name)
	if p == nil || threadID == "" || name == "" {
		return
	}
	p.autoThreadPlaceholders.Store(threadID, name)
	p.persistAutoThreadPlaceholders()
}

func (p *Platform) autoThreadPlaceholder(threadID string) string {
	if p == nil || threadID == "" {
		return ""
	}
	v, ok := p.autoThreadPlaceholders.Load(threadID)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func (p *Platform) forgetAutoThreadPlaceholder(threadID string) {
	if p == nil || threadID == "" {
		return
	}
	p.autoThreadPlaceholders.Delete(threadID)
	p.persistAutoThreadPlaceholders()
}

func (p *Platform) loadAutoThreadPlaceholders() {
	if p == nil || p.placeholderStorePath == "" {
		return
	}
	data, err := os.ReadFile(p.placeholderStorePath)
	if err != nil {
		return
	}
	var names map[string]string
	if err := json.Unmarshal(data, &names); err != nil {
		slog.Debug("discord: load auto-thread placeholders failed", "path", p.placeholderStorePath, "error", err)
		return
	}
	for id, name := range names {
		id = strings.TrimSpace(id)
		name = strings.TrimSpace(name)
		if id == "" || name == "" {
			continue
		}
		p.autoThreadPlaceholders.Store(id, name)
	}
}

func (p *Platform) persistAutoThreadPlaceholders() {
	if p == nil || p.placeholderStorePath == "" {
		return
	}
	names := make(map[string]string)
	p.autoThreadPlaceholders.Range(func(key, value any) bool {
		id, _ := key.(string)
		name, _ := value.(string)
		id = strings.TrimSpace(id)
		name = strings.TrimSpace(name)
		if id != "" && name != "" {
			names[id] = name
		}
		return true
	})
	data, err := json.Marshal(names)
	if err != nil {
		slog.Debug("discord: persist auto-thread placeholders marshal failed", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.placeholderStorePath), 0o755); err != nil {
		slog.Debug("discord: persist auto-thread placeholders mkdir failed", "path", p.placeholderStorePath, "error", err)
		return
	}
	tmp := p.placeholderStorePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Debug("discord: persist auto-thread placeholders write failed", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, p.placeholderStorePath); err != nil {
		_ = os.Remove(tmp)
		slog.Debug("discord: persist auto-thread placeholders rename failed", "path", p.placeholderStorePath, "error", err)
	}
}

func (p *Platform) attachAutoThreadPlaceholder(rc replyContext) replyContext {
	if rc.autoThreadInitialName != "" {
		if rc.threadID != "" {
			p.rememberAutoThreadPlaceholder(rc.threadID, rc.autoThreadInitialName)
		}
		return rc
	}
	threadID := rc.threadID
	if threadID == "" {
		threadID = rc.channelID
	}
	if name := p.autoThreadPlaceholder(threadID); name != "" {
		rc.autoThreadInitialName = name
	}
	return rc
}

// guildMessageSkipsMention is true when a guild message should be admitted
// without @mention: group_reply_all, or a thread this bot already joined
// (Hermes-style in-thread shortcut). thread_require_mention disables the
// shortcut so multi-bot threads stay mention-gated.
//
// Participation is keyed by Discord channel ID. Parent text channels are
// never stored, so a set lookup is enough — no REST Channel GET on the
// unmentioned hot path.
func (p *Platform) guildMessageSkipsMention(guildID, channelID string, _ discordThreadOps) bool {
	if p.isGroupReplyAllGuild(guildID) {
		return true
	}
	if p.threadRequireMention || channelID == "" {
		return false
	}
	return p.hasParticipatedInThread(channelID)
}

func (p *Platform) recordThreadParticipation(channelID, threadID string, ops discordThreadOps) {
	if threadID != "" {
		p.markParticipatedThread(threadID)
		return
	}
	if channelID == "" || ops == nil {
		return
	}
	ch, err := ops.ResolveChannel(channelID)
	if err != nil || ch == nil || !isThreadChannelType(ch.Type) {
		return
	}
	p.markParticipatedThread(ch.ID)
}

// recordKnownThreadParticipation marks a thread without hitting Discord REST.
// Outbound send paths use this so unit tests that stub ChannelMessageSend
// are not forced to also stub Channel GET. Inbound handlers still resolve.
func (p *Platform) recordKnownThreadParticipation(channelID, threadID string) {
	if threadID != "" {
		p.markParticipatedThread(threadID)
		return
	}
	if p == nil || p.session == nil || channelID == "" {
		return
	}
	ch, err := p.session.State.Channel(channelID)
	if err != nil || ch == nil || !isThreadChannelType(ch.Type) {
		return
	}
	p.markParticipatedThread(ch.ID)
}

func (p *Platform) makeSessionKey(channelID string, userID string) string {
	return buildSessionKey(channelID, userID, p.shareSessionInChannel)
}

func rememberDedupID(store *sync.Map, id string) bool {
	if id == "" {
		return true
	}
	if _, loaded := store.LoadOrStore(id, struct{}{}); loaded {
		return false
	}
	time.AfterFunc(2*time.Minute, func() { store.Delete(id) })
	return true
}

func buildSessionKey(channelID string, userID string, shareSessionInChannel bool) string {
	if shareSessionInChannel {
		return fmt.Sprintf("discord:%s", channelID)
	}
	return fmt.Sprintf("discord:%s:%s", channelID, userID)
}

// TODO: thread_isolation currently keys each Discord thread as one shared session, so share_session_in_channel=false does not further isolate users within the same thread.
func buildThreadSessionKey(threadID string) string {
	return fmt.Sprintf("discord:%s", threadID)
}

func parseDiscordReplyToMode(v any) (string, error) {
	s, _ := v.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "first":
		return "first", nil
	case "off", "all":
		return s, nil
	default:
		return "", fmt.Errorf("discord: invalid reply_to_mode %q (want off, first, or all)", s)
	}
}

func newReplyContext(channelID, messageID, threadID, sourceChannelID string) replyContext {
	return replyContext{
		channelID:       channelID,
		messageID:       messageID,
		threadID:        threadID,
		sourceChannelID: sourceChannelID,
	}
}

func (rc replyContext) targetChannelID() string {
	if rc.threadID != "" {
		return rc.threadID
	}
	return rc.channelID
}

func (rc replyContext) reactionChannelID() string {
	if rc.sourceChannelID != "" {
		return rc.sourceChannelID
	}
	return rc.targetChannelID()
}

func (rc replyContext) messageReference() *discordgo.MessageReference {
	if rc.messageID == "" {
		return nil
	}
	failIfNotExists := false
	return &discordgo.MessageReference{
		MessageID:       rc.messageID,
		ChannelID:       rc.targetChannelID(),
		FailIfNotExists: &failIfNotExists,
	}
}

func (rc replyContext) referenceForChunk(mode string, index int) *discordgo.MessageReference {
	switch mode {
	case "all":
		return rc.messageReference()
	case "first":
		if index == 0 {
			return rc.messageReference()
		}
		return nil
	default:
		return nil
	}
}

func isDiscordReplyReferenceRejected(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	if strings.Contains(text, "10008") {
		return true
	}
	if strings.Contains(text, "REPLIES_CANNOT_REFERENCE_OTHER_CHANNEL") {
		return true
	}
	lower := strings.ToLower(text)
	return strings.Contains(text, "50035") && (strings.Contains(lower, "system message") || strings.Contains(lower, "cannot reply to a message in a different channel"))
}

type discordThreadOps interface {
	ResolveChannel(channelID string) (*discordgo.Channel, error)
	StartThread(channelID, messageID, name string, archiveDuration int) (*discordgo.Channel, error)
	StartStandaloneThread(channelID, name string, typ discordgo.ChannelType, archiveDuration int) (*discordgo.Channel, error)
	JoinThread(threadID string) error
}

type sessionThreadOps struct {
	session *discordgo.Session
}

func (o sessionThreadOps) ResolveChannel(channelID string) (*discordgo.Channel, error) {
	if o.session == nil {
		return nil, fmt.Errorf("discord: session not initialized")
	}
	if ch, err := o.session.State.Channel(channelID); err == nil && ch != nil {
		return ch, nil
	}
	return o.session.Channel(channelID)
}

func (o sessionThreadOps) StartThread(channelID, messageID, name string, archiveDuration int) (*discordgo.Channel, error) {
	if o.session == nil {
		return nil, fmt.Errorf("discord: session not initialized")
	}
	return o.session.MessageThreadStart(channelID, messageID, name, archiveDuration)
}

func (o sessionThreadOps) StartStandaloneThread(channelID, name string, typ discordgo.ChannelType, archiveDuration int) (*discordgo.Channel, error) {
	if o.session == nil {
		return nil, fmt.Errorf("discord: session not initialized")
	}
	return o.session.ThreadStart(channelID, name, typ, archiveDuration)
}

func (o sessionThreadOps) JoinThread(threadID string) error {
	if o.session == nil {
		return fmt.Errorf("discord: session not initialized")
	}
	return o.session.ThreadJoin(threadID)
}

func isThreadChannelType(t discordgo.ChannelType) bool {
	switch t {
	case discordgo.ChannelTypeGuildNewsThread,
		discordgo.ChannelTypeGuildPublicThread,
		discordgo.ChannelTypeGuildPrivateThread:
		return true
	default:
		return false
	}
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func prefixWithinUTF16Limit(s string, limit int) string {
	if utf16Len(s) <= limit {
		return s
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if utf16Len(string(runes[:mid])) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}

func sanitizeDiscordThreadTitle(title string) string {
	cleaned := strings.Join(strings.Fields(strings.ReplaceAll(title, "\n", " ")), " ")
	if cleaned == "" {
		return "cc session"
	}
	if utf16Len(cleaned) <= 80 {
		return cleaned
	}
	return strings.TrimRight(prefixWithinUTF16Limit(cleaned, 77), " ") + "..."
}

var discordMentionToken = regexp.MustCompile(`<@!?[0-9]+>|<@&[0-9]+>|<#\d+>`)

func threadNameForMessage(m *discordgo.MessageCreate, botID string) string {
	name := stripDiscordMention(m.Content, botID)
	name = discordMentionToken.ReplaceAllString(name, "")
	name = strings.Join(strings.Fields(strings.ReplaceAll(name, "\n", " ")), " ")
	if name == "" && m.Author != nil {
		name = "cc " + m.Author.Username
	}
	return sanitizeDiscordThreadTitle(name)
}

func freshThreadName(title string) string {
	if strings.TrimSpace(title) == "" {
		return "cc cron"
	}
	return sanitizeDiscordThreadTitle(title)
}

func standaloneThreadType(parentType discordgo.ChannelType) (discordgo.ChannelType, bool) {
	switch parentType {
	case discordgo.ChannelTypeGuildText:
		return discordgo.ChannelTypeGuildPublicThread, true
	case discordgo.ChannelTypeGuildNews:
		return discordgo.ChannelTypeGuildNewsThread, true
	default:
		return 0, false
	}
}

func parseDiscordSessionKeyChannelID(sessionKey string) (string, error) {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != "discord" || parts[1] == "" {
		return "", fmt.Errorf("discord: invalid session key %q", sessionKey)
	}
	return parts[1], nil
}

func resolveSessionKeyForChannel(channelID, userID string, shareSessionInChannel bool, threadIsolation bool, ops discordThreadOps) string {
	if !threadIsolation {
		return buildSessionKey(channelID, userID, shareSessionInChannel)
	}
	ch, err := ops.ResolveChannel(channelID)
	if err != nil {
		slog.Warn("discord: resolve channel for session key failed, falling back", "channel", channelID, "error", err)
		return buildSessionKey(channelID, userID, shareSessionInChannel)
	}
	if isThreadChannelType(ch.Type) {
		return buildThreadSessionKey(channelID)
	}
	return buildSessionKey(channelID, userID, shareSessionInChannel)
}

// resolveParentChannelID returns the ID of the parent guild channel a message
// or interaction belongs to, looking through threads. Returns channelID
// unchanged when it isn't a thread or when ParentID is unavailable.
//
// This is the workspace-binding identity for thread-isolation mode: we want
// `<base_dir>/<parent-channel-name>` to drive auto-bind, not `<thread-name>`.
func resolveParentChannelID(channelID string, ops discordThreadOps) string {
	ch, err := ops.ResolveChannel(channelID)
	if err != nil {
		slog.Debug("discord: resolve channel for parent lookup failed", "channel", channelID, "error", err)
		return channelID
	}
	if isThreadChannelType(ch.Type) && ch.ParentID != "" {
		return ch.ParentID
	}
	return channelID
}

// resolveThreadReplyContext routes a guild message into a Discord thread for
// thread_isolation mode and returns the per-thread session key, the reply
// context, and the parent channel ID.
//
// parentChannelID is the channel the thread lives under (or, for messages
// posted directly into an existing thread, the thread's ParentID). It is
// distinct from the thread itself: it's what callers should stamp onto
// Message.ChannelKey so multi-workspace auto-bind keys by channel name
// rather than thread name. Without this distinction, threads break the
// "channel name → workspace folder" convention because Discord threads
// have their own names that rarely match a workspace directory.
func resolveThreadReplyContext(m *discordgo.MessageCreate, botID string, ops discordThreadOps) (string, replyContext, string, error) {
	ch, err := ops.ResolveChannel(m.ChannelID)
	if err != nil {
		return "", replyContext{}, "", fmt.Errorf("resolve channel %s: %w", m.ChannelID, err)
	}
	if isThreadChannelType(ch.Type) {
		// Message posted directly inside an existing thread. The parent
		// channel comes from ch.ParentID; fall back to m.ChannelID only
		// if Discord didn't populate it (defensive — discordgo always
		// sets ParentID for thread channels).
		parentChannelID := ch.ParentID
		if parentChannelID == "" {
			parentChannelID = m.ChannelID
		}
		if err := ops.JoinThread(m.ChannelID); err != nil {
			slog.Debug("discord: join existing thread failed", "thread", m.ChannelID, "error", err)
		}
		rc := newReplyContext(m.ChannelID, m.ID, m.ChannelID, m.ChannelID)
		return buildThreadSessionKey(m.ChannelID), rc, parentChannelID, nil
	}
	if m.Message != nil && m.Message.Thread != nil && m.Message.Thread.ID != "" {
		threadID := m.Message.Thread.ID
		if err := ops.JoinThread(threadID); err != nil {
			slog.Debug("discord: join attached thread failed", "thread", threadID, "error", err)
		}
		rc := newReplyContext(threadID, m.ID, threadID, m.ChannelID)
		return buildThreadSessionKey(threadID), rc, m.ChannelID, nil
	}
	if m.Flags&discordgo.MessageFlagsHasThread != 0 {
		threadID := m.ID
		if err := ops.JoinThread(threadID); err != nil {
			slog.Debug("discord: join message thread failed", "thread", threadID, "error", err)
		}
		rc := newReplyContext(threadID, m.ID, threadID, m.ChannelID)
		return buildThreadSessionKey(threadID), rc, m.ChannelID, nil
	}

	threadName := threadNameForMessage(m, botID)
	thread, err := ops.StartThread(m.ChannelID, m.ID, threadName, 1440)
	if err != nil {
		return "", replyContext{}, "", fmt.Errorf("start thread for message %s: %w", m.ID, err)
	}
	if err := ops.JoinThread(thread.ID); err != nil {
		slog.Debug("discord: join new thread failed", "thread", thread.ID, "error", err)
	}
	rc := newReplyContext(thread.ID, m.ID, thread.ID, m.ChannelID)
	rc.autoThreadInitialName = threadName
	return buildThreadSessionKey(thread.ID), rc, m.ChannelID, nil
}

func resolveCronReplyTarget(sessionKey, title string, ops discordThreadOps) (string, replyContext, error) {
	channelID, err := parseDiscordSessionKeyChannelID(sessionKey)
	if err != nil {
		return "", replyContext{}, err
	}

	ch, err := ops.ResolveChannel(channelID)
	if err != nil {
		return "", replyContext{}, fmt.Errorf("resolve channel %s: %w", channelID, err)
	}

	// Already a thread/sub-post: deliver back there. Opening a sibling
	// thread under the parent posts a "thread created" system message in
	// the parent channel and leaves the original sub-post silent.
	if isThreadChannelType(ch.Type) {
		if err := ops.JoinThread(channelID); err != nil {
			slog.Debug("discord: join existing cron thread failed", "thread", channelID, "error", err)
		}
		rc := replyContext{channelID: channelID, threadID: channelID}
		return sessionKey, rc, nil
	}

	threadType, ok := standaloneThreadType(ch.Type)
	if !ok {
		return "", replyContext{}, core.ErrNotSupported
	}

	thread, err := ops.StartStandaloneThread(channelID, freshThreadName(title), threadType, 1440)
	if err != nil {
		return "", replyContext{}, fmt.Errorf("start thread in channel %s: %w", channelID, err)
	}
	if err := ops.JoinThread(thread.ID); err != nil {
		slog.Debug("discord: join fresh thread failed", "thread", thread.ID, "error", err)
	}

	rc := replyContext{channelID: thread.ID, threadID: thread.ID}
	return buildThreadSessionKey(thread.ID), rc, nil
}

// resolveForkReplyTarget opens a sibling Discord thread for /fork.
// Unlike cron (which stays in an existing thread), /fork always creates a
// new standalone thread under the parent guild channel so the original
// conversation is left untouched. DMs, forums, and other non-threadable
// parents return ErrNotSupported so the engine can fall back to --here.
func resolveForkReplyTarget(sessionKey, title string, origin replyContext, ops discordThreadOps) (string, replyContext, error) {
	channelID, err := parseDiscordSessionKeyChannelID(sessionKey)
	if err != nil {
		if origin.threadID != "" {
			channelID = origin.threadID
		} else if origin.channelID != "" {
			channelID = origin.channelID
		} else {
			return "", replyContext{}, err
		}
	}

	ch, err := ops.ResolveChannel(channelID)
	if err != nil {
		return "", replyContext{}, fmt.Errorf("resolve channel %s: %w", channelID, err)
	}

	parentID := channelID
	parentType := ch.Type
	if isThreadChannelType(ch.Type) {
		if ch.ParentID == "" {
			return "", replyContext{}, core.ErrNotSupported
		}
		parent, err := ops.ResolveChannel(ch.ParentID)
		if err != nil {
			return "", replyContext{}, fmt.Errorf("resolve parent %s: %w", ch.ParentID, err)
		}
		parentID = parent.ID
		parentType = parent.Type
	}

	threadType, ok := standaloneThreadType(parentType)
	if !ok {
		return "", replyContext{}, core.ErrNotSupported
	}

	thread, err := ops.StartStandaloneThread(parentID, freshThreadName(title), threadType, 1440)
	if err != nil {
		return "", replyContext{}, fmt.Errorf("start fork thread in channel %s: %w", parentID, err)
	}
	if err := ops.JoinThread(thread.ID); err != nil {
		slog.Debug("discord: join fork thread failed", "thread", thread.ID, "error", err)
	}

	rc := replyContext{channelID: thread.ID, threadID: thread.ID}
	return buildThreadSessionKey(thread.ID), rc, nil
}

// isDiscordConversationalMessageType matches Hermes Discord admission:
// only default and reply messages become model turns. Channel/thread
// renames (type 4), pins, joins, and "thread created" system messages
// must not wake the agent.
func isDiscordConversationalMessageType(t discordgo.MessageType) bool {
	switch t {
	case discordgo.MessageTypeDefault, discordgo.MessageTypeReply:
		return true
	default:
		return false
	}
}

// RegisterCommands registers bot commands with Discord for the slash command menu.
func (p *Platform) RegisterCommands(commands []core.BotCommandInfo) error {
	// Wait for Ready event to ensure appID is populated
	select {
	case <-p.readyCh:
	case <-time.After(15 * time.Second):
		return fmt.Errorf("discord: timed out waiting for Ready event")
	}

	var cmds []*discordgo.ApplicationCommand
	for _, c := range commands {
		if len(c.Command) > 32 {
			slog.Warn("discord: command name > 32 skip " + c.Command)
			continue
		}
		desc := c.Description
		if runes := []rune(desc); len(runes) > 100 {
			desc = string(runes[:97]) + "..."
		}
		cmds = append(cmds, &discordgo.ApplicationCommand{
			Name:        c.Command,
			Description: desc,
			// A trick to be able to input any args
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Description: "optional args",
					Name:        "args",
					Required:    false,
				},
			},
		})
	}

	// Discord allows max 100 commands per bulk overwrite (guild or global).
	if len(cmds) > 100 {
		slog.Warn("discord: truncating commands to Discord limit of 100", "total", len(cmds), "dropped", len(cmds)-100)
		cmds = cmds[:100]
	}

	if len(cmds) == 0 {
		slog.Debug("discord: no commands to register")
		return nil
	}

	registered, err := p.session.ApplicationCommandBulkOverwrite(p.appID, p.guildID, cmds)
	if err != nil {
		slog.Error("discord: failed to register slash commands — "+
			"make sure the bot was invited with BOTH 'bot' AND 'applications.commands' OAuth2 scopes. "+
			"Re-invite URL: https://discord.com/oauth2/authorize?client_id="+p.appID+
			"&scope=bot+applications.commands&permissions=2147485696",
			"error", err, "guild_id", p.guildID)
		return err
	}
	scope := "global (may take up to 1h to appear — set guild_id for instant)"
	if p.guildID != "" {
		scope = "guild:" + p.guildID
	}
	slog.Info("discord: registered slash commands", "count", len(registered), "scope", scope)

	return nil
}

// SetLifecycleHandler registers a handler that will be notified when the
// Discord platform becomes ready or unavailable. Implementing this method
// also opts the platform into Engine.Start's AsyncRecoverablePlatform path,
// so a transient gateway error at startup no longer aborts the platform.
func (p *Platform) SetLifecycleHandler(h core.PlatformLifecycleHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lifecycleHandler = h
}

// Start launches the gateway connection in the background. It returns nil as
// soon as the recovery loop is running; the caller should treat
// OnPlatformReady as the signal that the platform is actually usable.
//
// Before this change Start returned the first session.Open() error directly,
// which meant a transient proxy/network blip during cc-connect startup
// permanently took Discord offline until manual restart (release-gate
// 2026-06-14: "discord: open gateway: ... EOF" with no retry).
func (p *Platform) Start(handler core.MessageHandler) error {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return fmt.Errorf("discord: platform stopped")
	}
	p.handler = handler
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()

	go p.connectLoop(ctx)
	return nil
}

// buildSession creates a fresh discordgo session with proxy + handlers wired up.
// It is called once per connect attempt so a session that failed mid-handshake
// is fully discarded before the next retry.
func (p *Platform) buildSession() (*discordgo.Session, error) {
	session, err := discordgo.New("Bot " + p.token)
	if err != nil {
		return nil, fmt.Errorf("discord: create session: %w", err)
	}
	if p.proxyURL != nil {
		transport := &http.Transport{Proxy: http.ProxyURL(p.proxyURL)}
		session.Client = &http.Client{Transport: transport, Timeout: 60 * time.Second}
		session.Dialer = &websocket.Dialer{Proxy: http.ProxyURL(p.proxyURL)}
		slog.Info("discord: using proxy", "proxy", p.proxyURL.Host)
	}

	session.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent

	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		p.botID = r.User.ID
		p.appID = r.User.ID
		slog.Info("discord: connected", "bot", r.User.Username+"#"+r.User.Discriminator)
		// Signal readiness before guild role lookups so RegisterCommands
		// is not blocked by slow API calls when there are many guilds.
		select {
		case <-p.readyCh:
		default:
			close(p.readyCh)
		}
		for _, g := range r.Guilds {
			if g == nil || g.ID == "" || g.Unavailable {
				continue
			}
			p.cacheBotRoleIDForGuild(s, g.ID, g.Roles)
		}
	})

	session.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
		if g == nil || g.Guild == nil || g.ID == "" || g.Unavailable {
			return
		}
		p.cacheBotRoleIDForGuild(s, g.ID, g.Roles)
	})

	session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		// Deduplicate: Discord gateway may deliver the same event twice
		if !rememberDedupID(&p.seenMsgs, m.ID) {
			slog.Debug("discord: ignoring duplicate message", "msg_id", m.ID)
			return
		}

		if m.Author.Bot || m.Author.ID == p.botID {
			if m.Author.ID == p.botID && m.GuildID != "" {
				p.recordThreadParticipation(m.ChannelID, "", sessionThreadOps{session: p.session})
			}
			return
		}
		if !isDiscordConversationalMessageType(m.Type) {
			slog.Debug("discord: ignoring non-conversational message", "msg_id", m.ID, "type", m.Type)
			return
		}
		if core.IsOldMessage(m.Timestamp) {
			slog.Debug("discord: ignoring old message after restart", "timestamp", m.Timestamp)
			return
		}
		if !core.AllowList(p.allowFrom, m.Author.ID) {
			slog.Debug("discord: message from unauthorized user", "user", m.Author.ID)
			return
		}

		// In guild channels, only respond when the bot is @mentioned (unless
		// group_reply_all, or this is a thread the bot already participated in).
		// Check both user mentions and role mentions (Discord auto-creates a
		// managed role for each bot; users may @ the role instead of the user).
		botRoleID := p.botRoleIDForGuild(m.GuildID)
		if botRoleID == "" && m.GuildID != "" {
			p.cacheBotRoleIDForGuild(s, m.GuildID, nil)
			botRoleID = p.botRoleIDForGuild(m.GuildID)
		}
		threadOps := sessionThreadOps{session: p.session}
		mentioned := isDiscordBotMention(m, p.botID, botRoleID, p.respondToAtEveryoneAndHere)
		if m.GuildID != "" && !mentioned && !p.guildMessageSkipsMention(m.GuildID, m.ChannelID, threadOps) {
			slog.Debug("discord: ignoring guild message without bot mention", "channel", m.ChannelID)
			return
		}
		if mentioned {
			m.Content = stripDiscordMentionWithRole(m.Content, p.botID, botRoleID)
			if m.MentionEveryone {
				m.Content = stripEveryoneHere(m.Content)
			}
		}

		slog.Debug("discord: message received", "user", m.Author.Username, "channel", m.ChannelID)

		sessionKey := p.makeSessionKey(m.ChannelID, m.Author.ID)
		rctx := newReplyContext(m.ChannelID, m.ID, "", m.ChannelID)
		// channelKey pins workspace binding to the parent channel even when
		// thread_isolation rewrites SessionKey to a thread ID. Without it,
		// effectiveChannelID() would extract the thread ID from SessionKey
		// and multi-workspace auto-bind would try to match `<base_dir>/<thread-name>`,
		// which never exists. Empty value falls back to SessionKey extraction
		// (the historical, non-isolated behavior).
		channelKey := ""
		if p.threadIsolation && m.GuildID != "" {
			threadSessionKey, threadCtx, parentChannelID, err := resolveThreadReplyContext(m, p.botID, threadOps)
			if err != nil {
				slog.Warn("discord: thread isolation setup failed, falling back", "message", m.ID, "channel", m.ChannelID, "error", err)
			} else {
				sessionKey = threadSessionKey
				rctx = p.attachAutoThreadPlaceholder(threadCtx)
				channelKey = parentChannelID
			}
		}

		images, files, audio := classifyAttachments(m.Attachments, downloadURL)

		if m.Content == "" && len(images) == 0 && len(files) == 0 && audio == nil && m.ReferencedMessage == nil {
			return
		}

		// Prepend the replied-to message's content and images so the agent has context.
		if m.ReferencedMessage != nil {
			m.Content, images = applyReferencedMessage(m.ReferencedMessage, m.Content, images, downloadURL)
		}

		msg := &core.Message{
			SessionKey: sessionKey, ChannelKey: channelKey, Platform: "discord",
			ChannelID: m.ChannelID,
			MessageID: m.ID,
			UserID:    m.Author.ID, UserName: m.Author.Username,
			Content: m.Content, Images: images, Files: files, Audio: audio, ReplyCtx: rctx,
		}
		msg.ChatName, _ = p.ResolveChannelName(m.ChannelID)
		p.recordThreadParticipation(m.ChannelID, rctx.threadID, threadOps)
		p.dispatchMessage(msg)
	})

	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		p.handleInteraction(s, i)
	})

	return session, nil
}

// connectLoop drives the Discord gateway connection with exponential-backoff
// retry. After session.Open() succeeds once, discordgo's own logic keeps the
// gateway alive (heartbeat + automatic reconnect on transient errors), so the
// loop blocks on ctx.Done() and tears the session down on shutdown.
func (p *Platform) connectLoop(ctx context.Context) {
	backoff := discordInitialReconnectBackoff
	attempt := 0

	for {
		if err := ctx.Err(); err != nil || p.isStopping() {
			return
		}

		attempt++
		session, err := p.buildSession()
		if err == nil {
			err = session.Open()
		}

		if err == nil {
			p.mu.Lock()
			p.session = session
			p.everConnected = true
			handler := p.lifecycleHandler
			p.mu.Unlock()

			if attempt > 1 {
				slog.Info("discord: connected after retries", "attempts", attempt)
			}
			if handler != nil {
				handler.OnPlatformReady(p.selfPlatform())
			}

			// Block until shutdown; discordgo manages reconnects internally.
			<-ctx.Done()
			_ = session.Close()
			return
		}

		// Best-effort cleanup of a half-opened session.
		if session != nil {
			_ = session.Close()
		}

		slog.Warn("discord: connect attempt failed",
			"attempt", attempt,
			"backoff", backoff,
			"error", err,
		)
		p.mu.RLock()
		handler := p.lifecycleHandler
		p.mu.RUnlock()
		if handler != nil {
			handler.OnPlatformUnavailable(p.selfPlatform(), err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if backoff < discordMaxReconnectBackoff {
			backoff *= 2
			if backoff > discordMaxReconnectBackoff {
				backoff = discordMaxReconnectBackoff
			}
		}
	}
}

func (p *Platform) isStopping() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stopping
}

// handleInteraction processes incoming Discord command and button interactions.
func (p *Platform) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !rememberDedupID(&p.seenInteractions, i.ID) {
		slog.Debug("discord: ignoring duplicate interaction", "interaction_id", i.ID, "type", i.Type)
		return
	}

	userID, userName := "", ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
		userName = i.Member.User.Username
	} else if i.User != nil {
		userID = i.User.ID
		userName = i.User.Username
	}

	if !core.AllowList(p.allowFrom, userID) {
		slog.Debug("discord: interaction from unauthorized user", "user", userID)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "You are not authorized to use this bot.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	switch i.Type {
	case discordgo.InteractionMessageComponent:
		p.handleComponentInteraction(s, i, userID, userName)
		return
	case discordgo.InteractionApplicationCommand:
	default:
		return
	}

	var rctx any
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		// Defer must usually happen within ~3s; if it fails (e.g. "Unknown interaction"),
		// aborting here drops the command entirely (#258). Fall back to normal channel
		// messages — sendInteraction already falls back similarly on edit failures.
		slog.Warn("discord: defer interaction failed, continuing with channel replies", "error", err)
		channelID := i.ChannelID
		var rc replyContext
		if ch, chErr := s.Channel(channelID); chErr != nil {
			slog.Debug("discord: channel lookup for slash fallback failed", "channel", channelID, "error", chErr)
			rc = replyContext{channelID: channelID}
		} else {
			rc = replyContextForDeferredInteractionFallback(ch, channelID)
		}
		rctx = rc
	} else {
		rctx = &interactionReplyCtx{
			interaction: i.Interaction,
			channelID:   i.ChannelID,
		}
	}

	data := i.ApplicationCommandData()
	cmdText := reconstructCommand(data)
	channelID := i.ChannelID

	slog.Debug("discord: slash command", "user", userName, "command", cmdText, "channel", channelID)

	ops := sessionThreadOps{session: p.session}
	sessionKey := resolveSessionKeyForChannel(channelID, userID, p.shareSessionInChannel, p.threadIsolation, ops)
	channelKey := ""
	if p.threadIsolation {
		channelKey = resolveParentChannelID(channelID, ops)
	}

	msg := &core.Message{
		SessionKey: sessionKey, ChannelKey: channelKey, Platform: "discord",
		MessageID: i.ID,
		ChannelID: i.ChannelID,
		UserID:    userID, UserName: userName,
		Content: cmdText, ReplyCtx: rctx,
	}
	msg.ChatName, _ = p.ResolveChannelName(channelID)
	p.recordThreadParticipation(channelID, "", ops)
	p.dispatchMessage(msg)
}

// replyContextForDeferredInteractionFallback builds a replyContext for slash commands
// when InteractionRespond(defer) failed. Thread channels must set threadID so
// sendChannelReply uses ChannelMessageSend instead of ChannelMessageSendReply with an empty ref.
func replyContextForDeferredInteractionFallback(ch *discordgo.Channel, channelID string) replyContext {
	if ch == nil {
		return replyContext{channelID: channelID}
	}
	if isThreadChannelType(ch.Type) {
		return replyContext{channelID: channelID, threadID: channelID}
	}
	return replyContext{channelID: channelID}
}

// reconstructCommand converts a Discord interaction back to a text command string
// (e.g. "/config thinking_max_len 200") that the engine can parse.
func reconstructCommand(data discordgo.ApplicationCommandInteractionData) string {
	name := data.Name
	var parts []string
	parts = append(parts, "/"+name)
	for _, opt := range data.Options {
		switch opt.Type {
		case discordgo.ApplicationCommandOptionInteger:
			parts = append(parts, fmt.Sprintf("%d", opt.IntValue()))
		default:
			parts = append(parts, opt.StringValue())
		}
	}
	return strings.Join(parts, " ")
}

func (p *Platform) handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, userID, userName string) {
	data := i.MessageComponentData()
	command, isPermissionResponse, ok := discordComponentCommand(data.CustomID)
	if !ok {
		slog.Debug("discord: unknown component interaction", "custom_id", data.CustomID)
		return
	}

	origText := ""
	if i.Message != nil {
		origText = i.Message.Content
	}
	emptyComponents := []discordgo.MessageComponent{}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    origText + "\n\n> " + command,
			Components: emptyComponents,
		},
	}); err != nil {
		slog.Debug("discord: command component update failed", "error", err)
	}

	channelID := i.ChannelID
	ops := sessionThreadOps{session: p.session}
	sessionKey := resolveSessionKeyForChannel(channelID, userID, p.shareSessionInChannel, p.threadIsolation, ops)
	channelKey := ""
	if p.threadIsolation {
		channelKey = resolveParentChannelID(channelID, ops)
	}
	rc := replyContext{channelID: channelID}
	if i.Message != nil {
		rc.messageID = i.Message.ID
	}
	chatName, _ := p.ResolveChannelName(channelID)
	p.recordThreadParticipation(channelID, "", ops)
	p.dispatchMessage(&core.Message{
		SessionKey:           sessionKey,
		ChannelKey:           channelKey,
		Platform:             "discord",
		MessageID:            i.ID,
		UserID:               userID,
		UserName:             userName,
		Content:              command,
		ChatName:             chatName,
		ReplyCtx:             rc,
		IsPermissionResponse: isPermissionResponse,
	})
}

func discordComponentCommand(customID string) (command string, isPermissionResponse bool, ok bool) {
	if command, found := strings.CutPrefix(customID, "cmd:"); found && command != "" {
		return command, false, true
	}
	if action, found := strings.CutPrefix(customID, "perm:"); found {
		switch action {
		case "allow", "deny":
			return action, true, true
		case "allow_all":
			return "allow all", true, true
		}
	}
	return "", false, false
}

func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	switch rc := rctx.(type) {
	case *interactionReplyCtx:
		return p.sendInteraction(rc, content)
	case replyContext:
		return p.sendChannelMessages(rc, content)
	default:
		return fmt.Errorf("discord: invalid reply context type %T", rctx)
	}
}

// Send delivers a channel message. Hermes reply_to_mode applies here:
// the engine's final answer uses Send, not Reply. Progress uses
// SendPreviewStart and never carries a reply reference.
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	switch rc := rctx.(type) {
	case *interactionReplyCtx:
		return p.sendInteraction(rc, content)
	case replyContext:
		return p.sendChannelMessages(rc, content)
	default:
		return fmt.Errorf("discord: invalid reply context type %T", rctx)
	}
}

// sendInteraction delivers a message through the Discord interaction response
// mechanism. The first call edits the deferred "thinking" response; subsequent
// calls create followup messages.
func (p *Platform) sendInteraction(ictx *interactionReplyCtx, content string) error {
	if ictx != nil {
		p.recordKnownThreadParticipation(ictx.channelID, "")
	}
	chunks := core.SplitMessageCodeFenceAware(convertTablesToBullets(content), maxDiscordLen)
	for _, chunk := range chunks {
		ictx.mu.Lock()
		first := !ictx.firstDone
		if first {
			ictx.firstDone = true
		}
		ictx.mu.Unlock()

		var err error
		if first {
			c := chunk
			_, err = p.session.InteractionResponseEdit(ictx.interaction, &discordgo.WebhookEdit{Content: &c})
		} else {
			_, err = p.session.FollowupMessageCreate(ictx.interaction, true, &discordgo.WebhookParams{Content: chunk})
		}

		if err != nil {
			slog.Warn("discord: interaction response failed, falling back to channel message", "error", err)
			_, err = p.session.ChannelMessageSend(ictx.channelID, chunk)
			if err != nil {
				return fmt.Errorf("discord: send fallback: %w", err)
			}
		}
	}
	return nil
}

func (p *Platform) effectiveReplyToMode() string {
	switch p.replyToMode {
	case "off", "all", "first":
		return p.replyToMode
	default:
		return "first"
	}
}

func (p *Platform) sendChannelReply(rc replyContext, content string) error {
	return p.sendChannelMessages(rc, content)
}

func (p *Platform) sendChannel(rc replyContext, content string) error {
	return p.sendChannelMessages(rc, content)
}

// sendChannelMessages is Hermes reply_to_mode: the first chunk of this
// Send (and every chunk when mode=all) carries message_reference. Thread
// isolation still replies to the user message; the reference channel is
// the send target (the thread), not the parent that owns the starter.
func (p *Platform) sendChannelMessages(rc replyContext, content string) error {
	p.recordKnownThreadParticipation(rc.targetChannelID(), rc.threadID)
	chunks := core.SplitMessageCodeFenceAware(convertTablesToBullets(content), maxDiscordLen)
	mode := p.effectiveReplyToMode()
	for i, chunk := range chunks {
		ref := rc.referenceForChunk(mode, i)
		if err := p.sendChunk(rc.targetChannelID(), chunk, ref); err != nil {
			return fmt.Errorf("discord: send: %w", err)
		}
	}
	return nil
}

func (p *Platform) sendChunk(channelID, content string, ref *discordgo.MessageReference) error {
	if p.session == nil {
		return fmt.Errorf("discord: session not connected")
	}
	msg := &discordgo.MessageSend{Content: content}
	if ref != nil {
		msg.Reference = ref
	}
	_, err := p.session.ChannelMessageSendComplex(channelID, msg)
	if err != nil && ref != nil && isDiscordReplyReferenceRejected(err) {
		slog.Warn("discord: reply target rejected the reply reference; retrying without it")
		msg.Reference = nil
		_, err = p.session.ChannelMessageSendComplex(channelID, msg)
	}
	return err
}

// SendImage sends an image to the channel or interaction.
// Implements core.ImageSender.
func (p *Platform) SendImage(ctx context.Context, rctx any, img core.ImageAttachment) error {
	name := img.FileName
	if name == "" {
		name = "image.png"
	}

	newFile := func() *discordgo.File {
		return &discordgo.File{
			Name:        name,
			ContentType: img.MimeType,
			Reader:      bytes.NewReader(img.Data),
		}
	}

	switch rc := rctx.(type) {
	case *interactionReplyCtx:
		rc.mu.Lock()
		first := !rc.firstDone
		if first {
			rc.firstDone = true
		}
		rc.mu.Unlock()

		var err error
		if first {
			_, err = p.session.InteractionResponseEdit(rc.interaction, &discordgo.WebhookEdit{
				Files: []*discordgo.File{newFile()},
			})
		} else {
			_, err = p.session.FollowupMessageCreate(rc.interaction, true, &discordgo.WebhookParams{
				Files: []*discordgo.File{newFile()},
			})
		}
		if err != nil {
			slog.Warn("discord: interaction image failed, falling back to channel message", "error", err)
			_, err = p.session.ChannelMessageSendComplex(rc.channelID, &discordgo.MessageSend{
				Files: []*discordgo.File{newFile()},
			})
			if err != nil {
				return fmt.Errorf("discord: send image fallback: %w", err)
			}
		}
		return nil
	case replyContext:
		_, err := p.session.ChannelMessageSendComplex(rc.targetChannelID(), &discordgo.MessageSend{
			Files: []*discordgo.File{newFile()},
		})
		if err != nil {
			return fmt.Errorf("discord: send image: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("discord: SendImage: invalid reply context type %T", rctx)
	}
}

func (p *Platform) SendFile(ctx context.Context, rctx any, file core.FileAttachment) error {
	name := file.FileName
	if name == "" {
		name = "attachment"
	}

	newFile := func() *discordgo.File {
		return &discordgo.File{
			Name:        name,
			ContentType: file.MimeType,
			Reader:      bytes.NewReader(file.Data),
		}
	}

	switch rc := rctx.(type) {
	case *interactionReplyCtx:
		rc.mu.Lock()
		first := !rc.firstDone
		if first {
			rc.firstDone = true
		}
		rc.mu.Unlock()

		var err error
		if first {
			_, err = p.session.InteractionResponseEdit(rc.interaction, &discordgo.WebhookEdit{
				Files: []*discordgo.File{newFile()},
			})
		} else {
			_, err = p.session.FollowupMessageCreate(rc.interaction, true, &discordgo.WebhookParams{
				Files: []*discordgo.File{newFile()},
			})
		}
		if err != nil {
			slog.Warn("discord: interaction file failed, falling back to channel message", "error", err)
			_, err = p.session.ChannelMessageSendComplex(rc.channelID, &discordgo.MessageSend{
				Files: []*discordgo.File{newFile()},
			})
			if err != nil {
				return fmt.Errorf("discord: send file fallback: %w", err)
			}
		}
		return nil
	case replyContext:
		_, err := p.session.ChannelMessageSendComplex(rc.targetChannelID(), &discordgo.MessageSend{
			Files: []*discordgo.File{newFile()},
		})
		if err != nil {
			return fmt.Errorf("discord: send file: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("discord: SendFile: invalid reply context type %T", rctx)
	}
}

func buildDiscordActionRows(rows [][]core.ButtonOption) []discordgo.MessageComponent {
	components := make([]discordgo.MessageComponent, 0, len(rows))
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		buttons := make([]discordgo.MessageComponent, 0, len(row))
		for idx, btn := range row {
			style := discordgo.SecondaryButton
			switch idx {
			case 0:
				style = discordgo.SuccessButton
			case 1:
				style = discordgo.DangerButton
			case 2:
				style = discordgo.PrimaryButton
			}
			buttons = append(buttons, discordgo.Button{
				Label:    btn.Text,
				Style:    style,
				CustomID: btn.Data,
			})
		}
		components = append(components, discordgo.ActionsRow{Components: buttons})
	}
	return components
}

func (p *Platform) SendWithButtons(ctx context.Context, rctx any, content string, buttons [][]core.ButtonOption) error {
	if len(buttons) == 0 {
		return fmt.Errorf("discord: no buttons provided")
	}
	components := buildDiscordActionRows(buttons)
	if len(components) == 0 {
		return fmt.Errorf("discord: no buttons provided")
	}

	switch rc := rctx.(type) {
	case *interactionReplyCtx:
		if err := p.sendInteraction(rc, content); err != nil {
			return err
		}
		_, err := p.session.FollowupMessageCreate(rc.interaction, true, &discordgo.WebhookParams{
			Content:    content,
			Components: components,
		})
		if err != nil {
			return fmt.Errorf("discord: send button followup: %w", err)
		}
		return nil
	case replyContext:
		_, err := p.session.ChannelMessageSendComplex(rc.targetChannelID(), &discordgo.MessageSend{
			Content:    content,
			Components: components,
		})
		if err != nil {
			return fmt.Errorf("discord: send channel buttons: %w", err)
		}
		return nil
	default:
		return core.ErrNotSupported
	}
}

func (p *progressPlatform) ProgressUpdateInterval() time.Duration {
	return 2 * time.Second
}

var _ core.ImageSender = (*Platform)(nil)
var _ core.FileSender = (*Platform)(nil)
var _ core.InlineButtonSender = (*Platform)(nil)
var _ core.TypingIndicator = (*Platform)(nil)
var _ core.TypingIndicatorDone = (*Platform)(nil)
var _ core.TypingIndicatorFail = (*Platform)(nil)
var _ core.ProgressStyleProvider = (*progressPlatform)(nil)
var _ core.ProgressCardPayloadSupport = (*progressPlatform)(nil)
var _ core.ProgressUpdateThrottler = (*progressPlatform)(nil)

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// discord:{channelID}:{userID} or discord:{threadID}
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != "discord" {
		return nil, fmt.Errorf("discord: invalid session key %q", sessionKey)
	}
	rc := replyContext{channelID: parts[1]}
	if len(parts) == 2 {
		rc.threadID = parts[1]
	}
	return rc, nil
}

func (p *Platform) ResolveCronReplyTarget(sessionKey string, title string) (string, any, error) {
	if !p.threadIsolation {
		return "", nil, core.ErrNotSupported
	}
	resolvedSessionKey, rc, err := resolveCronReplyTarget(sessionKey, title, sessionThreadOps{session: p.session})
	if err != nil {
		return "", nil, err
	}
	return resolvedSessionKey, rc, nil
}

func (p *Platform) ResolveForkReplyTarget(sessionKey string, title string, replyCtx any) (string, any, error) {
	if !p.threadIsolation {
		return "", nil, core.ErrNotSupported
	}
	var origin replyContext
	switch rc := replyCtx.(type) {
	case replyContext:
		origin = rc
	case *interactionReplyCtx:
		if rc != nil {
			origin = replyContext{channelID: rc.channelID}
		}
	}
	resolvedSessionKey, rc, err := resolveForkReplyTarget(sessionKey, title, origin, sessionThreadOps{session: p.session})
	if err != nil {
		return "", nil, err
	}
	return resolvedSessionKey, rc, nil
}

// discordPreviewHandle stores the IDs needed to edit or delete a preview message.
type discordPreviewHandle struct {
	channelID string
	messageID string
}

// SendPreviewStart sends a new message and returns a handle for subsequent edits.
// Progress/preview is not a Discord "reply" — Hermes only threads the final Send.
func (p *Platform) SendPreviewStart(ctx context.Context, rctx any, content string) (any, error) {
	var channelID string
	switch rc := rctx.(type) {
	case replyContext:
		channelID = rc.targetChannelID()
	case *interactionReplyCtx:
		channelID = rc.channelID
	default:
		return nil, fmt.Errorf("discord: invalid reply context type %T", rctx)
	}

	msg := buildDiscordPreviewMessage(content)
	sent, err := p.session.ChannelMessageSendComplex(channelID, msg)
	if err != nil {
		return nil, fmt.Errorf("discord: send preview: %w", err)
	}
	return &discordPreviewHandle{channelID: channelID, messageID: sent.ID}, nil
}

// UpdateMessage edits an existing message identified by previewHandle.
func (p *Platform) UpdateMessage(ctx context.Context, previewHandle any, content string) error {
	h, ok := previewHandle.(*discordPreviewHandle)
	if !ok {
		return fmt.Errorf("discord: invalid preview handle type %T", previewHandle)
	}
	_, err := p.session.ChannelMessageEditComplex(buildDiscordPreviewEdit(h.channelID, h.messageID, content))
	if err != nil {
		return fmt.Errorf("discord: edit message: %w", err)
	}
	return nil
}

// DeletePreviewMessage removes the preview message so the final response can
// be sent as a fresh message (avoids notification confusion).
func (p *Platform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	h, ok := previewHandle.(*discordPreviewHandle)
	if !ok {
		return fmt.Errorf("discord: invalid preview handle type %T", previewHandle)
	}
	return p.session.ChannelMessageDelete(h.channelID, h.messageID)
}

const (
	discordAckEmoji  = "👀"
	discordOkEmoji   = "✅"
	discordFailEmoji = "❌"
)

func (p *Platform) addReaction(channelID, messageID, emoji string) {
	if p == nil || p.session == nil || channelID == "" || messageID == "" || emoji == "" {
		return
	}
	if err := p.session.MessageReactionAdd(channelID, messageID, emoji); err != nil {
		slog.Debug("discord: add reaction failed", "emoji", emoji, "error", err)
	}
}

func (p *Platform) removeOwnReaction(channelID, messageID, emoji string) {
	if p == nil || p.session == nil || channelID == "" || messageID == "" || emoji == "" {
		return
	}
	if err := p.session.MessageReactionRemove(channelID, messageID, emoji, "@me"); err != nil {
		slog.Debug("discord: remove reaction failed", "emoji", emoji, "error", err)
	}
}

// StartTyping shows Discord typing and, when reactions are on, 👀 on the
// user's message (Hermes progress light). Stop clears 👀; ✅/❌ are added
// afterwards by AddDoneReaction / AddFailReaction.
func (p *Platform) StartTyping(ctx context.Context, rctx any) (stop func()) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return func() {}
	}
	channelID := rc.targetChannelID()
	if channelID == "" {
		return func() {}
	}
	reactCh := rc.reactionChannelID()
	msgID := rc.messageID
	if p.reactions && msgID != "" {
		p.addReaction(reactCh, msgID, discordAckEmoji)
	}

	if p.session != nil {
		_ = p.session.ChannelTyping(channelID)
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if p.session != nil {
					_ = p.session.ChannelTyping(channelID)
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			if p.reactions && msgID != "" {
				p.removeOwnReaction(reactCh, msgID, discordAckEmoji)
			}
		})
	}
}

func (p *Platform) AddDoneReaction(rctx any) {
	if !p.reactions {
		return
	}
	rc, ok := rctx.(replyContext)
	if !ok || rc.messageID == "" {
		return
	}
	p.addReaction(rc.reactionChannelID(), rc.messageID, discordOkEmoji)
}

func (p *Platform) AddFailReaction(rctx any) {
	if !p.reactions {
		return
	}
	rc, ok := rctx.(replyContext)
	if !ok || rc.messageID == "" {
		return
	}
	p.addReaction(rc.reactionChannelID(), rc.messageID, discordFailEmoji)
}

func (p *Platform) ResolveChannelName(channelID string) (string, error) {
	if cached, ok := p.channelNameCache.Load(channelID); ok {
		return cached.(string), nil
	}
	ch, err := p.session.Channel(channelID)
	if err != nil {
		slog.Debug("discord: resolve channel name failed", "channel", channelID, "error", err)
		return channelID, err
	}
	name := ch.Name
	slog.Debug("discord: resolve channel name", "channel", channelID, "name", ch.Name)
	if name == "" {
		return channelID, nil
	}
	p.channelNameCache.Store(channelID, name)
	return name, nil
}

func (p *Platform) Stop() error {
	p.mu.Lock()
	p.stopping = true
	cancel := p.cancel
	session := p.session
	p.cancel = nil
	p.session = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if session != nil {
		return session.Close()
	}
	return nil
}

// RenameSessionTitle implements core.SessionTitleRenamer. Hermes-style:
// only rename a thread this bot just created, and only while Discord still
// shows the placeholder name (a human rename is left alone).
func (p *Platform) RenameSessionTitle(ctx context.Context, replyCtx any, title string) error {
	rc, ok := replyCtx.(replyContext)
	if !ok {
		return nil
	}
	threadID := rc.threadID
	if threadID == "" {
		threadID = rc.channelID
	}
	placeholder := rc.autoThreadInitialName
	if placeholder == "" {
		placeholder = p.autoThreadPlaceholder(threadID)
	}
	return p.renameAutoThread(threadID, placeholder, title)
}

func shouldRenameAutoThread(currentName, onlyIfCurrentName, cleaned string) bool {
	if onlyIfCurrentName == "" || cleaned == "" || cleaned == onlyIfCurrentName {
		return false
	}
	if currentName != onlyIfCurrentName {
		return false
	}
	return currentName != cleaned
}

func (p *Platform) renameAutoThread(threadID, onlyIfCurrentName, title string) error {
	if onlyIfCurrentName == "" || threadID == "" {
		return nil
	}
	cleaned := sanitizeDiscordThreadTitle(title)
	if cleaned == "" || cleaned == onlyIfCurrentName {
		return nil
	}
	p.mu.RLock()
	session := p.session
	p.mu.RUnlock()
	if session == nil {
		return nil
	}
	ch, err := session.Channel(threadID)
	if err != nil || ch == nil {
		slog.Debug("discord: resolve thread for rename failed", "thread", threadID, "error", err)
		return nil
	}
	if !isThreadChannelType(ch.Type) {
		return nil
	}
	if !shouldRenameAutoThread(ch.Name, onlyIfCurrentName, cleaned) {
		if ch.Name != onlyIfCurrentName {
			slog.Info("discord: skip semantic thread rename; current name is not the placeholder",
				"thread", threadID, "current", ch.Name, "expected", onlyIfCurrentName)
			p.forgetAutoThreadPlaceholder(threadID)
		}
		return nil
	}
	_, err = session.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: cleaned})
	if err != nil {
		slog.Debug("discord: semantic thread rename failed", "thread", threadID, "error", err)
		return nil
	}
	p.forgetAutoThreadPlaceholder(threadID)
	slog.Info("discord: renamed auto-thread", "thread", threadID, "from", onlyIfCurrentName, "to", cleaned)
	return nil
}

// stripDiscordMention removes <@botID> and <@!botID> (nick mention) from text.
func stripDiscordMention(text, botID string) string {
	return stripDiscordMentionWithRole(text, botID, "")
}

func stripDiscordMentionWithRole(text, botID string, botRoleID string) string {
	text = strings.ReplaceAll(text, "<@!"+botID+">", "")
	text = strings.ReplaceAll(text, "<@"+botID+">", "")
	if botRoleID != "" {
		text = strings.ReplaceAll(text, "<@&"+botRoleID+">", "")
	}
	return strings.TrimSpace(text)
}

// stripEveryoneHere removes @everyone and @here from text.
func stripEveryoneHere(text string) string {
	text = strings.ReplaceAll(text, "@everyone", "")
	text = strings.ReplaceAll(text, "@here", "")
	return strings.TrimSpace(text)
}

// isDiscordBotMention checks if the message mentions the bot by user ID or managed role ID.
func isDiscordBotMention(m *discordgo.MessageCreate, botID string, botRoleID string, respondToAtEveryoneAndHere bool) bool {
	if respondToAtEveryoneAndHere && m.MentionEveryone {
		return true
	}
	for _, u := range m.Mentions {
		if u != nil && u.ID == botID {
			return true
		}
	}
	if strings.Contains(m.Content, "<@"+botID+">") || strings.Contains(m.Content, "<@!"+botID+">") {
		return true
	}
	for _, roleID := range m.MentionRoles {
		if roleID == botRoleID && roleID != "" {
			return true
		}
	}
	return botRoleID != "" && strings.Contains(m.Content, "<@&"+botRoleID+">")
}

func (p *Platform) botRoleIDForGuild(guildID string) string {
	if guildID == "" {
		return ""
	}
	v, ok := p.botRoleIDs.Load(guildID)
	if !ok {
		return ""
	}
	roleID, _ := v.(string)
	return roleID
}

func (p *Platform) cacheBotRoleIDForGuild(s *discordgo.Session, guildID string, guildRoles []*discordgo.Role) {
	if s == nil || guildID == "" || p.botID == "" {
		return
	}
	roleID, err := p.resolveBotRoleIDForGuild(s, guildID, guildRoles)
	if err != nil {
		slog.Debug("discord: resolve bot managed role failed", "guild", guildID, "error", err)
		return
	}
	if roleID == "" {
		return
	}
	p.botRoleIDs.Store(guildID, roleID)
}

func (p *Platform) resolveBotRoleIDForGuild(s *discordgo.Session, guildID string, guildRoles []*discordgo.Role) (string, error) {
	member, err := s.GuildMember(guildID, p.botID)
	if err != nil {
		return "", fmt.Errorf("fetch bot member: %w", err)
	}
	if member == nil || len(member.Roles) == 0 {
		return "", nil
	}

	memberRoleSet := make(map[string]struct{}, len(member.Roles))
	for _, roleID := range member.Roles {
		memberRoleSet[roleID] = struct{}{}
	}

	roles := guildRoles
	if len(roles) == 0 {
		roles, err = s.GuildRoles(guildID)
		if err != nil {
			return "", fmt.Errorf("fetch guild roles: %w", err)
		}
	}

	for _, role := range roles {
		if role == nil {
			continue
		}
		if _, ok := memberRoleSet[role.ID]; !ok {
			continue
		}
		if role.Managed {
			return role.ID, nil
		}
	}
	return "", nil
}

// classifyAttachments downloads and sorts Discord message attachments into
// images, files, and a single voice/audio attachment based on ContentType.
// Attachments whose ContentType is empty fall back to width/height for
// image detection; anything unrecognized is treated as a generic file so
// PDFs, documents, archives, etc. are not silently dropped. If multiple
// audio attachments appear only the last successful one is kept.
func classifyAttachments(atts []*discordgo.MessageAttachment, download func(string) ([]byte, error)) (images []core.ImageAttachment, files []core.FileAttachment, audio *core.AudioAttachment) {
	for _, att := range atts {
		if att == nil {
			continue
		}
		ct := strings.ToLower(att.ContentType)
		switch {
		case strings.HasPrefix(ct, "audio/"):
			data, err := download(att.URL)
			if err != nil {
				slog.Error("discord: download audio failed", "url", att.URL, "error", err)
				continue
			}
			format := "ogg"
			if parts := strings.SplitN(ct, "/", 2); len(parts) == 2 {
				format = parts[1]
			}
			audio = &core.AudioAttachment{MimeType: ct, Data: data, Format: format}
		case strings.HasPrefix(ct, "image/"), ct == "" && att.Width > 0 && att.Height > 0:
			data, err := download(att.URL)
			if err != nil {
				slog.Error("discord: download image failed", "url", att.URL, "file_name", att.Filename, "error", err)
				continue
			}
			images = append(images, core.ImageAttachment{MimeType: att.ContentType, Data: data, FileName: att.Filename})
		default:
			data, err := download(att.URL)
			if err != nil {
				slog.Error("discord: download file failed", "url", att.URL, "file_name", att.Filename, "error", err)
				continue
			}
			files = append(files, core.FileAttachment{MimeType: att.ContentType, Data: data, FileName: att.Filename})
		}
	}
	return images, files, audio
}

const maxDownloadBytes = 50 << 20 // 50 MiB

func downloadURL(u string) ([]byte, error) {
	resp, err := core.HTTPClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: status %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
}

// applyReferencedMessage prepends the replied-to message's author and content
// to content and prepends any images from the referenced message's attachments.
// Image attachments (width > 0) are downloaded via download and prepended so the
// agent sees them before the current message's own images.
func applyReferencedMessage(ref *discordgo.Message, content string, images []core.ImageAttachment, download func(string) ([]byte, error)) (string, []core.ImageAttachment) {
	author := ""
	if ref.Author != nil {
		author = ref.Author.Username
	}
	content = "[replying to " + author + ": " + ref.Content + "]\n" + content
	for _, att := range ref.Attachments {
		if att.Width > 0 && att.Height > 0 {
			data, err := download(att.URL)
			if err != nil {
				slog.Error("discord: download referenced attachment failed", "url", att.URL, "error", err)
				continue
			}
			images = append([]core.ImageAttachment{{
				MimeType: att.ContentType, Data: data, FileName: att.Filename,
			}}, images...)
		}
	}
	return content, images
}
