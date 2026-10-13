package acp

import (
	"encoding/json"
	"testing"
)

// Fixtures captured from a standalone grok agent ACP stdio session
// (/workspace/steer-test/trace-interject.jsonl), redacted of secrets.

const fixtureSessionNew = `{"sessionId": "01a134fe-7341-7750-af75-a2f1b557e3eb", "models": {"currentModelId": "grok-4.6-xhigh", "availableModels": [{"modelId": "big-pickle", "name": "big-pickle", "_meta": {"totalContextTokens": 256000, "agentType": "grok-build-plan"}}, {"modelId": "claude-haiku-4-5", "name": "claude-haiku-4-5", "_meta": {"totalContextTokens": 256000, "agentType": "grok-build-plan"}}, {"modelId": "claude-opus-4-6", "name": "claude-opus-4-6", "_meta": {"totalContextTokens": 256000, "agentType": "grok-build-plan"}}]}}`

const fixtureResponseCompleted = `{"sessionId": "01a134fe-7341-7750-af75-a2f1b557e3eb", "update": {"sessionUpdate": "response_completed", "usage": {"input_tokens": 14959, "output_tokens": 195, "cache_read_input_tokens": 3712, "cache_creation_input_tokens": 0, "reasoning_tokens": 0}}}`

const fixtureTurnCompleted = `{"sessionId": "01a134fe-7341-7750-af75-a2f1b557e3eb", "update": {"sessionUpdate": "turn_completed", "prompt_id": "7ae43a33-120f-45cd-93ae-a9b974ec5116", "stop_reason": "end_turn", "usage": {"inputTokens": 116147, "outputTokens": 499, "totalTokens": 116646, "cachedReadTokens": 68864, "cacheCreationTokens": 0, "reasoningTokens": 0, "modelCalls": 6, "apiDurationMs": 49515, "numTurns": 6}, "elapsed_ms": 89726}}`

const fixturePromptResult = `{"stopReason": "end_turn", "_meta": {"sessionId": "01a134fe-7341-7750-af75-a2f1b557e3eb", "promptId": "7ae43a33-120f-45cd-93ae-a9b974ec5116", "totalTokens": 19729, "inputTokens": 19592, "outputTokens": 137, "cachedReadTokens": 19584, "modelId": "grok-4.6-xhigh", "usage": {"inputTokens": 116147, "outputTokens": 499, "totalTokens": 116646, "cachedReadTokens": 68864, "cacheCreationTokens": 0, "reasoningTokens": 0, "modelCalls": 6, "apiDurationMs": 49515, "numTurns": 6}}}`

func TestParseUsageUpdate(t *testing.T) {
	used, size, ok := parseUsageUpdate(json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":53000,"size":200000,"cost":{"amount":0.045,"currency":"USD"}}}`))
	if !ok || used != 53000 || size != 200000 {
		t.Fatalf("got %d %d %v", used, size, ok)
	}
	if _, _, ok := parseUsageUpdate(json.RawMessage(`{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"x"}}}`)); ok {
		t.Fatal("non-usage update accepted")
	}
}

func TestContextWindowFromModels_SessionNew(t *testing.T) {
	w, id, ok := contextWindowFromModels(json.RawMessage(fixtureSessionNew))
	if !ok {
		t.Fatal("expected window")
	}
	// Fixture currentModelId at session/new time may be grok-4 (200k) before set_model.
	if w <= 0 || id == "" {
		t.Fatalf("window=%d model=%q", w, id)
	}
}

func TestContextWindowFromModels_FlatUpdate(t *testing.T) {
	raw := `{"currentModelId":"grok-4.6-xhigh","availableModels":[{"modelId":"grok-4.6-xhigh","_meta":{"totalContextTokens":256000}},{"modelId":"other","_meta":{"totalContextTokens":100000}}]}`
	w, id, ok := contextWindowFromModels(json.RawMessage(raw))
	if !ok || w != 256000 || id != "grok-4.6-xhigh" {
		t.Fatalf("got %d %q %v", w, id, ok)
	}
}

func TestAbsorbGrokResponseCompleted(t *testing.T) {
	s := &acpSession{}
	s.setContextWindow(256000)
	s.maybeAbsorbGrokUsageNotification(json.RawMessage(fixtureResponseCompleted))
	u := s.GetContextUsage()
	if u == nil {
		t.Fatal("nil usage")
	}
	// fixture: input_tokens=14959 + cache_read=3712 = 18671
	if u.UsedTokens != 14959+3712 {
		t.Fatalf("used=%d", u.UsedTokens)
	}
	if u.ContextWindow != 256000 {
		t.Fatalf("window=%d", u.ContextWindow)
	}
}

func TestAbsorbGrokTurnCompleted(t *testing.T) {
	s := &acpSession{}
	s.maybeAbsorbGrokUsageNotification(json.RawMessage(fixtureTurnCompleted))
	u := s.GetContextUsage()
	if u == nil {
		t.Fatal("nil")
	}
	// totalTokens - outputTokens ≈ last-call context
	want := 116646 - 499
	if u.UsedTokens != want {
		t.Fatalf("used=%d want %d", u.UsedTokens, want)
	}
}

func TestParsePromptUsage_GrokMeta(t *testing.T) {
	pu := parsePromptUsage("s", json.RawMessage(fixturePromptResult))
	if pu.Source != "meta.usage" {
		t.Fatalf("source=%q", pu.Source)
	}
	if pu.TurnTokens != 116646 {
		t.Fatalf("turn=%d", pu.TurnTokens)
	}
	if pu.CtxUsed != 19729 {
		t.Fatalf("ctx=%d", pu.CtxUsed)
	}
}

func TestParsePromptUsage_StandardUsage(t *testing.T) {
	pu := parsePromptUsage("s", json.RawMessage(`{"stopReason":"end_turn","usage":{"totalTokens":53000,"inputTokens":35000,"outputTokens":12000,"cachedReadTokens":5000}}`))
	if pu.Source != "usage" || pu.TurnTokens != 53000 {
		t.Fatalf("%+v", pu)
	}
}

func TestParsePromptUsage_Absent(t *testing.T) {
	pu := parsePromptUsage("s", json.RawMessage(`{"stopReason":"end_turn"}`))
	if pu.Source != "absent" || pu.TurnTokens != 0 {
		t.Fatalf("%+v", pu)
	}
}

func TestAcpSessionGetContextUsage_UsageUpdate(t *testing.T) {
	s := &acpSession{}
	if s.GetContextUsage() != nil {
		t.Fatal("want nil before any usage")
	}
	s.maybeAbsorbUsageUpdate(json.RawMessage(`{"update":{"sessionUpdate":"usage_update","used":48000,"size":500000}}`))
	u := s.GetContextUsage()
	if u == nil || u.UsedTokens != 48000 || u.ContextWindow != 500000 {
		t.Fatalf("%+v", u)
	}
	u.UsedTokens = 1
	if s.GetContextUsage().UsedTokens != 48000 {
		t.Fatal("must return a copy")
	}
}

func TestMapSessionUpdate_UsageUpdateEmitsNoText(t *testing.T) {
	evs := mapSessionUpdate("s", json.RawMessage(`{"update":{"sessionUpdate":"usage_update","used":1,"size":2}}`))
	for _, ev := range evs {
		if ev.Content != "" {
			t.Fatalf("usage_update leaked: %+v", ev)
		}
	}
}

func TestPromptUsageAppliedToSession(t *testing.T) {
	s := &acpSession{}
	s.setContextWindow(256000)
	pu := parsePromptUsage("s", json.RawMessage(fixturePromptResult))
	if pu.CtxUsed > 0 {
		s.recordContextUsed(pu.CtxUsed, "prompt_meta.totalTokens")
	}
	u := s.GetContextUsage()
	if u == nil || u.UsedTokens != 19729 || u.ContextWindow != 256000 {
		t.Fatalf("%+v", u)
	}
	if pu.TurnTokens != 116646 {
		t.Fatalf("turn %d", pu.TurnTokens)
	}
}
