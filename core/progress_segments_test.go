package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// segFakePlatform records Sends with their reply context and every preview
// message (start + edits) so tests can check segment delivery.
type segFakePlatform struct {
	stubPlatformEngine
	mu       sync.Mutex
	sends    []segSend
	previews []*segPreview
}

type segSend struct {
	ctx     any
	content string
}

type segPreview struct {
	ctx  any
	text string // latest content
}

func (p *segFakePlatform) Send(_ context.Context, rctx any, content string) error {
	p.mu.Lock()
	p.sends = append(p.sends, segSend{rctx, content})
	p.mu.Unlock()
	return p.stubPlatformEngine.Send(context.Background(), rctx, content)
}

func (p *segFakePlatform) SendPreviewStart(_ context.Context, rctx any, content string) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pv := &segPreview{ctx: rctx, text: content}
	p.previews = append(p.previews, pv)
	return pv, nil
}

func (p *segFakePlatform) UpdateMessage(_ context.Context, h any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pv, ok := h.(*segPreview); ok {
		pv.text = content
	}
	return nil
}

func (p *segFakePlatform) DeletePreviewMessage(_ context.Context, h any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pv, ok := h.(*segPreview); ok {
		pv.text = "<deleted>"
	}
	return nil
}

// ProgressReplyContext marks progress contexts so tests can tell them apart.
func (p *segFakePlatform) ProgressReplyContext(rctx any) any {
	return fmt.Sprintf("progress:%v", rctx)
}

func (p *segFakePlatform) snapshot() ([]segSend, []segPreview) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := append([]segSend(nil), p.sends...)
	pv := make([]segPreview, 0, len(p.previews))
	for _, x := range p.previews {
		pv = append(pv, *x)
	}
	return s, pv
}

func runSegTurn(t *testing.T, p *segFakePlatform, display DisplayCfg, preview bool, status bool, events []Event) {
	t.Helper()
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(display)
	if preview {
		e.SetStreamPreviewCfg(StreamPreviewCfg{Enabled: true, IntervalMs: 1, MinDeltaChars: 1, MaxChars: 2000})
	} else {
		e.SetStreamPreviewCfg(StreamPreviewCfg{Enabled: false})
	}
	e.SetStatusMessageConfig(StatusMessageCfg{Enabled: status, Interval: time.Hour})
	sessionKey := "seg:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	as := newControllableSession("s1")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state
	go func() {
		for _, ev := range events {
			as.events <- ev
			time.Sleep(5 * time.Millisecond)
		}
	}()
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, "ctx")
}

var compactHidden = DisplayCfg{Mode: "compact", ThinkingMessages: false, ToolMessages: false}

func segEvents() []Event {
	return []Event{
		{Type: EventText, Content: "Segment A text."},
		{Type: EventThinking, Content: "hmm"},
		{Type: EventToolUse, ToolName: "bash", ToolInput: "ls"},
		{Type: EventText, Content: "Segment B text."},
		{Type: EventToolUse, ToolName: "bash", ToolInput: "pwd"},
		{Type: EventText, Content: "Final C answer."},
		{Type: EventResult, Content: "", Done: true},
	}
}

// Each process-text segment streams as its own preview; nothing is merged
// and no segment degrades into a plain Send.
func TestCompactSegments_RearmPreviewPerSegment(t *testing.T) {
	p := &segFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "seg"}}
	runSegTurn(t, p, compactHidden, true, false, segEvents())
	sends, previews := p.snapshot()

	var texts []string
	for _, pv := range previews {
		texts = append(texts, pv.text)
	}
	want := []string{"Segment A text.", "Segment B text.", "Final C answer."}
	if len(previews) != 3 {
		t.Fatalf("previews = %q, want 3 separate segment previews", texts)
	}
	for i, w := range want {
		if !strings.Contains(previews[i].text, w) || strings.Count(previews[i].text, "Segment")+strings.Count(previews[i].text, "Final") != 1 {
			t.Fatalf("preview[%d] = %q, want only %q", i, previews[i].text, w)
		}
	}
	for _, s := range sends {
		if strings.Contains(s.content, "Segment") || strings.Contains(s.content, "Final C") {
			t.Fatalf("segment re-sent via Send: %+v (previews %q)", s, texts)
		}
	}
}

// Process segments use the progress reply context; only the final answer
// uses the original reply context (and thus can carry a reply reference).
func TestCompactSegments_ReplyReferenceOnlyOnFinal(t *testing.T) {
	p := &segFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "seg"}}
	runSegTurn(t, p, compactHidden, false, false, segEvents())
	sends, _ := p.snapshot()
	if len(sends) != 3 {
		t.Fatalf("sends = %+v, want 3", sends)
	}
	for i, s := range sends[:2] {
		if s.ctx != "progress:ctx" {
			t.Fatalf("segment %d ctx = %v, want progress ctx (%q)", i, s.ctx, s.content)
		}
	}
	last := sends[2]
	if last.ctx != "ctx" || !strings.Contains(last.content, "Final C answer.") {
		t.Fatalf("final send = %+v, want original ctx", last)
	}
}

// Tool / thinking messages (shown) are progress too.
func TestShownToolMessages_UseProgressReplyCtx(t *testing.T) {
	p := &segFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "seg"}}
	runSegTurn(t, p, DisplayCfg{Mode: "full", ThinkingMessages: true, ToolMessages: true}, false, false, segEvents())
	sends, _ := p.snapshot()
	if len(sends) < 2 {
		t.Fatalf("sends = %+v", sends)
	}
	for _, s := range sends[:len(sends)-1] {
		if s.ctx != "progress:ctx" {
			t.Fatalf("progress message with final ctx: %+v", s)
		}
	}
	if last := sends[len(sends)-1]; last.ctx != "ctx" {
		t.Fatalf("final = %+v", last)
	}
}

// Enabling the status message adds only the status message; segment and
// final delivery are identical to status off.
func TestStatusMessage_DoesNotAlterDelivery(t *testing.T) {
	for _, display := range []DisplayCfg{compactHidden, {Mode: "full", ThinkingMessages: true, ToolMessages: true}} {
		for _, preview := range []bool{false, true} {
			off := &segFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "seg"}}
			runSegTurn(t, off, display, preview, false, segEvents())
			on := &segFakePlatform{stubPlatformEngine: stubPlatformEngine{n: "seg"}}
			runSegTurn(t, on, display, preview, true, segEvents())

			offSends, offPv := off.snapshot()
			onSends, onPv := on.snapshot()
			if fmt.Sprint(offSends) != fmt.Sprint(onSends) {
				t.Fatalf("mode=%s preview=%v sends differ:\noff=%+v\non =%+v", display.Mode, preview, offSends, onSends)
			}
			// Status adds exactly one extra preview (the status message).
			var onNonStatus []segPreview
			statusCount := 0
			for _, pv := range onPv {
				if strings.HasPrefix(pv.text, "⏳") || strings.HasPrefix(pv.text, "✅") || strings.HasPrefix(pv.text, "⏹") || strings.HasPrefix(pv.text, "⚠️") {
					statusCount++
					continue
				}
				onNonStatus = append(onNonStatus, pv)
			}
			if statusCount != 1 {
				t.Fatalf("mode=%s preview=%v: status previews = %d, want 1 (%+v)", display.Mode, preview, statusCount, onPv)
			}
			if fmt.Sprint(onNonStatus) != fmt.Sprint(offPv) {
				t.Fatalf("mode=%s preview=%v previews differ:\noff=%+v\non =%+v", display.Mode, preview, offPv, onNonStatus)
			}
		}
	}
}

func (p *segFakePlatform) KeepPreviewOnFinish() bool { return true }
