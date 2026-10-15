package acp

import "testing"

func TestACPSessionGetModelTracksLive(t *testing.T) {
	s := &acpSession{}
	if s.GetModel() != "" || s.GetReasoningEffort() != "" {
		t.Fatal("expected empty before any model known")
	}
	s.liveModel.Store("grok-4.7")
	s.liveEffort.Store("high")
	if s.GetModel() != "grok-4.7" || s.GetReasoningEffort() != "high" {
		t.Fatalf("got %q %q", s.GetModel(), s.GetReasoningEffort())
	}
}
