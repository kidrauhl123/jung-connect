package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const channelNameCacheTTL = 5 * time.Minute

type channelNameCacheEntry struct {
	name      string
	expiresAt time.Time
}

// parseTelegramChannelKey parses the channel ID passed by core effectiveChannelID.
// Telegram ChannelKey is "{chatID}" or "{chatID}:{threadID}" (no platform prefix).
// Chat IDs may be negative (supergroups), so we split on the last colon.
func parseTelegramChannelKey(channelID string) (chatID int64, threadID int, ok bool) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return 0, 0, false
	}
	if idx := strings.LastIndexByte(channelID, ':'); idx > 0 {
		chatPart := channelID[:idx]
		threadPart := channelID[idx+1:]
		cid, err1 := strconv.ParseInt(chatPart, 10, 64)
		tid, err2 := strconv.Atoi(threadPart)
		if err1 == nil && err2 == nil && tid != 0 {
			return cid, tid, true
		}
		// Fall through: treat whole string as chat ID (e.g. unexpected format).
	}
	cid, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return cid, 0, true
}

// sanitizeWorkspaceChannelName makes a name safe as a workspace folder under base_dir.
// Keeps most unicode (topic titles are often Chinese) but strips path separators and controls.
func sanitizeWorkspaceChannelName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r == 0 || r == '/' || r == '\\' || r == ':':
			b.WriteByte('-')
		case unicode.IsSpace(r) || unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	out = strings.TrimSpace(out)
	out = strings.Trim(out, ".-")
	if out == "" || out == "." || out == ".." {
		return ""
	}
	const maxLen = 120
	runes := []rune(out)
	if len(runes) > maxLen {
		out = string(runes[:maxLen])
		out = strings.TrimRight(out, " .-")
	}
	return out
}

func chatDisplayName(info *models.ChatFullInfo) string {
	if info == nil {
		return ""
	}
	if title := strings.TrimSpace(info.Title); title != "" {
		return title
	}
	return strings.TrimSpace(strings.TrimSpace(info.FirstName) + " " + strings.TrimSpace(info.LastName))
}

func (p *Platform) cacheChannelName(key, name string) {
	name = sanitizeWorkspaceChannelName(name)
	if key == "" || name == "" {
		return
	}
	p.channelNameCacheMu.Lock()
	defer p.channelNameCacheMu.Unlock()
	if p.channelNameCache == nil {
		p.channelNameCache = make(map[string]channelNameCacheEntry)
	}
	p.channelNameCache[key] = channelNameCacheEntry{
		name:      name,
		expiresAt: time.Now().Add(channelNameCacheTTL),
	}
}

func (p *Platform) lookupCachedChannelName(key string) (string, bool) {
	p.channelNameCacheMu.RLock()
	defer p.channelNameCacheMu.RUnlock()
	if p.channelNameCache == nil {
		return "", false
	}
	ent, ok := p.channelNameCache[key]
	if !ok {
		return "", false
	}
	if time.Now().After(ent.expiresAt) {
		return "", false
	}
	return ent.name, true
}

// rememberChannelNamesFromMessage opportunistically learns topic/chat titles from
// the incoming update so ResolveChannelName can succeed without a Bot API
// getForumTopic (which does not exist). Non-reply messages in a topic often
// include reply_to_message.forum_topic_created with the topic name.
func (p *Platform) rememberChannelNamesFromMessage(msg *models.Message) {
	if msg == nil {
		return
	}
	if title := strings.TrimSpace(msg.Chat.Title); title != "" {
		p.cacheChannelName(strconv.FormatInt(msg.Chat.ID, 10), title)
	}

	isGroup := msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup
	threadID := 0
	if msg.Chat.IsForum || !isGroup {
		threadID = msg.MessageThreadID
	}

	if msg.ForumTopicCreated != nil {
		if name := strings.TrimSpace(msg.ForumTopicCreated.Name); name != "" {
			// Service message MessageThreadID is the new topic id.
			tid := msg.MessageThreadID
			if tid == 0 {
				tid = threadID
			}
			if tid != 0 {
				p.cacheChannelName(buildChannelKey(msg.Chat.ID, tid), name)
			}
		}
	}
	if msg.ForumTopicEdited != nil {
		if name := strings.TrimSpace(msg.ForumTopicEdited.Name); name != "" && msg.MessageThreadID != 0 {
			p.cacheChannelName(buildChannelKey(msg.Chat.ID, msg.MessageThreadID), name)
		}
	}
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.ForumTopicCreated != nil {
		if name := strings.TrimSpace(msg.ReplyToMessage.ForumTopicCreated.Name); name != "" && threadID != 0 {
			p.cacheChannelName(buildChannelKey(msg.Chat.ID, threadID), name)
		}
	}
	// Edited topic name may appear on reply_to_message when Telegram attaches
	// the topic root; ForumTopicEdited on the reply is uncommon but cheap to check.
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.ForumTopicEdited != nil {
		if name := strings.TrimSpace(msg.ReplyToMessage.ForumTopicEdited.Name); name != "" && threadID != 0 {
			p.cacheChannelName(buildChannelKey(msg.Chat.ID, threadID), name)
		}
	}
}

// ResolveChannelName implements core.ChannelNameResolver.
//
// channelID is the Telegram workspace ChannelKey from effectiveChannelID:
// plain chat → "{chatID}"; forum topic → "{chatID}:{threadID}".
//
// Bot API has no getForumTopic. Topic titles are resolved from an opportunistic
// cache (forum_topic_created / forum_topic_edited / reply_to_message) and fall
// back to getChat title when the topic name is unknown.
func (p *Platform) ResolveChannelName(channelID string) (string, error) {
	chatID, threadID, ok := parseTelegramChannelKey(channelID)
	if !ok {
		return "", fmt.Errorf("telegram: invalid channel id %q", channelID)
	}
	key := buildChannelKey(chatID, threadID)

	if name, hit := p.lookupCachedChannelName(key); hit {
		return name, nil
	}
	// Forum topic: also try exact channelID string in case caller passed a key
	// that already matches buildChannelKey (same as key when format is correct).
	if channelID != key {
		if name, hit := p.lookupCachedChannelName(channelID); hit {
			return name, nil
		}
	}

	// Fetch chat title via GetChat (official Bot API). Does not return topic names.
	bot, err := p.connectedBot("resolve channel name")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := bot.GetChat(ctx, &tgbot.GetChatParams{ChatID: chatID})
	if err != nil {
		return "", fmt.Errorf("telegram: getChat %d: %w", chatID, err)
	}
	title := sanitizeWorkspaceChannelName(chatDisplayName(info))
	if title == "" {
		slog.Debug("telegram: resolve channel name empty", "channel", key)
		return "", fmt.Errorf("telegram: no display name for chat %d", chatID)
	}

	// Cache plain-chat title under chat-only key.
	p.cacheChannelName(strconv.FormatInt(chatID, 10), title)

	if threadID == 0 {
		return title, nil
	}

	// Topic name unknown: Bot API limitation. Prefer cached topic if a concurrent
	// message just warmed it; otherwise fall back to chat title so /workspace
	// auto-bind / skip_git mkdir prompts are non-empty (not ideal uniqueness).
	if name, hit := p.lookupCachedChannelName(key); hit {
		return name, nil
	}
	// General topic conventionally uses thread id 1.
	if threadID == 1 {
		general := sanitizeWorkspaceChannelName("General")
		p.cacheChannelName(key, general)
		return general, nil
	}
	slog.Warn("telegram: forum topic name unknown; falling back to chat title",
		"channel", key, "chat_title", title)
	// Do not cache chat title under the topic key — a later forum_topic_* update
	// should still be able to replace it with the real topic name.
	return title, nil
}
