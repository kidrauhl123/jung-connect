package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// sessionModelAgent is a ModelSwitcher + ChannelProfileInjector +
// SessionModelSetter that records the effective model of each StartSession
// using the same precedence real agents use: session > channel > global.
type sessionModelAgent struct {
	stubModelModeAgent
	PendingSessionModel
	channel *ChannelSessionProfile
	started []string
}

func (a *sessionModelAgent) SetChannelProfile(p *ChannelSessionProfile) { a.channel = p }

func (a *sessionModelAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	model := a.model
	if a.channel != nil && a.channel.Model != "" {
		model = a.channel.Model
	}
	a.channel = nil
	if m := a.TakeSessionModel(); m != "" {
		model = m
	}
	a.started = append(a.started, model)
	return &stubAgentSession{}, nil
}

func newSessionModelEngine(t *testing.T) (*Engine, *sessionModelAgent, *stubPlatformEngine) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	agent := &sessionModelAgent{stubModelModeAgent: stubModelModeAgent{model: "global-m"}}
	e := NewEngine("test", agent, nil, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	return e, agent, &stubPlatformEngine{n: "plain"}
}

func startFor(e *Engine, p Platform, key string) {
	e.getOrCreateInteractiveStateWith(key, p, nil, e.sessions.GetOrCreateActive(key), e.sessions, nil, "")
}

func TestCmdModel_DefaultSwitchIsSessionOnly(t *testing.T) {
	e, agent, p := newSessionModelEngine(t)
	saved := ""
	e.modelSaveFunc = func(m string) error { saved = m; return nil }
	msg := &Message{SessionKey: "test:u1", ReplyCtx: "ctx"}

	e.cmdModel(p, msg, []string{"sess-m"})

	if agent.model != "global-m" {
		t.Fatalf("global model changed to %q", agent.model)
	}
	if saved != "" {
		t.Fatalf("config must not be saved for a session switch, saved %q", saved)
	}
	if got := e.sessions.GetOrCreateActive("test:u1").GetModelOverride(); got != "sess-m" {
		t.Fatalf("override = %q", got)
	}
	if len(p.getSent()) == 0 || !strings.Contains(p.getSent()[len(p.getSent())-1], "this session only") {
		t.Fatalf("reply = %v", p.getSent())
	}

	startFor(e, p, "test:u1")
	startFor(e, p, "test:u2")
	if len(agent.started) != 2 || agent.started[0] != "sess-m" || agent.started[1] != "global-m" {
		t.Fatalf("started = %v", agent.started)
	}
}

func TestCmdModel_GlobalFlagSwitchesDefaultAndClearsOverride(t *testing.T) {
	for _, flag := range []string{"--global", "-g"} {
		e, agent, p := newSessionModelEngine(t)
		saved := ""
		e.modelSaveFunc = func(m string) error { saved = m; return nil }
		msg := &Message{SessionKey: "test:u1", ReplyCtx: "ctx"}

		e.cmdModel(p, msg, []string{"sess-m"})
		e.cmdModel(p, msg, []string{flag, "new-global"})

		if agent.model != "new-global" || saved != "new-global" {
			t.Fatalf("%s: model=%q saved=%q", flag, agent.model, saved)
		}
		if got := e.sessions.GetOrCreateActive("test:u1").GetModelOverride(); got != "" {
			t.Fatalf("%s: override should be cleared, got %q", flag, got)
		}
	}
}

func TestSessionModel_PrecedenceOverChannelAndGlobal(t *testing.T) {
	e, agent, p := newSessionModelEngine(t)
	e.SetChannelProfiles([]ChannelProfile{{Match: "discord:PARENT", Model: "chan-m"}})
	key := "discord:PARENT:u1"
	e.channelProfileForMessage(&Message{Platform: "discord", SessionKey: key})

	startFor(e, p, key) // channel > global
	e.cleanupInteractiveState(key)
	e.sessions.GetOrCreateActive(key).SetModelOverride("sess-m")
	startFor(e, p, key) // session > channel

	if len(agent.started) != 2 || agent.started[0] != "chan-m" || agent.started[1] != "sess-m" {
		t.Fatalf("started = %v", agent.started)
	}
	sw := ModelSwitcher(agent)
	line := e.modelScopesLine(key, sw, e.sessions)
	for _, want := range []string{"Session: sess-m", "Channel: chan-m", "Global: global-m"} {
		if !strings.Contains(line, want) {
			t.Fatalf("scopes line %q missing %q", line, want)
		}
	}
}

func TestCmdModel_NoArgsShowsScopes(t *testing.T) {
	e, _, p := newSessionModelEngine(t)
	msg := &Message{SessionKey: "test:u1", ReplyCtx: "ctx"}
	e.cmdModel(p, msg, []string{"sess-m"})
	e.cmdModel(p, msg, nil)
	all := strings.Join(p.getSent(), "\n")
	if !strings.Contains(all, "Session: sess-m | Channel: - | Global: global-m") {
		t.Fatalf("no-arg output missing scopes: %q", all)
	}
}

func TestSessionModel_ClearedOnNew(t *testing.T) {
	e, agent, p := newSessionModelEngine(t)
	msg := &Message{SessionKey: "test:u1", ReplyCtx: "ctx"}
	e.cmdModel(p, msg, []string{"sess-m"})
	old := e.sessions.GetOrCreateActive("test:u1")

	e.cmdNew(p, msg, nil)

	if old.GetModelOverride() != "" {
		t.Fatalf("old session override not cleared")
	}
	if got := e.sessions.GetOrCreateActive("test:u1").GetModelOverride(); got != "" {
		t.Fatalf("new session inherited override %q", got)
	}
	startFor(e, p, "test:u1")
	if agent.started[len(agent.started)-1] != "global-m" {
		t.Fatalf("started = %v", agent.started)
	}
}

func TestSessionModel_ClearedOnResetAllSessions(t *testing.T) {
	e, _, _ := newSessionModelEngine(t)
	s := e.sessions.GetOrCreateActive("test:u1")
	s.SetModelOverride("sess-m")
	e.resetAllSessions()
	if s.GetModelOverride() != "" {
		t.Fatalf("override not cleared by reset")
	}
}

func TestSessionModel_SurvivesConfigReloadAndRestart(t *testing.T) {
	e, _, p := newSessionModelEngine(t)
	store := filepath.Join(t.TempDir(), "sessions.json")
	e.sessions = NewSessionManager(store)
	e.SetConfigReloadFunc(func() (*ConfigReloadResult, error) { return &ConfigReloadResult{}, nil })
	msg := &Message{SessionKey: "test:u1", ReplyCtx: "ctx"}
	e.cmdModel(p, msg, []string{"sess-m"})

	e.cmdConfigReload(p, msg)
	if got := e.sessions.GetOrCreateActive("test:u1").GetModelOverride(); got != "sess-m" {
		t.Fatalf("override lost on reload: %q", got)
	}
	reloaded := NewSessionManager(store)
	if got := reloaded.GetOrCreateActive("test:u1").GetModelOverride(); got != "sess-m" {
		t.Fatalf("override not persisted: %q", got)
	}
}
