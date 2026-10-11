package acp

import (
	"reflect"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestInjectChannelProfileArgs(t *testing.T) {
	base := []string{"/workspace/grok-acp-filter.py", "agent", "--no-leader", "--always-approve", "stdio"}
	if got := injectChannelProfileArgs(base, "", ""); !reflect.DeepEqual(got, base) {
		t.Fatalf("empty profile must keep argv, got %v", got)
	}
	got := injectChannelProfileArgs(base, "/d/p.md", "/d/plug")
	want := []string{"/workspace/grok-acp-filter.py", "agent", "--no-leader", "--always-approve", "--agent-profile", "/d/p.md", "--plugin-dir", "/d/plug", "stdio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	// Model/effort injection still lands before stdio.
	got = injectReasoningEffortArgs(injectModelArgs(got, "grok-4.7"), "high")
	if got[len(got)-1] != "stdio" || got[len(got)-3] != "--reasoning-effort" {
		t.Fatalf("ordering broken: %v", got)
	}
	if got := injectChannelProfileArgs([]string{"agent"}, "/p.md", ""); !reflect.DeepEqual(got, []string{"agent", "--agent-profile", "/p.md"}) {
		t.Fatalf("no-stdio append = %v", got)
	}
}

func TestUsesGrokChannelFlags(t *testing.T) {
	cases := []struct {
		a    *Agent
		want bool
	}{
		{&Agent{cmd: "/usr/bin/python3", args: []string{"/workspace/grok-acp-filter.py", "agent", "stdio"}}, true},
		{&Agent{cmd: "/home/box/.grok/bin/grok", args: []string{"agent", "stdio"}}, true},
		{&Agent{cmd: "cursor-agent", args: []string{"acp"}}, false},
		{&Agent{cmd: "cursor-agent", channelProfileMode: "grok"}, true},
		{&Agent{cmd: "grok", channelProfileMode: "prompt"}, false},
	}
	for i, c := range cases {
		if got := c.a.usesGrokChannelFlags(); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestSetChannelProfile_ConsumedOnce(t *testing.T) {
	a := &Agent{cmd: "grok"}
	a.SetChannelProfile(&core.ChannelSessionProfile{Match: "discord:1"})
	a.mu.Lock()
	got := a.pendingChannel
	a.mu.Unlock()
	if got == nil || got.Match != "discord:1" {
		t.Fatalf("pending = %+v", got)
	}
	a.SetChannelProfile(nil)
	if a.pendingChannel != nil {
		t.Fatal("nil must clear the staged profile")
	}
}
