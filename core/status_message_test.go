package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type statusFakePlatform struct {
	stubPlatformEngine
	mu        sync.Mutex
	starts    []string
	edits     []string
	deletes   int
	editErrs  []error // consumed in order, nil = success
	deleteErr error
	pref      *bool
}

func (p *statusFakePlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts = append(p.starts, content)
	return "status-handle", nil
}

func (p *statusFakePlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.editErrs) > 0 {
		err := p.editErrs[0]
		p.editErrs = p.editErrs[1:]
		if err != nil {
			return err
		}
	}
	p.edits = append(p.edits, content)
	return nil
}

func (p *statusFakePlatform) DeletePreviewMessage(_ context.Context, _ any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes++
	return p.deleteErr
}

func (p *statusFakePlatform) StatusMessagePreference() (bool, bool) {
	if p.pref == nil {
		return false, false
	}
	return *p.pref, true
}

func (p *statusFakePlatform) snapshot() (starts, edits []string, deletes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.starts...), append([]string(nil), p.edits...), p.deletes
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock           { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }
func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

func newTestStatus(t *testing.T, p *statusFakePlatform, cfg StatusMessageCfg) (*statusMessage, *fakeClock) {
	t.Helper()
	sm := newStatusMessage(context.Background(), p, "ctx", cfg)
	if sm == nil {
		t.Fatal("newStatusMessage returned nil")
	}
	clk := newFakeClock()
	sm.now = clk.now
	if !sm.Start() {
		t.Fatal("Start failed")
	}
	return sm, clk
}

func TestStatusMessage_ThrottlesEdits(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm, clk := newTestStatus(t, p, StatusMessageCfg{Interval: 3 * time.Second})

	sm.Tool("read_file", `{"target_file":"/root/x/upstream.go"}`)
	clk.add(time.Second)
	sm.Tool("grep", `{"pattern":"isGrokChatModel"}`)
	if _, edits, _ := p.snapshot(); len(edits) != 0 {
		t.Fatalf("edits within interval = %v, want none", edits)
	}
	clk.add(3 * time.Second)
	sm.Tool("run_terminal_command", "go test ./...")
	starts, edits, _ := p.snapshot()
	if len(starts) != 1 || len(edits) != 1 {
		t.Fatalf("starts=%v edits=%v, want 1/1", starts, edits)
	}
	if !strings.Contains(edits[0], "3 steps") || !strings.Contains(edits[0], "Running go test ./...") || !strings.Contains(edits[0], "4s") {
		t.Fatalf("edit = %q", edits[0])
	}
	// Unchanged content is not re-sent even when the throttle allows it.
	sm.mu.Lock()
	sm.lastEdit = time.Time{}
	sm.mu.Unlock()
	sm.Tick()
	if _, edits, _ := p.snapshot(); len(edits) != 1 {
		t.Fatalf("unchanged tick re-sent: %v", edits)
	}
}

func TestStatusMessage_StuckDetection(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm, clk := newTestStatus(t, p, StatusMessageCfg{Interval: time.Second, StuckAfter: 3 * time.Minute})
	sm.Tool("read_file", `{"path":"a.go"}`)
	clk.add(2 * time.Minute)
	sm.Tick()
	_, edits, _ := p.snapshot()
	if strings.Contains(lastOf(edits), "Possibly stuck") {
		t.Fatalf("stuck too early: %q", lastOf(edits))
	}
	clk.add(70 * time.Second)
	sm.Tick()
	_, edits, _ = p.snapshot()
	if !strings.Contains(lastOf(edits), "Possibly stuck") || !strings.Contains(lastOf(edits), "3m 10s idle") {
		t.Fatalf("want stuck marker, got %q", lastOf(edits))
	}
	clk.add(2 * time.Second)
	sm.Tool("edit", `{"file_path":"a.go"}`)
	_, edits, _ = p.snapshot()
	if strings.Contains(lastOf(edits), "Possibly stuck") || !strings.Contains(lastOf(edits), "Editing a.go") {
		t.Fatalf("progress should clear stuck marker: %q", lastOf(edits))
	}
}

func TestStatusMessage_CompletionSummaryAndDelete(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm, clk := newTestStatus(t, p, StatusMessageCfg{})
	sm.Tool("bash", "ls")
	sm.Tool("bash", "pwd")
	clk.add(65 * time.Second)
	sm.Finish(true)
	_, edits, deletes := p.snapshot()
	if deletes != 0 || lastOf(edits) != "✅ Done (2 steps, 1m 05s)" {
		t.Fatalf("summary: deletes=%d last=%q", deletes, lastOf(edits))
	}
	sm.Finish(true) // idempotent
	if _, e2, _ := p.snapshot(); len(e2) != len(edits) {
		t.Fatal("second Finish must be a no-op")
	}

	p2 := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm2, _ := newTestStatus(t, p2, StatusMessageCfg{OnDone: StatusOnDoneDelete})
	sm2.Finish(true)
	if _, _, d := p2.snapshot(); d != 1 {
		t.Fatalf("deletes = %d, want 1", d)
	}

	// Delete failure falls back to the summary edit; failure is never deleted.
	p3 := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}, deleteErr: errors.New("403 missing permissions")}
	sm3, _ := newTestStatus(t, p3, StatusMessageCfg{OnDone: StatusOnDoneDelete})
	sm3.Finish(true)
	if _, e3, _ := p3.snapshot(); !strings.HasPrefix(lastOf(e3), "✅ Done") {
		t.Fatalf("fallback summary = %v", e3)
	}
	p4 := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm4, _ := newTestStatus(t, p4, StatusMessageCfg{OnDone: StatusOnDoneDelete})
	sm4.Finish(false)
	if _, e4, d4 := p4.snapshot(); d4 != 0 || !strings.HasPrefix(lastOf(e4), "⏹ Interrupted") {
		t.Fatalf("interrupted: deletes=%d edits=%v", d4, e4)
	}
}

func TestStatusMessage_NotModifiedAndRateLimit(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"},
		editErrs: []error{
			errors.New("Bad Request: message is not modified"),
			errors.New("Too Many Requests: retry after 7"),
		}}
	sm, clk := newTestStatus(t, p, StatusMessageCfg{Interval: time.Second})
	clk.add(2 * time.Second)
	sm.Tool("bash", "a") // not modified → treated as sent
	if !sm.Active() {
		t.Fatal("not-modified must not disable the status message")
	}
	clk.add(2 * time.Second)
	sm.Tool("bash", "b") // 429 → back off 7s
	clk.add(3 * time.Second)
	sm.Tool("bash", "c") // still backing off
	if _, edits, _ := p.snapshot(); len(edits) != 0 {
		t.Fatalf("edited during back-off: %v", edits)
	}
	clk.add(5 * time.Second)
	sm.Tool("bash", "d")
	if _, edits, _ := p.snapshot(); len(edits) != 1 || !strings.Contains(edits[0], "Running d") {
		t.Fatalf("after back-off edits = %v", edits)
	}
	if !sm.Active() {
		t.Fatal("rate limit must not disable the status message")
	}
}

func TestStatusMessage_RepeatedFailuresDisable(t *testing.T) {
	boom := errors.New("Unknown Message")
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}, editErrs: []error{boom, boom, boom}}
	sm, clk := newTestStatus(t, p, StatusMessageCfg{Interval: time.Second})
	for i := 0; i < 3; i++ {
		clk.add(2 * time.Second)
		sm.Tool("bash", strings.Repeat("x", i+1))
	}
	if sm.Active() {
		t.Fatal("status message should disable after 3 hard failures")
	}
}

func TestStatusMessage_RenderStaysShort(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm, clk := newTestStatus(t, p, StatusMessageCfg{Interval: time.Second})
	clk.add(2 * time.Second)
	sm.Tool("bash", strings.Repeat("长", 5000))
	_, edits, _ := p.snapshot()
	if n := len([]rune(lastOf(edits))); n == 0 || n > statusMessageMaxRunes {
		t.Fatalf("status length = %d", n)
	}
}

func TestSummarizeToolStep(t *testing.T) {
	cases := []struct{ name, input, want string }{
		// grok ACP shapes (from ADA logs)
		{"read_file", `{"target_file":"/root/jung-connect/core/upstream.go"}`, "📖 Reading upstream.go"},
		{"read_file", `{"target_file":"/root/JungUI/skills/jungui/SKILL.md"}`, "📖 Reading jungui/SKILL.md"},
		{"list_dir", `{"target_directory":"/root/JungUI"}`, "📂 Listing JungUI"},
		{"search_replace", `{"file_path":"x/engine.go"}`, "✏️ Editing engine.go"},
		{"Execute", "# Check git status\ncd /root/notes && git status", "💻 Running cd /root/notes && git status"},
		{"Execute", "# Run tests\ngo test ./core\necho done", "💻 Running go test ./core …"},
		{"grep", `{"path":"/root/notes","pattern":"opencode2api|OpenCode2API","glob":"*.md"}`, `🔍 Searching "opencode2api|OpenCode2API" (*.md)`},
		{"use_tool", `{"tool_input":{"query":"jasonxu114514 opencode2api pull requests","max_results":8},"tool_name":"jung-search__search","variant":"UseTool"}`, "🌐 Searching web jasonxu114514 opencode2api pull requests"},
		{"use_tool", `{"tool_input":{"urls":["https://github.com/x/y"]},"tool_name":"jung-search__extract"}`, "🌐 Browsing github.com/x/y"},
		{"use_tool", `{"tool_input":{"query":"from:github"},"tool_name":"gmail__search_threads"}`, "🔌 gmail·search_threads from:github"},
		{"use_tool", `{"tool_input":{},"tool_name":"gmail__list_labels"}`, "🔌 gmail·list_labels"},
		// Claude Code
		{"Read", `{"file_path":"/a/b/c.txt"}`, "📖 Reading c.txt"},
		{"Edit", `{"file_path":"/repo/core/engine.go","old_string":"a","new_string":"b"}`, "✏️ Editing engine.go"},
		{"Bash", `{"command":"go test ./core","description":"Run tests"}`, "💻 Running go test ./core"},
		{"Glob", `{"pattern":"**/*.go"}`, `🔍 Searching "**/*.go"`},
		{"WebFetch", `{"url":"https://example.com/docs","prompt":"x"}`, "🌐 Browsing example.com/docs"},
		{"WebSearch", `{"query":"acp usage_update"}`, "🌐 Searching web acp usage_update"},
		{"TodoWrite", `{"todos":[]}`, "📝 Updating plan"},
		{"Task", `{"description":"Explore repo","prompt":"..."}`, "🤖 Subagent Explore repo"},
		{"mcp__github__get_issue", `{"owner":"a","repo":"b","issue_number":3}`, "🔌 github·get_issue"},
		{"mcp__exa__web_search_exa", `{"query":"grok cli"}`, "🌐 Searching web grok cli"},
		// Codex
		{"exec_command", `{"cmd":"ls -la"}`, "💻 Running ls -la"},
		{"shell", `{"command":["bash","-lc","make test"]}`, "💻 Running bash -lc make test"},
		{"apply_patch", "*** Begin Patch\n*** Update File: core/x.go\n@@", "✏️ Editing x.go"},
		{"update_plan", `{"plan":[]}`, "📝 Updating plan"},
		// misc
		{"think", "", "💭 Thinking"},
		{"mystery", "", "⚙️ mystery"},
		{"AskUserQuestion", `{"question":"ok?"}`, "⚙️ AskUserQuestion"},
		{"run_terminal_command", "go test ./...\nmore", "💻 Running go test ./... …"},
		{"web_fetch", `{"url":"https://example.com"}`, "🌐 Browsing example.com"},
		// word-based: "tools"/"thread" must not match ls/read
		{"list_tools", `{}`, "📂 Listing"},
		{"get_thread", `{"id":"t1"}`, "⚙️ get_thread t1"},
	}
	for _, c := range cases {
		if got := summarizeToolStep(c.name, c.input); got != c.want {
			t.Errorf("summarizeToolStep(%q, %q) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
}

func TestSummarizeToolStep_TruncatesLongCommands(t *testing.T) {
	cmd := "go test -tags no_web ./core ./config ./platform/discord ./platform/telegram ./cmd/... -run VeryLongPattern"
	got := summarizeToolStep("Bash", `{"command":"`+cmd+`"}`)
	if n := utf8.RuneCountInString(got); n > statusStepMaxRunes {
		t.Fatalf("len %d > %d: %q", n, statusStepMaxRunes, got)
	}
	if !strings.HasPrefix(got, "💻 Running go test -tags no_web") || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q", got)
	}
	if got := summarizeToolStep("Bash", "{\"command\":\"echo   a\\n\\t  b\"}"); got != "💻 Running echo a …" {
		t.Fatalf("whitespace: %q", got)
	}
}

func TestToolNameWords(t *testing.T) {
	cases := map[string]string{
		"WebFetch":         "web fetch",
		"list_dir":         "list dir",
		"jung-search":      "jung search",
		"HTTPRequest":      "http request",
		"run_terminal_cmd": "run terminal cmd",
		"NotebookEdit":     "notebook edit",
		"exec.command":     "exec command",
	}
	for in, want := range cases {
		if got := strings.Join(toolNameWords(in), " "); got != want {
			t.Errorf("toolNameWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatTokenAndContextUsage(t *testing.T) {
	tok := map[int]string{0: "0", 950: "950", 1000: "1k", 12345: "12.3k", 500000: "500k", 1234567: "1.2M"}
	for n, want := range tok {
		if got := formatTokenCount(n); got != want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", n, got, want)
		}
	}
	ctx := []struct {
		used, size int
		want       string
	}{
		{0, 500000, ""},
		{48000, 500000, "ctx 48k/500k (9%)"},
		{50000, 500000, "ctx 50k/500k (10%)"},
		{460000, 500000, "⚠️ ctx 92%"},
		{400000, 500000, "⚠️ ctx 80%"},
		{48000, 0, "ctx 48k"},
	}
	for _, c := range ctx {
		if got := formatContextUsage(c.used, c.size); got != c.want {
			t.Errorf("formatContextUsage(%d,%d) = %q, want %q", c.used, c.size, got, c.want)
		}
	}
}

func TestStatusMessage_UsageHeaderAndDoneTokens(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm, clk := newTestStatus(t, p, StatusMessageCfg{Enabled: true, Interval: time.Second})
	var usage *ContextUsage
	sm.SetUsageSource(func() *ContextUsage { return usage })
	clk.add(2 * time.Second)
	sm.Tool("read_file", `{"target_file":"/x/engine.go"}`)
	_, edits, _ := p.snapshot()
	if strings.Contains(lastOf(edits), "ctx") {
		t.Fatalf("ctx shown without usage: %q", lastOf(edits))
	}
	usage = &ContextUsage{UsedTokens: 50000, ContextWindow: 500000}
	clk.add(2 * time.Second)
	sm.Tool("grep", `{"pattern":"x"}`)
	_, edits, _ = p.snapshot()
	want := "⏳ Working · 4s · 2 steps · ctx 50k/500k (10%)\n🔍 Searching \"x\""
	if lastOf(edits) != want {
		t.Fatalf("header = %q, want %q", lastOf(edits), want)
	}
	usage = &ContextUsage{UsedTokens: 460000, ContextWindow: 500000}
	clk.add(2 * time.Second)
	sm.Thinking()
	_, edits, _ = p.snapshot()
	if !strings.Contains(lastOf(edits), "· ⚠️ ctx 92%\n💭 Thinking") {
		t.Fatalf("warn header = %q", lastOf(edits))
	}
	clk.add(155 * time.Second)
	sm.SetTurnTokens(12345)
	sm.Finish(true)
	_, edits, _ = p.snapshot()
	if lastOf(edits) != "✅ Done (2 steps, 2m 41s · 12.3k tokens)" {
		t.Fatalf("done = %q", lastOf(edits))
	}
}

func TestStatusMessageEnabled_Precedence(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "discord"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	on, off := true, false
	e.SetChannelProfiles([]ChannelProfile{
		{Match: "discord:QUIET", StatusMessage: &off},
		{Match: "discord:LOUD", StatusMessage: &on},
		{Match: "discord:PLAIN"},
	})
	quiet, loud, plain := "discord:QUIET:u1", "discord:LOUD:u1", "discord:PLAIN:u1"
	for _, k := range []string{quiet, loud, plain} {
		e.channelProfileForMessage(&Message{Platform: "discord", SessionKey: k})
	}

	// Project default off, no platform preference.
	if e.statusMessageEnabled(p, plain) || !e.statusMessageEnabled(p, loud) || e.statusMessageEnabled(p, quiet) {
		t.Fatal("project-off precedence wrong")
	}
	// Project default on.
	e.SetStatusMessageConfig(StatusMessageCfg{Enabled: true})
	if !e.statusMessageEnabled(p, plain) || e.statusMessageEnabled(p, quiet) {
		t.Fatal("project-on precedence wrong")
	}
	// Platform option overrides project, channel overrides platform.
	p.pref = &off
	if e.statusMessageEnabled(p, plain) || !e.statusMessageEnabled(p, loud) {
		t.Fatal("platform/channel precedence wrong")
	}
	p.pref = &on
	e.SetStatusMessageConfig(StatusMessageCfg{Enabled: false})
	if !e.statusMessageEnabled(p, plain) || e.statusMessageEnabled(p, quiet) {
		t.Fatal("platform-on precedence wrong")
	}
}

func TestProcessInteractiveEvents_StatusMessageOffKeepsToolMessages(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "full", ThinkingMessages: true, ToolMessages: true})
	sessionKey := "fake:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	as := newControllableSession("s1")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx-1"}
	e.interactiveStates[sessionKey] = state

	as.events <- Event{Type: EventToolUse, ToolName: "bash", ToolInput: "ls"}
	as.events <- Event{Type: EventResult, Content: "done", Done: true}
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, nil)

	starts, _, _ := p.snapshot()
	for _, s := range starts {
		if strings.Contains(s, "Working") {
			t.Fatalf("status message sent while disabled: %q", s)
		}
	}
	if len(p.getSent()) < 2 && len(starts) == 0 {
		t.Fatalf("tool message missing: sent=%v starts=%v", p.getSent(), starts)
	}
}

func runGapTurn(t *testing.T, status bool, preview bool) (*statusFakePlatform, []string) {
	t.Helper()
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet"})
	e.SetStatusMessageConfig(StatusMessageCfg{Enabled: status, Interval: time.Hour})
	if !preview {
		e.SetStreamPreviewCfg(StreamPreviewCfg{Enabled: false})
	}
	sessionKey := "fake:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	as := newControllableSession("s1")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx-1"}
	e.interactiveStates[sessionKey] = state
	go func() {
		as.events <- Event{Type: EventText, Content: "First sentence of narration here."}
		for i := 0; i < 6; i++ {
			as.events <- Event{Type: EventThinking, Content: "hmm"}
			as.events <- Event{Type: EventToolUse, ToolName: "bash", ToolInput: "ls"}
		}
		as.events <- Event{Type: EventText, Content: "Second sentence of narration here."}
		as.events <- Event{Type: EventToolUse, ToolName: "bash", ToolInput: "pwd"}
		as.events <- Event{Type: EventResult, Content: "", Done: true}
	}()
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, nil)
	starts, edits, _ := p.snapshot()
	all := append(append(append([]string{}, starts...), edits...), p.getSent()...)
	return p, all
}

func TestQuietTurn_NoStackedBlankLines(t *testing.T) {
	for _, tc := range []struct{ status, preview bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
		_, all := runGapTurn(t, tc.status, tc.preview)
		seen := false
		for _, m := range all {
			if strings.Contains(m, "\n\n\n") {
				t.Fatalf("status=%v preview=%v: stacked blank lines in %q", tc.status, tc.preview, m)
			}
			if strings.Contains(m, "First sentence of narration here.\n\nSecond sentence") {
				seen = true
			}
		}
		if !seen && (tc.status || tc.preview) {
			t.Fatalf("status=%v preview=%v: narration not joined by one blank line: %q", tc.status, tc.preview, all)
		}
	}
}

func TestStreamPreviewAppendSeparatorIdempotent(t *testing.T) {
	sp := &streamPreview{cfg: StreamPreviewCfg{Enabled: true}, fullText: "abc"}
	if !sp.appendSeparator("\n\n") {
		t.Fatal("first separator should append")
	}
	for i := 0; i < 5; i++ {
		if sp.appendSeparator("\n\n") {
			t.Fatal("repeated separator should be skipped")
		}
	}
	if sp.fullText != "abc\n\n" {
		t.Fatalf("fullText = %q", sp.fullText)
	}
}
