package acp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func writeJSONRPC(t *testing.T, w *io.PipeWriter, method, params string) {
	t.Helper()
	line := fmt.Sprintf(`{"jsonrpc":"2.0","method":%s,"params":%s}`+"\n", jsonQuote(method), params)
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write notification: %v", err)
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func sessionUpdateParams(update string) string {
	return `{"sessionId":"test-session-id","update":` + update + `}`
}

func agentTextUpdate(text string) string {
	return fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%s}}`, jsonQuote(text))
}

func turnCompletedUpdate() string {
	return `{"sessionUpdate":"turn_completed","stop_reason":"end_turn","usage":{"inputTokens":10,"outputTokens":4,"totalTokens":14}}`
}

func responseCompletedUpdate() string {
	return `{"sessionUpdate":"response_completed","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":0}}`
}

func collectEvents(t *testing.T, s *acpSession, want int, timeout time.Duration) []core.Event {
	t.Helper()
	var got []core.Event
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev := <-s.Events():
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

func assertNoEvent(t *testing.T, s *acpSession, wait time.Duration) {
	t.Helper()
	select {
	case ev := <-s.Events():
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(wait):
	}
}

func shortenBackgroundTimers(t *testing.T, idle, settle time.Duration) {
	t.Helper()
	oldIdle, oldSettle := backgroundTurnIdle, cancelSettleIdle
	backgroundTurnIdle = idle
	cancelSettleIdle = settle
	t.Cleanup(func() {
		backgroundTurnIdle = oldIdle
		cancelSettleIdle = oldSettle
	})
}

func drainTransport(t *testing.T, r *io.PipeReader) {
	t.Helper()
	go func() { _, _ = io.Copy(io.Discard, r) }()
}

// A single out-of-turn message from an agent that never emits
// turn_completed (e.g. kimi acp's deferred "Compaction completed")
// must still be closed with one synthetic EventResult after idle.
func TestSession_OutOfTurnTextGetsSyntheticResult(t *testing.T) {
	shortenBackgroundTimers(t, 60*time.Millisecond, 60*time.Millisecond)
	s, wResp, _ := newTestSession(t, nil)

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate("Compaction completed.")))

	got := collectEvents(t, s, 2, 2*time.Second)
	if len(got) < 2 {
		t.Fatalf("want EventText + synthetic EventResult, got %+v", got)
	}
	if got[0].Type != core.EventText || got[0].Content != "Compaction completed." {
		t.Fatalf("first event = %+v", got[0])
	}
	if got[1].Type != core.EventResult || !got[1].Done {
		t.Fatalf("second event = %+v, want synthetic EventResult", got[1])
	}
	assertNoEvent(t, s, 80*time.Millisecond)
}

// Streaming out-of-turn agent_message_chunk deltas are one turn: many
// EventText, exactly one EventResult, and only after turn_completed.
func TestSession_OutOfTurnStreamAggregatesToOneResult(t *testing.T) {
	shortenBackgroundTimers(t, 80*time.Millisecond, 80*time.Millisecond)
	s, wResp, _ := newTestSession(t, nil)

	chunks := []string{"跑", "完了", "。", "剩余", "166", "个", "账号"}
	for _, c := range chunks {
		writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate(c)))
	}

	got := collectEvents(t, s, len(chunks), 2*time.Second)
	if len(got) != len(chunks) {
		t.Fatalf("want %d EventText, got %d: %+v", len(chunks), len(got), got)
	}
	var b strings.Builder
	for i, ev := range got {
		if ev.Type != core.EventText || ev.Content != chunks[i] {
			t.Fatalf("event %d = %+v, want EventText %q", i, ev, chunks[i])
		}
		b.WriteString(ev.Content)
	}
	if b.String() != "跑完了。剩余166个账号" {
		t.Fatalf("aggregated text = %q", b.String())
	}

	// Mid-stream quiet must not close a Grok-style turn after a per-call signal.
	writeJSONRPC(t, wResp, "_x.ai/session_notification", sessionUpdateParams(responseCompletedUpdate()))
	assertNoEvent(t, s, 150*time.Millisecond)

	writeJSONRPC(t, wResp, "_x.ai/session_notification", sessionUpdateParams(turnCompletedUpdate()))
	got = collectEvents(t, s, 1, 2*time.Second)
	if len(got) != 1 || got[0].Type != core.EventResult || !got[0].Done {
		t.Fatalf("want one EventResult after turn_completed, got %+v", got)
	}
	assertNoEvent(t, s, 80*time.Millisecond)
}

// response_completed is per model call, not the end of a background turn.
func TestSession_ResponseCompletedDoesNotCloseTurn(t *testing.T) {
	shortenBackgroundTimers(t, 80*time.Millisecond, 80*time.Millisecond)
	s, wResp, _ := newTestSession(t, nil)

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate("hello")))
	got := collectEvents(t, s, 1, 2*time.Second)
	if len(got) != 1 || got[0].Type != core.EventText {
		t.Fatalf("want EventText, got %+v", got)
	}
	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(responseCompletedUpdate()))
	assertNoEvent(t, s, 150*time.Millisecond)

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate(" world")))
	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(turnCompletedUpdate()))
	got = collectEvents(t, s, 2, 2*time.Second)
	if len(got) != 2 || got[0].Type != core.EventText || got[0].Content != " world" {
		t.Fatalf("want trailing EventText then EventResult, got %+v", got)
	}
	if got[1].Type != core.EventResult || !got[1].Done {
		t.Fatalf("want EventResult, got %+v", got[1])
	}
}

// Whitespace-only out-of-turn chunks must not open a turn or emit EventResult.
func TestSession_OutOfTurnWhitespaceGetsNoSyntheticResult(t *testing.T) {
	shortenBackgroundTimers(t, 60*time.Millisecond, 60*time.Millisecond)
	s, wResp, _ := newTestSession(t, nil)

	for _, chunk := range []string{"", " ", "\n", "\t"} {
		writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate(chunk)))
	}
	// Empty mapAgentMessageChunk emits nothing. Whitespace-only chunks may
	// surface as EventText, but must not open/close a synthetic turn.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == core.EventResult {
				t.Fatalf("unexpected EventResult for whitespace-only chunks: %+v", ev)
			}
			if ev.Type == core.EventText && strings.TrimSpace(ev.Content) != "" {
				t.Fatalf("unexpected visible text: %+v", ev)
			}
		case <-deadline:
			goto afterWS
		}
	}
afterWS:
	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(turnCompletedUpdate()))
	deadline = time.After(150 * time.Millisecond)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == core.EventResult {
				t.Fatalf("turn_completed must not emit EventResult without visible text: %+v", ev)
			}
		case <-deadline:
			return
		}
	}
}

// Two background turns separated by turn_completed stay two turns.
func TestSession_SecondBackgroundTurnAfterTurnCompletedIsSeparate(t *testing.T) {
	shortenBackgroundTimers(t, 80*time.Millisecond, 80*time.Millisecond)
	s, wResp, _ := newTestSession(t, nil)

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate("first")))
	writeJSONRPC(t, wResp, "_x.ai/session_notification", sessionUpdateParams(turnCompletedUpdate()))
	got := collectEvents(t, s, 2, 2*time.Second)
	if len(got) != 2 || got[0].Content != "first" || got[1].Type != core.EventResult {
		t.Fatalf("first turn = %+v", got)
	}

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate("second")))
	writeJSONRPC(t, wResp, "_x.ai/session_notification", sessionUpdateParams(turnCompletedUpdate()))
	got = collectEvents(t, s, 2, 2*time.Second)
	if len(got) != 2 || got[0].Content != "second" || got[1].Type != core.EventResult {
		t.Fatalf("second turn = %+v", got)
	}
}

// After CancelTurn, leftover stream chunks must be dropped (no EventText
// and no synthetic EventResult). Once the cancel window settles, a new
// background message is relayed again.
func TestSession_CancelDropsLateChunksThenNewBackgroundWorks(t *testing.T) {
	shortenBackgroundTimers(t, 60*time.Millisecond, 80*time.Millisecond)
	s, wResp, rReq := newTestSession(t, nil)
	drainTransport(t, rReq)

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate("跑")))
	got := collectEvents(t, s, 1, 2*time.Second)
	if len(got) != 1 || got[0].Content != "跑" {
		t.Fatalf("want initial EventText, got %+v", got)
	}

	if err := s.CancelTurn(); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}

	for _, c := range []string{"完了", "。", "%", "ers"} {
		writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate(c)))
	}
	writeJSONRPC(t, wResp, "_x.ai/session_notification", sessionUpdateParams(turnCompletedUpdate()))
	assertNoEvent(t, s, 200*time.Millisecond)

	// After settle, a genuine new background turn must work.
	time.Sleep(120 * time.Millisecond)
	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate("new result")))
	writeJSONRPC(t, wResp, "_x.ai/session_notification", sessionUpdateParams(turnCompletedUpdate()))
	got = collectEvents(t, s, 2, 2*time.Second)
	if len(got) != 2 || got[0].Content != "new result" || got[1].Type != core.EventResult {
		t.Fatalf("want new background turn after settle, got %+v", got)
	}
}

// Text chunks of a foreground turn (prompt RPC in flight) must NOT get a
// synthetic EventResult — the turn's own result follows when the RPC
// returns.
func TestSession_InFlightTextGetsNoSyntheticResult(t *testing.T) {
	shortenBackgroundTimers(t, 60*time.Millisecond, 60*time.Millisecond)
	s, wResp, _ := newTestSession(t, nil)
	s.promptInFlight.Store(1)
	defer s.promptInFlight.Store(0)

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(agentTextUpdate("streamed chunk")))

	select {
	case ev := <-s.Events():
		if ev.Type != core.EventText {
			t.Fatalf("first event = %+v, want EventText", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for EventText")
	}

	assertNoEvent(t, s, 200*time.Millisecond)
}

// Non-text out-of-turn updates (e.g. tool_call progress with no visible
// text) must not produce a synthetic result either.
func TestSession_OutOfTurnNonTextGetsNoSyntheticResult(t *testing.T) {
	shortenBackgroundTimers(t, 60*time.Millisecond, 60*time.Millisecond)
	s, wResp, _ := newTestSession(t, nil)

	writeJSONRPC(t, wResp, "session/update", sessionUpdateParams(`{"sessionUpdate":"tool_call_update","toolCallId":"tc1","status":"in_progress"}`))

	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == core.EventResult {
				t.Fatalf("unexpected synthetic EventResult: %+v", ev)
			}
		case <-deadline:
			return
		}
	}
}
