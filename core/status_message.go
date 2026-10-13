package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Status message: one editable "agent is working" message per turn.
//
// Instead of posting every thinking chunk / tool call as its own IM message,
// the engine sends a single status message when a turn starts and edits it in
// place with the current step, the elapsed time and the step count. Edits are
// throttled; when nothing happens for StuckAfter the message is marked as
// possibly stuck. When the turn ends the message is deleted or turned into a
// one-line summary, and the final reply is sent as a separate message.

// Status message completion modes.
const (
	StatusOnDoneSummary = "summary" // edit to "✅ Done (N steps, 1m 05s)"
	StatusOnDoneDelete  = "delete"  // delete the status message
)

// Defaults for StatusMessageCfg.
const (
	DefaultStatusInterval   = 3 * time.Second
	DefaultStatusStuckAfter = 3 * time.Minute
)

// statusMessageMaxRunes keeps the status well below Discord's 2000-char and
// Telegram's 4096-char message limits.
const statusMessageMaxRunes = 600

// StatusMessageCfg is the project-level ([display] / [projects.display])
// status message configuration.
type StatusMessageCfg struct {
	Enabled    bool
	Interval   time.Duration // min time between edits; <=0 → DefaultStatusInterval
	StuckAfter time.Duration // no-progress threshold; <=0 → DefaultStatusStuckAfter
	OnDone     string        // StatusOnDoneSummary (default) or StatusOnDoneDelete
}

func (c StatusMessageCfg) normalized() StatusMessageCfg {
	if c.Interval <= 0 {
		c.Interval = DefaultStatusInterval
	}
	if c.StuckAfter <= 0 {
		c.StuckAfter = DefaultStatusStuckAfter
	}
	if c.OnDone != StatusOnDoneDelete {
		c.OnDone = StatusOnDoneSummary
	}
	return c
}

// StatusMessagePreference is optionally implemented by platforms whose
// options carry `status_message = true|false`. set=false means "not
// configured on this platform" (fall back to the project default).
type StatusMessagePreference interface {
	StatusMessagePreference() (enabled bool, set bool)
}

// statusMessageEnabled resolves the toggle: channel profile > platform
// option > project/global [display].
func (e *Engine) statusMessageEnabled(p Platform, sessionKey string) bool {
	if cp := e.channelProfileForSessionKey(sessionKey); cp != nil && cp.StatusMessage != nil {
		return *cp.StatusMessage
	}
	if pref, ok := p.(StatusMessagePreference); ok {
		if enabled, set := pref.StatusMessagePreference(); set {
			return enabled
		}
	}
	return e.statusMsgCfg.Enabled
}

// SetStatusMessageConfig sets the project-level status message config.
func (e *Engine) SetStatusMessageConfig(cfg StatusMessageCfg) {
	e.statusMsgCfg = cfg.normalized()
}

// statusMessage drives one status message for one turn.
type statusMessage struct {
	ctx      context.Context
	platform Platform
	replyCtx any
	starter  PreviewStarter
	updater  MessageUpdater
	cleaner  PreviewCleaner // may be nil
	cfg      StatusMessageCfg
	now      func() time.Time

	mu           sync.Mutex
	handle       any
	failed       bool
	finished     bool
	start        time.Time
	lastProgress time.Time
	lastEdit     time.Time
	lastSent     string
	steps        int
	current      string
	backoffUntil time.Time
	editFailures int

	// usage is polled before each render (context window load); nil-safe.
	usageFn    func() *ContextUsage
	ctxUsed    int
	ctxSize    int
	turnTokens int

	stopTicker chan struct{}
	tickerDone chan struct{}
}

// newStatusMessage returns nil when the platform cannot send+edit messages.
func newStatusMessage(ctx context.Context, p Platform, replyCtx any, cfg StatusMessageCfg) *statusMessage {
	starter, ok1 := p.(PreviewStarter)
	updater, ok2 := p.(MessageUpdater)
	if !ok1 || !ok2 {
		return nil
	}
	cfg = cfg.normalized()
	if t, ok := p.(ProgressUpdateThrottler); ok {
		if d := t.ProgressUpdateInterval(); d > cfg.Interval {
			cfg.Interval = d
		}
	}
	sm := &statusMessage{
		ctx:      ctx,
		platform: p,
		replyCtx: replyCtx,
		starter:  starter,
		updater:  updater,
		cfg:      cfg,
		now:      time.Now,
	}
	if c, ok := p.(PreviewCleaner); ok {
		sm.cleaner = c
	}
	return sm
}

// Start sends the initial status message. Returns false when sending failed
// (the caller should then fall back to the normal per-event messages).
func (sm *statusMessage) Start() bool {
	if sm == nil {
		return false
	}
	sm.mu.Lock()
	now := sm.now()
	sm.start, sm.lastProgress = now, now
	sm.current = "⏳ Starting"
	text := sm.renderLocked(now)
	sm.mu.Unlock()

	callCtx, cancel := context.WithTimeout(sm.ctx, compactProgressAPITimeout)
	handle, err := sm.starter.SendPreviewStart(callCtx, sm.replyCtx, text)
	cancel()
	if err != nil || handle == nil {
		slog.Warn("status message: send failed, falling back", "platform", sm.platform.Name(), "error", err)
		sm.mu.Lock()
		sm.failed = true
		sm.mu.Unlock()
		return false
	}
	sm.mu.Lock()
	sm.handle = handle
	sm.lastSent = text
	sm.lastEdit = now
	sm.mu.Unlock()
	return true
}

// StartTicker refreshes the elapsed time / stuck marker every interval even
// when no agent events arrive. Stop it with Finish/Abort.
func (sm *statusMessage) StartTicker() {
	if sm == nil || sm.failed {
		return
	}
	sm.stopTicker = make(chan struct{})
	sm.tickerDone = make(chan struct{})
	go func() {
		defer close(sm.tickerDone)
		t := time.NewTicker(sm.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-sm.stopTicker:
				return
			case <-sm.ctx.Done():
				return
			case <-t.C:
				sm.Tick()
			}
		}
	}()
}

func (sm *statusMessage) stopTickerAndWait() {
	if sm.stopTicker != nil {
		close(sm.stopTicker)
		<-sm.tickerDone
		sm.stopTicker = nil
	}
}

// Thinking records a thinking event (not counted as a step).
func (sm *statusMessage) Thinking() {
	sm.progress("💭 Thinking", false)
}

// Tool records one tool call as a step.
func (sm *statusMessage) Tool(name, input string) {
	sm.progress(summarizeToolStep(name, input), true)
}

// Writing records streamed answer text.
func (sm *statusMessage) Writing() {
	sm.progress("✍️ Writing reply", false)
}

func (sm *statusMessage) progress(summary string, step bool) {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	if sm.failed || sm.finished {
		sm.mu.Unlock()
		return
	}
	sm.lastProgress = sm.now()
	if step {
		sm.steps++
	}
	if summary != "" {
		sm.current = summary
	}
	sm.mu.Unlock()
	sm.flush(false)
}

// SetUsageSource installs a callback returning the session's current
// context usage (ACP usage_update, Codex token_count, ...).
func (sm *statusMessage) SetUsageSource(fn func() *ContextUsage) {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	sm.usageFn = fn
	sm.mu.Unlock()
}

// SetTurnTokens records the token total of the finished turn (0 = unknown).
func (sm *statusMessage) SetTurnTokens(n int) {
	if sm == nil || n <= 0 {
		return
	}
	sm.mu.Lock()
	sm.turnTokens = n
	sm.mu.Unlock()
}

func (sm *statusMessage) refreshUsage() {
	sm.mu.Lock()
	fn := sm.usageFn
	sm.mu.Unlock()
	if fn == nil {
		return
	}
	u := fn()
	if u == nil {
		return
	}
	used := u.UsedTokens
	if used <= 0 {
		used = u.TotalTokens
	}
	sm.mu.Lock()
	if used > 0 {
		sm.ctxUsed = used
	}
	if u.ContextWindow > 0 {
		sm.ctxSize = u.ContextWindow
	}
	sm.mu.Unlock()
}

// Tick re-renders the message (elapsed time, stuck marker). Throttled.
func (sm *statusMessage) Tick() {
	sm.flush(false)
}

// flush edits the message when the content changed and the throttle allows it.
func (sm *statusMessage) flush(force bool) {
	if sm == nil {
		return
	}
	sm.refreshUsage()
	sm.mu.Lock()
	if sm.failed || sm.handle == nil || (sm.finished && !force) {
		sm.mu.Unlock()
		return
	}
	now := sm.now()
	if !force {
		if now.Before(sm.backoffUntil) || now.Sub(sm.lastEdit) < sm.cfg.Interval {
			sm.mu.Unlock()
			return
		}
	}
	text := sm.renderLocked(now)
	if text == sm.lastSent {
		sm.mu.Unlock()
		return
	}
	handle := sm.handle
	sm.lastEdit = now
	sm.mu.Unlock()

	sm.edit(handle, text)
}

func (sm *statusMessage) edit(handle any, text string) bool {
	callCtx, cancel := context.WithTimeout(sm.ctx, compactProgressAPITimeout)
	err := sm.updater.UpdateMessage(callCtx, handle, text)
	cancel()

	sm.mu.Lock()
	defer sm.mu.Unlock()
	if err == nil || isMessageNotModifiedError(err) {
		sm.lastSent = text
		sm.editFailures = 0
		return true
	}
	if isProgressRateLimitError(err) {
		wait := retryAfterFromError(err)
		if wait <= 0 {
			wait = 2 * sm.cfg.Interval
		}
		sm.backoffUntil = sm.now().Add(wait)
		slog.Debug("status message: rate-limited", "platform", sm.platform.Name(), "retry_in", wait)
		return false
	}
	sm.editFailures++
	slog.Warn("status message: edit failed", "platform", sm.platform.Name(), "error", err, "failures", sm.editFailures)
	if sm.editFailures >= 3 {
		sm.failed = true
	}
	return false
}

// Finish ends the status message: delete it or edit it into a summary.
// ok=false marks the turn as failed/interrupted.
func (sm *statusMessage) Finish(ok bool) {
	if sm == nil {
		return
	}
	sm.stopTickerAndWait()
	sm.mu.Lock()
	if sm.finished || sm.handle == nil {
		sm.finished = true
		sm.mu.Unlock()
		return
	}
	sm.finished = true
	handle := sm.handle
	now := sm.now()
	elapsed := formatStatusElapsed(now.Sub(sm.start))
	steps := sm.steps
	mode := sm.cfg.OnDone
	tokens := sm.turnTokens
	sm.mu.Unlock()

	if ok && mode == StatusOnDoneDelete && sm.cleaner != nil {
		callCtx, cancel := context.WithTimeout(sm.ctx, compactProgressAPITimeout)
		err := sm.cleaner.DeletePreviewMessage(callCtx, handle)
		cancel()
		if err == nil {
			return
		}
		slog.Debug("status message: delete failed, editing to summary instead", "error", err)
	}
	detail := fmt.Sprintf("%s, %s", statusStepsLabel(steps), elapsed)
	if tokens > 0 {
		detail += " · " + formatTokenCount(tokens) + " tokens"
	}
	var text string
	if ok {
		text = "✅ Done (" + detail + ")"
	} else {
		text = "⏹ Interrupted (" + detail + ")"
	}
	sm.mu.Lock()
	sm.backoffUntil = time.Time{}
	sm.mu.Unlock()
	if !sm.edit(handle, text) {
		// One retry after a rate-limit back-off so the final state is not lost.
		sm.mu.Lock()
		wait := sm.backoffUntil.Sub(sm.now())
		sm.mu.Unlock()
		if wait > 0 && wait <= 10*time.Second {
			select {
			case <-time.After(wait):
				sm.edit(handle, text)
			case <-sm.ctx.Done():
			}
		}
	}
}

// Active reports whether the status message replaces per-event messages.
func (sm *statusMessage) Active() bool {
	if sm == nil {
		return false
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.handle != nil && !sm.failed
}

func (sm *statusMessage) renderLocked(now time.Time) string {
	elapsed := formatStatusElapsed(now.Sub(sm.start))
	var b strings.Builder
	idle := now.Sub(sm.lastProgress)
	if idle >= sm.cfg.StuckAfter {
		b.WriteString(fmt.Sprintf("⚠️ Possibly stuck (%s idle)", formatStatusElapsed(idle)))
	} else {
		b.WriteString("⏳ Working")
	}
	b.WriteString(fmt.Sprintf(" · %s · %s", elapsed, statusStepsLabel(sm.steps)))
	if c := formatContextUsage(sm.ctxUsed, sm.ctxSize); c != "" {
		b.WriteString(" · ")
		b.WriteString(c)
	}
	if cur := strings.TrimSpace(sm.current); cur != "" {
		b.WriteString("\n")
		b.WriteString(statusTruncate(cur, 80))
	}
	return statusTruncate(b.String(), statusMessageMaxRunes)
}

func statusStepsLabel(n int) string {
	if n == 1 {
		return "1 step"
	}
	return fmt.Sprintf("%d steps", n)
}

// statusCtxWarnPercent switches the ctx segment to a warning.
const statusCtxWarnPercent = 80

// formatContextUsage renders "ctx 48k/500k (10%)", "⚠️ ctx 92%" or "ctx 48k".
func formatContextUsage(used, size int) string {
	if used <= 0 {
		return ""
	}
	if size <= 0 {
		return "ctx " + formatTokenCount(used)
	}
	pct := used * 100 / size
	if pct >= statusCtxWarnPercent {
		return fmt.Sprintf("⚠️ ctx %d%%", pct)
	}
	return fmt.Sprintf("ctx %s/%s (%d%%)", formatTokenCount(used), formatTokenCount(size), pct)
}

// formatTokenCount renders 950, 12.3k, 500k, 1.2M.
func formatTokenCount(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimDotZero(fmt.Sprintf("%.1f", float64(n)/1000)) + "k"
	default:
		return trimDotZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	}
}

func trimDotZero(s string) string {
	if strings.HasSuffix(s, ".0") {
		return s[:len(s)-2]
	}
	return s
}

func statusTruncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func formatStatusElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%dm %02ds", m, s%60)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

func isMessageNotModifiedError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not modified")
}

var retryAfterRe = regexp.MustCompile(`(?i)retry[ _-]?after[^0-9]*([0-9]+(?:\.[0-9]+)?)`)

func retryAfterFromError(err error) time.Duration {
	if err == nil {
		return 0
	}
	m := retryAfterRe.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return 0
	}
	f, perr := strconv.ParseFloat(m[1], 64)
	if perr != nil || f <= 0 {
		return 0
	}
	if f > 300 {
		f = 300
	}
	return time.Duration(f * float64(time.Second))
}

// summarizeToolStep turns a tool call into one short step line such as
// "📖 Reading engine.go", "💻 Running go test ./core" or
// "🔌 gmail·search_threads from:github".
func summarizeToolStep(name, input string) string {
	name = strings.TrimSpace(name)
	m := parseToolInputJSON(input)

	// Grok wraps MCP calls: {"tool_name":"srv__tool","tool_input":{...}}.
	if m != nil {
		if inner, ok := m["tool_name"].(string); ok && strings.TrimSpace(inner) != "" {
			name = strings.TrimSpace(inner)
			if ti, ok := m["tool_input"].(map[string]any); ok {
				m = ti
			} else if ts, ok := m["tool_input"].(string); ok {
				m = parseToolInputJSON(ts)
				if m == nil {
					input = ts
				}
			} else {
				m = map[string]any{}
			}
		}
	}

	server, tool := splitMCPToolName(name)
	words := toolNameWords(tool)
	if server != "" {
		words = append(toolNameWords(server), words...)
	}
	has := func(ws ...string) bool {
		for _, w := range ws {
			for _, x := range words {
				if x == w {
					return true
				}
			}
		}
		return false
	}
	pick := func(keys ...string) string {
		if m == nil {
			return ""
		}
		for _, k := range keys {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	plain := ""
	if m == nil {
		plain = strings.TrimSpace(input)
	}
	pathArg := func() string {
		if v := pick("file_path", "target_file", "filePath", "path", "notebook_path", "filename", "file"); v != "" {
			return displayPath(v)
		}
		if p := patchFilePath(plain); p != "" {
			return displayPath(p)
		}
		if plain != "" && !strings.ContainsAny(plain, "\n") {
			return displayPath(plain)
		}
		return ""
	}

	var label, arg string
	webWords := []string{"web", "browse", "browser", "fetch", "url", "http", "exa", "tavily", "brave", "serp", "google", "bing", "jina", "perplexity", "playwright", "puppeteer", "crawl"}
	webish := has(webWords...)
	webServer := false
	if server != "" {
		for _, w := range toolNameWords(server) {
			for _, x := range append(webWords, "search") {
				if w == x {
					webServer = true
				}
			}
		}
		if !webServer {
			for _, w := range toolNameWords(tool) {
				for _, x := range webWords {
					if w == x {
						webServer = true
					}
				}
			}
		}
	}
	switch {
	case webServer:
		toolSearch := false
		for _, w := range toolNameWords(tool) {
			if w == "search" || w == "query" {
				toolSearch = true
			}
		}
		if toolSearch || pick("query", "q", "search_term") != "" {
			label = "🌐 Searching web"
			arg = pick("query", "q", "search_term")
		} else {
			label = "🌐 Browsing"
			arg = displayURL(pick("url", "urls"))
		}
		if arg == "" {
			arg = firstStringArg(m)
			if arg == "" {
				arg = displayURL(firstURLArg(m))
			}
		}
	case server != "":
		label = "🔌 " + server + "·" + tool
		arg = firstStringArg(m)
	case has("todo", "todos", "plan"):
		label = "📝 Updating plan"
	case has("task", "agent", "subagent", "delegate", "spawn"):
		label = "🤖 Subagent"
		arg = pick("description", "prompt", "task")
	case has("think", "thinking", "reason"):
		label = "💭 Thinking"
	case webish || (has("search") && has("web")):
		if has("search") || pick("query") != "" {
			label = "🌐 Searching web"
			arg = pick("query", "q", "search_term")
		} else {
			label = "🌐 Browsing"
			arg = displayURL(pick("url"))
		}
	case has("ls", "list", "dir", "directory", "tree") && !has("grep", "search", "find"):
		label = "📂 Listing"
		if v := pick("target_directory", "path", "directory", "dir", "relative_workspace_path"); v != "" {
			arg = displayPath(v)
		}
	case has("read", "view", "cat", "open"):
		label = "📖 Reading"
		arg = pathArg()
	case has("edit", "write", "replace", "patch", "create", "apply", "multiedit", "notebookedit", "str", "insert"):
		label = "✏️ Editing"
		arg = pathArg()
	case has("grep", "glob", "search", "find", "rg", "codebase"):
		label = "🔍 Searching"
		if q := pick("pattern", "query", "regex", "search_term", "glob", "glob_pattern"); q != "" {
			arg = strconv.Quote(collapseSpace(q))
			if g := pick("glob", "glob_pattern", "include"); g != "" && g != q {
				arg += " (" + g + ")"
			}
		}
	case has("bash", "shell", "terminal", "exec", "execute", "command", "run", "cmd", "sh"):
		label = "💻 Running"
		if v := pick("command", "cmd", "script"); v != "" {
			arg = commandLine(v)
		} else if m == nil {
			arg = commandLine(plain)
		} else if arr, ok := m["command"].([]any); ok {
			parts := make([]string, 0, len(arr))
			for _, x := range arr {
				if s, ok := x.(string); ok {
					parts = append(parts, s)
				}
			}
			arg = commandLine(strings.Join(parts, " "))
		}
	default:
		if name == "" {
			name = "tool"
		}
		label = "⚙️ " + name
		if m != nil {
			arg = firstStringArg(m)
		} else {
			arg = firstLine(plain)
		}
	}
	arg = collapseSpace(arg)
	if arg == "" {
		return label
	}
	return statusTruncate(label+" "+arg, statusStepMaxRunes)
}

// statusStepMaxRunes caps the single step line.
const statusStepMaxRunes = 60

func parseToolInputJSON(input string) map[string]any {
	in := strings.TrimSpace(input)
	if !strings.HasPrefix(in, "{") {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(in), &m) != nil {
		return nil
	}
	return m
}

// splitMCPToolName splits "mcp__server__tool" and "server__tool".
func splitMCPToolName(name string) (server, tool string) {
	n := strings.TrimPrefix(name, "mcp__")
	if i := strings.Index(n, "__"); i > 0 && i+2 < len(n) {
		return n[:i], n[i+2:]
	}
	return "", name
}

// toolNameWords splits snake_case, kebab-case, dotted and camelCase names
// into lower-case words: "WebFetch" → [web fetch], "list_dir" → [list dir].
func toolNameWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ' || r == '/' || r == ':':
			flush()
		case r >= 'A' && r <= 'Z':
			if len(cur) > 0 {
				prev := rs[i-1]
				nextLower := i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
				if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') || (prev >= 'A' && prev <= 'Z' && nextLower) {
					flush()
				}
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// commandLine returns the first non-comment, non-empty line of a shell
// command ("# describe\ncmd" → "cmd"), plus "…" when more lines follow.
func commandLine(cmd string) string {
	var lines []string
	for _, l := range strings.Split(cmd, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 1 {
		return lines[0] + " …"
	}
	return lines[0]
}

// displayPath shows the basename, or parent/base for generic names.
func displayPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.ContainsAny(p, "\n") {
		return firstLine(p)
	}
	clean := strings.TrimRight(p, "/")
	if clean == "" {
		return p
	}
	base := filepath.Base(clean)
	parent := filepath.Base(filepath.Dir(clean))
	if ambiguousBaseName(base) && parent != "." && parent != "/" && parent != "" {
		return parent + "/" + base
	}
	return base
}

func ambiguousBaseName(base string) bool {
	switch strings.ToLower(base) {
	case "readme.md", "skill.md", "agents.md", "claude.md", "index.ts", "index.tsx", "index.js",
		"main.go", "mod.rs", "lib.rs", "__init__.py", "package.json", "config.toml", "config.yaml",
		"config.yml", "makefile", "dockerfile", "go.mod", "settings.json", ".env":
		return true
	}
	return false
}

func displayURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "www.")
	return u
}

func patchFilePath(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		for _, pfx := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "+++ b/", "--- a/"} {
			if strings.HasPrefix(l, pfx) {
				return strings.TrimSpace(strings.TrimPrefix(l, pfx))
			}
		}
	}
	return ""
}

// firstStringArg picks the most informative string argument.
func firstStringArg(m map[string]any) string {
	if m == nil {
		return ""
	}
	for _, k := range []string{"query", "q", "search_term", "url", "file_path", "target_file", "path", "command", "pattern", "prompt", "description", "name", "id", "title", "text", "message"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			if k == "url" {
				return displayURL(v)
			}
			if k == "file_path" || k == "target_file" || k == "path" {
				return displayPath(v)
			}
			if k == "command" {
				return commandLine(v)
			}
			return firstLine(v)
		}
	}
	return ""
}

func firstURLArg(m map[string]any) string {
	if m == nil {
		return ""
	}
	for _, k := range []string{"url", "urls"} {
		switch v := m[k].(type) {
		case string:
			return v
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
