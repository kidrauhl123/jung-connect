package acp

import (
	"encoding/json"
	"testing"
)

func TestParseUsageUpdate(t *testing.T) {
	used, size, ok := parseUsageUpdate(json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":53000,"size":200000,"cost":{"amount":0.045,"currency":"USD"}}}`))
	if !ok || used != 53000 || size != 200000 {
		t.Fatalf("got %d %d %v", used, size, ok)
	}
	if _, _, ok := parseUsageUpdate(json.RawMessage(`{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"x"}}}`)); ok {
		t.Fatal("non-usage update accepted")
	}
}

func TestAcpSessionGetContextUsage(t *testing.T) {
	s := &acpSession{}
	if s.GetContextUsage() != nil {
		t.Fatal("want nil before any usage_update")
	}
	s.maybeAbsorbUsageUpdate(json.RawMessage(`{"update":{"sessionUpdate":"usage_update","used":48000,"size":500000}}`))
	u := s.GetContextUsage()
	if u == nil || u.UsedTokens != 48000 || u.ContextWindow != 500000 {
		t.Fatalf("usage = %+v", u)
	}
	u.UsedTokens = 1
	if s.GetContextUsage().UsedTokens != 48000 {
		t.Fatal("GetContextUsage must return a copy")
	}
}

func TestParsePromptUsage(t *testing.T) {
	cases := map[string]int{
		`{"stopReason":"end_turn","usage":{"totalTokens":53000,"inputTokens":35000,"outputTokens":12000,"cachedReadTokens":5000}}`: 53000,
		`{"stopReason":"end_turn","usage":{"inputTokens":1000,"outputTokens":234}}`:                                                1234,
		`{"stopReason":"end_turn"}`: 0,
		``:                          0,
		`not json`:                  0,
	}
	for in, want := range cases {
		if got := parsePromptUsage("s", json.RawMessage(in)); got != want {
			t.Errorf("parsePromptUsage(%s) = %d, want %d", in, got, want)
		}
	}
}

func TestMapSessionUpdate_UsageUpdateEmitsNoText(t *testing.T) {
	evs := mapSessionUpdate("s", json.RawMessage(`{"update":{"sessionUpdate":"usage_update","used":1,"size":2}}`))
	for _, ev := range evs {
		if ev.Content != "" {
			t.Fatalf("usage_update leaked as event: %+v", ev)
		}
	}
}
