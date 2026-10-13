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
	StatusOnDoneSummary = "summary" // edit to "✅ 完成 (N 步, 1m 5s)"
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
	sm.current = "开始处理"
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
	sm.progress("思考中", false)
}

// Tool records one tool call as a step.
func (sm *statusMessage) Tool(name, input string) {
	sm.progress(summarizeToolStep(name, input), true)
}

// Writing records streamed answer text.
func (sm *statusMessage) Writing() {
	sm.progress("撰写回复", false)
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

// Tick re-renders the message (elapsed time, stuck marker). Throttled.
func (sm *statusMessage) Tick() {
	sm.flush(false)
}

// flush edits the message when the content changed and the throttle allows it.
func (sm *statusMessage) flush(force bool) {
	if sm == nil {
		return
	}
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
	var text string
	if ok {
		text = fmt.Sprintf("✅ 完成（%d 步，%s）", steps, elapsed)
	} else {
		text = fmt.Sprintf("⏹ 已中断（%d 步，%s）", steps, elapsed)
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
		b.WriteString("⚠️ 可能卡住")
		b.WriteString(fmt.Sprintf("（%s 无进展）", formatStatusElapsed(idle)))
	} else {
		b.WriteString("⏳ 处理中")
	}
	b.WriteString(fmt.Sprintf(" · %s · %d 步", elapsed, sm.steps))
	if cur := strings.TrimSpace(sm.current); cur != "" {
		b.WriteString("\n")
		b.WriteString(statusTruncate(cur, 160))
	}
	return statusTruncate(b.String(), statusMessageMaxRunes)
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

// summarizeToolStep turns a tool call into a short Chinese step label, e.g.
// "读 upstream.go", "运行 go test ./...", "搜索 isGrokChatModel".
func summarizeToolStep(name, input string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	arg := toolStepArg(input)
	verb := ""
	switch {
	case containsAny(n, "todo", "plan"):
		verb = "更新计划"
		arg = ""
	case containsAny(n, "read", "view", "cat", "open_file"):
		verb = "读"
		arg = shortPath(arg)
	case containsAny(n, "edit", "write", "replace", "patch", "create_file", "multiedit", "apply"):
		verb = "改"
		arg = shortPath(arg)
	case containsAny(n, "grep", "glob", "search", "find", "ls", "list"):
		if containsAny(n, "web") {
			verb = "搜网页"
		} else {
			verb = "搜索"
		}
	case containsAny(n, "bash", "shell", "terminal", "exec", "command", "run"):
		verb = "运行"
	case containsAny(n, "fetch", "browse", "web", "http", "url"):
		verb = "查网页"
	case containsAny(n, "task", "agent", "subagent"):
		verb = "子任务"
	}
	label := strings.TrimSpace(name)
	if verb != "" {
		label = verb
	} else if label == "" {
		label = "工具"
	}
	arg = strings.Join(strings.Fields(arg), " ")
	if arg == "" {
		return label
	}
	return label + " " + statusTruncate(arg, 80)
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

func shortPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.ContainsAny(p, " \n") {
		return p
	}
	return filepath.Base(p)
}

// toolStepArg extracts the most useful argument from a tool input, which may
// be JSON (ACP / Claude) or plain text.
func toolStepArg(input string) string {
	in := strings.TrimSpace(input)
	if in == "" {
		return ""
	}
	if strings.HasPrefix(in, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(in), &m) == nil {
			for _, k := range []string{"file_path", "target_file", "path", "filePath", "command", "cmd", "pattern", "query", "url", "description", "prompt"} {
				if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
					return firstLine(v)
				}
			}
			return ""
		}
	}
	return firstLine(in)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
