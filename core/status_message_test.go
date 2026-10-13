package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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
	if !strings.Contains(edits[0], "3 步") || !strings.Contains(edits[0], "运行 go test ./...") || !strings.Contains(edits[0], "4s") {
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
	if strings.Contains(lastOf(edits), "可能卡住") {
		t.Fatalf("stuck too early: %q", lastOf(edits))
	}
	clk.add(70 * time.Second)
	sm.Tick()
	_, edits, _ = p.snapshot()
	if !strings.Contains(lastOf(edits), "可能卡住") || !strings.Contains(lastOf(edits), "3m 10s 无进展") {
		t.Fatalf("want stuck marker, got %q", lastOf(edits))
	}
	clk.add(2 * time.Second)
	sm.Tool("edit", `{"file_path":"a.go"}`)
	_, edits, _ = p.snapshot()
	if strings.Contains(lastOf(edits), "可能卡住") || !strings.Contains(lastOf(edits), "改 a.go") {
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
	if deletes != 0 || lastOf(edits) != "✅ 完成（2 步，1m 05s）" {
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
	if _, e3, _ := p3.snapshot(); !strings.HasPrefix(lastOf(e3), "✅ 完成") {
		t.Fatalf("fallback summary = %v", e3)
	}
	p4 := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	sm4, _ := newTestStatus(t, p4, StatusMessageCfg{OnDone: StatusOnDoneDelete})
	sm4.Finish(false)
	if _, e4, d4 := p4.snapshot(); d4 != 0 || !strings.HasPrefix(lastOf(e4), "⏹ 已中断") {
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
	if _, edits, _ := p.snapshot(); len(edits) != 1 || !strings.Contains(edits[0], "运行 d") {
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
	cases := map[[2]string]string{
		{"read_file", `{"target_file":"/root/jung-connect/core/upstream.go"}`}: "读 upstream.go",
		{"Read", `{"file_path":"/a/b/c.txt"}`}:                                 "读 c.txt",
		{"search_replace", `{"file_path":"x/engine.go"}`}:                      "改 engine.go",
		{"run_terminal_command", "go test ./...\nmore"}:                        "运行 go test ./...",
		{"grep", `{"pattern":"isGrokChatModel"}`}:                              "搜索 isGrokChatModel",
		{"web_fetch", `{"url":"https://example.com"}`}:                         "查网页 https://example.com",
		{"TodoWrite", `{"todos":[]}`}:                                          "更新计划",
		{"mystery", ""}:                                                        "mystery",
	}
	for in, want := range cases {
		if got := summarizeToolStep(in[0], in[1]); got != want {
			t.Errorf("summarizeToolStep(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
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

func TestProcessInteractiveEvents_StatusMessageReplacesToolMessages(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "full", ThinkingMessages: true, ToolMessages: true})
	e.SetStatusMessageConfig(StatusMessageCfg{Enabled: true, Interval: time.Hour})
	e.SetStreamPreviewCfg(StreamPreviewCfg{Enabled: false})
	sessionKey := "fake:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	as := newControllableSession("s1")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx-1"}
	e.interactiveStates[sessionKey] = state

	as.events <- Event{Type: EventThinking, Content: "let me look"}
	as.events <- Event{Type: EventToolUse, ToolName: "read_file", ToolInput: `{"target_file":"a/upstream.go"}`}
	as.events <- Event{Type: EventText, Content: "Checked the file."}
	as.events <- Event{Type: EventToolUse, ToolName: "grep", ToolInput: `{"pattern":"foo"}`}
	as.events <- Event{Type: EventToolUse, ToolName: "bash", ToolInput: "go test ./..."}
	as.events <- Event{Type: EventText, Content: "All good."}
	as.events <- Event{Type: EventResult, Content: "All good.", Done: true}
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, nil)

	starts, edits, _ := p.snapshot()
	if len(starts) != 1 {
		t.Fatalf("status starts = %v, want exactly one", starts)
	}
	if !strings.HasPrefix(lastOf(edits), "✅ 完成（3 步") {
		t.Fatalf("final status = %q (edits %v)", lastOf(edits), edits)
	}
	sent := p.getSent()
	if len(sent) != 1 {
		t.Fatalf("sent = %#v, want only the final reply", sent)
	}
	if !strings.Contains(sent[0], "Checked the file.") || !strings.Contains(sent[0], "All good.") {
		t.Fatalf("final reply = %q", sent[0])
	}
	for _, s := range sent {
		if strings.Contains(s, "upstream.go") || strings.Contains(s, "let me look") {
			t.Fatalf("tool/thinking leaked into chat: %q", s)
		}
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
		if strings.Contains(s, "处理中") {
			t.Fatalf("status message sent while disabled: %q", s)
		}
	}
	if len(p.getSent()) < 2 && len(starts) == 0 {
		t.Fatalf("tool message missing: sent=%v starts=%v", p.getSent(), starts)
	}
}

func TestProcessInteractiveEvents_StatusMessageWithStreamPreview(t *testing.T) {
	p := &statusFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "fake"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "full", ThinkingMessages: true, ToolMessages: true})
	e.SetStatusMessageConfig(StatusMessageCfg{Enabled: true, Interval: time.Hour})
	sessionKey := "fake:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	as := newControllableSession("s1")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx-1"}
	e.interactiveStates[sessionKey] = state

	as.events <- Event{Type: EventToolUse, ToolName: "bash", ToolInput: "ls"}
	as.events <- Event{Type: EventText, Content: "Here is the answer, long enough to stream as a preview message."}
	as.events <- Event{Type: EventToolUse, ToolName: "bash", ToolInput: "pwd"}
	as.events <- Event{Type: EventResult, Content: "Here is the answer, long enough to stream as a preview message.", Done: true}
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, nil)

	starts, edits, _ := p.snapshot()
	n := 0
	for _, s := range starts {
		if strings.HasPrefix(s, "⏳") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("status starts = %d (%v), want 1", n, starts)
	}
	found := false
	for _, ed := range edits {
		if strings.HasPrefix(ed, "✅ 完成（2 步") {
			found = true
		}
		if strings.Contains(ed, "🔧") {
			t.Fatalf("tool message leaked: %q", ed)
		}
	}
	if !found {
		t.Fatalf("no completion edit: %v", edits)
	}
	for _, s := range p.getSent() {
		if strings.Contains(s, "🔧") || strings.Contains(s, "pwd") {
			t.Fatalf("tool message leaked: %q", s)
		}
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
