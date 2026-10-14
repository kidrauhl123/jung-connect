package core

import (
	"strings"
	"testing"
)

type liveSwitchSession struct {
	stubAgentSession
	models  []string
	efforts []string
	reject  bool
}

func (s *liveSwitchSession) SetLiveModel(m string) bool {
	s.models = append(s.models, m)
	return !s.reject
}
func (s *liveSwitchSession) SetLiveReasoningEffort(e string) bool {
	s.efforts = append(s.efforts, e)
	return !s.reject
}

func TestApplyLiveSessionChange(t *testing.T) {
	e := newTestEngine()
	key := "test:chan:user"
	as := &liveSwitchSession{}
	e.interactiveStates[e.interactiveKeyForSessionKey(key)] = &interactiveState{agentSession: as}

	if r := e.applyLiveSessionChange(key, e.sessions, "m", liveModelFn("grok-x")); r != liveSwitchApplied {
		t.Fatalf("idle: got %v, want applied", r)
	}
	if len(as.models) != 1 || as.models[0] != "grok-x" {
		t.Fatalf("models = %v", as.models)
	}

	// Busy: deferred, then applied at next turn.
	s := e.sessions.GetOrCreateActive(key)
	if !s.TryLock() {
		t.Fatal("lock")
	}
	if r := e.applyLiveSessionChange(key, e.sessions, "r", liveReasoningFn("high")); r != liveSwitchDeferred {
		t.Fatalf("busy: got %v, want deferred", r)
	}
	if len(as.efforts) != 0 {
		t.Fatal("must not apply mid-turn")
	}
	s.Unlock()
	applyPendingLiveChanges(e.interactiveStates[e.interactiveKeyForSessionKey(key)])
	if len(as.efforts) != 1 || as.efforts[0] != "high" {
		t.Fatalf("efforts = %v", as.efforts)
	}

	as.reject = true
	if r := e.applyLiveSessionChange(key, e.sessions, "m", liveModelFn("y")); !r.needsRestart() {
		t.Fatalf("rejected switch must restart, got %v", r)
	}
	if r := e.applyLiveSessionChange("other", e.sessions, "m", liveModelFn("y")); r != liveSwitchNone || !r.needsRestart() {
		t.Fatalf("no live session: got %v", r)
	}
}

func TestCmdModelSessionSwitchKeepsLiveSession(t *testing.T) {
	e := newTestEngine()
	key := "test:chan:user"
	as := &liveSwitchSession{}
	iKey := e.interactiveKeyForSessionKey(key)
	e.interactiveStates[iKey] = &interactiveState{agentSession: as}
	sess := e.sessions.GetOrCreateActive(key)
	sess.SetAgentSessionID("keep-me", "test")
	if r := e.applyLiveSessionChange(key, e.sessions, "m", liveModelFn("z")); r.needsRestart() {
		t.Fatal("should be applied in place")
	}
	if e.interactiveStates[iKey] == nil || sess.GetAgentSessionID() != "keep-me" {
		t.Fatal("live session / agent session id must be kept")
	}
}

type ctxPlatform struct {
	stubPlatformEngine
	desc string
}

func (p *ctxPlatform) DescribeSessionContext(*Message) string { return p.desc }

func TestSessionContextLine(t *testing.T) {
	p := &ctxPlatform{stubPlatformEngine: stubPlatformEngine{n: "discord"}, desc: "#注册机 (1) · thread 「t」(2)"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	msg := &Message{Platform: "discord", SessionKey: "discord:2:u", ChannelID: "2", ChannelKey: "1"}
	got := e.sessionContextLine(p, msg)
	want := "[context] discord · #注册机 (1) · thread 「t」(2) · binding: none\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	e.SetChannelProfiles([]ChannelProfile{{Match: "discord:1", Prompt: "x"}})
	if got := e.sessionContextLine(p, msg); !strings.Contains(got, "binding: discord:1") {
		t.Fatalf("binding not reported: %q", got)
	}

	// Fallback for platforms without a describer.
	sp := &stubPlatformEngine{n: "telegram"}
	got = e.sessionContextLine(sp, &Message{Platform: "telegram", SessionKey: "telegram:-100:9", ChatName: "Group"})
	if !strings.HasPrefix(got, "[context] telegram · Group (") {
		t.Fatalf("fallback: %q", got)
	}

	e.display.NoContextLine = true
	if e.sessionContextLine(p, msg) != "" {
		t.Fatal("disabled context line must be empty")
	}
}
