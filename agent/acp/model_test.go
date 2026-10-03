package acp

import (
	"context"
	"reflect"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestInjectModelArgs_beforeStdio(t *testing.T) {
	got := injectModelArgs([]string{"/filter.py", "agent", "--always-approve", "stdio"}, "grok-4.6")
	want := []string{"/filter.py", "agent", "--always-approve", "-m", "grok-4.6", "stdio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestInjectModelArgs_afterAgentWhenNoStdio(t *testing.T) {
	got := injectModelArgs([]string{"agent", "--always-approve"}, "grok-4.5")
	want := []string{"agent", "-m", "grok-4.5", "--always-approve"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestInjectModelArgs_emptyModel(t *testing.T) {
	in := []string{"agent", "stdio"}
	got := injectModelArgs(in, "")
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %#v, want %#v", got, in)
	}
}

func TestIsGrokChatModel(t *testing.T) {
	cases := map[string]bool{
		"grok-4.6":               true,
		"grok-composer-2.5-fast": true,
		"grok-build-0.1":         true,
		"grok-chat-fast":         true,
		"grok-imagine-image":     false,
		"grok-voice-latest":      false,
		"grok-stt":               false,
		"":                       false,
	}
	for id, want := range cases {
		if got := isGrokChatModel(id); got != want {
			t.Errorf("isGrokChatModel(%q)=%v, want %v", id, got, want)
		}
	}
}

func TestParseGrokModelsCLI(t *testing.T) {
	out := `Default model: grok-4.5

Available models:
  * grok-4.5 (default)
  - grok-4.6
  - grok-imagine-image
  - grok-chat-fast
`
	got := parseGrokModelsCLI(out)
	names := make([]string, len(got))
	for i, m := range got {
		names[i] = m.Name
	}
	want := []string{"grok-4.5", "grok-4.6", "grok-chat-fast"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("got %#v, want %#v", names, want)
	}
}

func TestAgent_ModelSwitcher(t *testing.T) {
	a, err := New(map[string]any{
		"command": "true",
		"model":   "grok-4.5",
		"args":    []any{"agent", "--always-approve", "stdio"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms, ok := a.(core.ModelSwitcher)
	if !ok {
		t.Fatal("Agent does not implement core.ModelSwitcher")
	}
	if got := ms.GetModel(); got != "grok-4.5" {
		t.Fatalf("GetModel=%q, want grok-4.5", got)
	}
	ms.SetModel("grok-4.6")
	if got := ms.GetModel(); got != "grok-4.6" {
		t.Fatalf("GetModel after SetModel=%q, want grok-4.6", got)
	}
	rs, ok := a.(core.ReasoningEffortSwitcher)
	if !ok {
		t.Fatal("Agent does not implement core.ReasoningEffortSwitcher")
	}
	if got := rs.AvailableReasoningEfforts(); !reflect.DeepEqual(got, []string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Fatalf("AvailableReasoningEfforts=%v", got)
	}
	rs.SetReasoningEffort("high")
	if got := rs.GetReasoningEffort(); got != "high" {
		t.Fatalf("GetReasoningEffort=%q, want high", got)
	}
	rs.SetReasoningEffort("none")
	if got := rs.GetReasoningEffort(); got != "" {
		t.Fatalf("GetReasoningEffort after none=%q, want empty", got)
	}
	_ = acpFallbackModels()
	_ = a.(*Agent).AvailableModels(context.Background())
}
