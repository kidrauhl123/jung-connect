package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeSkill(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeChannelProfiles(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "p.md"), []byte("  from file  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NormalizeChannelProfiles([]ChannelProfile{
		{Match: "  discord:1 ", Prompt: " inline ", PromptFile: "p.md", Skills: []string{" a ", "", "b"}, WorkDir: "rel"},
		{Match: "   "},
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (blank match dropped)", len(out))
	}
	cp := out[0]
	if cp.Match != "discord:1" || cp.Prompt != "inline\n\nfrom file" {
		t.Fatalf("match/prompt = %q / %q", cp.Match, cp.Prompt)
	}
	if strings.Join(cp.Skills, ",") != "a,b" {
		t.Fatalf("skills = %v", cp.Skills)
	}
	if !filepath.IsAbs(cp.WorkDir) {
		t.Fatalf("work_dir not absolute: %q", cp.WorkDir)
	}
	if _, err := NormalizeChannelProfiles([]ChannelProfile{{Match: "discord:1", PromptFile: "missing.md"}}, base); err == nil {
		t.Fatal("expected error for missing prompt_file")
	}
}

func TestChannelProfileMatching_ThreadInheritsParent(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetChannelProfiles([]ChannelProfile{
		{Match: "discord:PARENT", Prompt: "parent"},
		{Match: "discord:SPECIAL", Prompt: "special-thread"},
	})

	// Message in a thread under PARENT: session key carries the thread ID,
	// ChannelKey carries the parent (as the Discord platform stamps it).
	threadMsg := &Message{Platform: "discord", SessionKey: "discord:THREAD:u1", ChannelKey: "PARENT"}
	if cp := e.channelProfileForMessage(threadMsg); cp == nil || cp.Prompt != "parent" {
		t.Fatalf("thread should inherit parent profile, got %+v", cp)
	}
	// Session-key-only lookups (session start, /model) use the remembered parent,
	// also for workspace-prefixed interactive keys.
	if cp := e.channelProfileForSessionKey("discord:THREAD:u1"); cp == nil || cp.Prompt != "parent" {
		t.Fatalf("session key lookup = %+v", cp)
	}
	if cp := e.channelProfileForSessionKey("/ws/x:discord:THREAD:u1"); cp == nil || cp.Prompt != "parent" {
		t.Fatalf("prefixed session key lookup = %+v", cp)
	}
	// A thread with its own entry wins over the parent.
	special := &Message{Platform: "discord", SessionKey: "discord:SPECIAL:u1", ChannelKey: "PARENT"}
	if cp := e.channelProfileForMessage(special); cp == nil || cp.Prompt != "special-thread" {
		t.Fatalf("exact thread entry should win, got %+v", cp)
	}
	// Plain channel message and unknown channel.
	if cp := e.channelProfileForMessage(&Message{Platform: "discord", SessionKey: "discord:PARENT:u1"}); cp == nil || cp.Prompt != "parent" {
		t.Fatalf("channel message = %+v", cp)
	}
	if cp := e.channelProfileForMessage(&Message{Platform: "discord", SessionKey: "discord:OTHER:u1", ChannelKey: "OTHER"}); cp != nil {
		t.Fatalf("unconfigured channel should have no profile, got %+v", cp)
	}
	// Reload replaces profiles.
	e.SetChannelProfiles(nil)
	if cp := e.channelProfileForSessionKey("discord:THREAD:u1"); cp != nil {
		t.Fatalf("after reload to empty, got %+v", cp)
	}
}

func TestResolveSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "direct"), "d")
	writeSkill(t, filepath.Join(root, "category", "nested"), "n")
	other := filepath.Join(t.TempDir(), "bypath")
	writeSkill(t, other, "p")

	if s, ok := resolveSkill("direct", []string{root}); !ok || s.Dir != filepath.Join(root, "direct") {
		t.Fatalf("direct = %+v %v", s, ok)
	}
	if s, ok := resolveSkill("nested", []string{root}); !ok || s.Dir != filepath.Join(root, "category", "nested") {
		t.Fatalf("nested = %+v %v", s, ok)
	}
	if s, ok := resolveSkill(other, nil); !ok || s.Name != "bypath" {
		t.Fatalf("path = %+v %v", s, ok)
	}
	if _, ok := resolveSkill("missing", []string{root}); ok {
		t.Fatal("missing skill should not resolve")
	}
}

func TestBuildChannelSessionProfile_GeneratesArtifacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSkill(t, filepath.Join(home, ".hermes", "skills", "niannian"), "---\nname: niannian\n---\nbody")
	dataDir := t.TempDir()

	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetDataDir(dataDir)
	e.SetProjectStateStore(NewProjectStateStore(filepath.Join(dataDir, "state.json")))
	cp := &ChannelProfile{Match: "discord:1", Prompt: "CANARY rule", Skills: []string{"niannian", "nope"}, Model: "grok-4.7", ReasoningEffort: "high"}

	sp := e.buildChannelSessionProfile(cp, t.TempDir())
	if sp == nil || sp.Model != "grok-4.7" || sp.ReasoningEffort != "high" {
		t.Fatalf("profile = %+v", sp)
	}
	if len(sp.Skills) != 1 || sp.Skills[0].Name != "niannian" {
		t.Fatalf("skills = %+v", sp.Skills)
	}
	if !strings.Contains(sp.Prompt, "CANARY rule") || !strings.Contains(sp.Prompt, "niannian: ") {
		t.Fatalf("prompt = %q", sp.Prompt)
	}
	if !strings.HasPrefix(sp.AgentProfilePath, filepath.Join(dataDir, "channel-profiles")) {
		t.Fatalf("profile path = %q", sp.AgentProfilePath)
	}
	data, err := os.ReadFile(sp.AgentProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{"---\nname: cc-channel-", "prompt_mode: full\n", "model: inherit\n", "agents_md: true\n", "---\n\nCANARY rule"} {
		if !strings.Contains(body, want) {
			t.Fatalf("agent profile missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "permission_mode") {
		t.Fatalf("agent profile must not override permission mode:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(sp.PluginDir, "skills", "niannian", "SKILL.md")); err != nil {
		t.Fatalf("plugin skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sp.PluginDir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("plugin manifest missing: %v", err)
	}

	// Same profile → same artifacts (stable hash); per-channel /model override wins.
	e.projectState.SetChannelModelOverride("discord:1", "grok-4.6-xhigh")
	sp2 := e.buildChannelSessionProfile(cp, t.TempDir())
	if sp2.AgentProfilePath != sp.AgentProfilePath || sp2.PluginDir != sp.PluginDir {
		t.Fatalf("artifacts not stable: %q vs %q", sp2.AgentProfilePath, sp.AgentProfilePath)
	}
	if sp2.Model != "grok-4.6-xhigh" {
		t.Fatalf("channel model override not applied: %q", sp2.Model)
	}

	// Prompt-less, skill-less profile (model only) generates no artifacts.
	sp3 := e.buildChannelSessionProfile(&ChannelProfile{Match: "discord:2", Model: "m"}, "")
	if sp3.AgentProfilePath != "" || sp3.PluginDir != "" || sp3.Model != "m" {
		t.Fatalf("model-only profile = %+v", sp3)
	}
}

// profileRecordingAgent is a native ChannelProfileInjector that records what
// was staged when StartSession ran (and consumes it like real agents do).
type profileRecordingAgent struct {
	stubAgent
	mu      sync.Mutex
	pending *ChannelSessionProfile
	started []*ChannelSessionProfile
}

func (a *profileRecordingAgent) SetChannelProfile(p *ChannelSessionProfile) {
	a.mu.Lock()
	a.pending = p
	a.mu.Unlock()
}

func (a *profileRecordingAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	a.mu.Lock()
	a.started = append(a.started, a.pending)
	a.pending = nil
	a.mu.Unlock()
	return &stubAgentSession{}, nil
}

func TestSessionStart_StagesChannelProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	agent := &profileRecordingAgent{}
	e := NewEngine("test", agent, nil, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetChannelProfiles([]ChannelProfile{{Match: "discord:PARENT", Prompt: "rule", Model: "grok-4.7"}})
	p := &stubPlatformEngine{n: "discord"}

	msg := &Message{Platform: "discord", SessionKey: "discord:THREAD:u1", ChannelKey: "PARENT"}
	e.channelProfileForMessage(msg)
	sess := e.sessions.GetOrCreateActive(msg.SessionKey)
	e.getOrCreateInteractiveStateWith(msg.SessionKey, p, nil, sess, e.sessions, nil, "")

	other := "discord:OTHER:u1"
	e.getOrCreateInteractiveStateWith(other, p, nil, e.sessions.GetOrCreateActive(other), e.sessions, nil, "")

	if len(agent.started) != 2 {
		t.Fatalf("starts = %d", len(agent.started))
	}
	if sp := agent.started[0]; sp == nil || sp.Model != "grok-4.7" || sp.AgentProfilePath == "" {
		t.Fatalf("configured channel staged %+v", sp)
	}
	if agent.started[1] != nil {
		t.Fatalf("unconfigured channel must start with no profile, got %+v", agent.started[1])
	}
	st := e.interactiveStates[msg.SessionKey]
	if st == nil || st.pendingChannelContext != "" {
		t.Fatalf("native agent must not get a first-message block")
	}
}

func TestSessionStart_NonNativeAgentGetsFirstMessageBlock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetChannelProfiles([]ChannelProfile{{Match: "discord:1557", Prompt: "rule-xyz"}})
	p := &stubPlatformEngine{n: "discord"}
	key := "discord:1557:u1"
	e.channelProfileForMessage(&Message{Platform: "discord", SessionKey: key})
	st := e.getOrCreateInteractiveStateWith(key, p, nil, e.sessions.GetOrCreateActive(key), e.sessions, nil, "")
	if !strings.Contains(st.pendingChannelContext, "<channel-context>\nrule-xyz") {
		t.Fatalf("pendingChannelContext = %q", st.pendingChannelContext)
	}
}

func TestPersistModelOverride_PerChannel(t *testing.T) {
	dataDir := t.TempDir()
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetProjectStateStore(NewProjectStateStore(filepath.Join(dataDir, "state.json")))
	e.SetChannelProfiles([]ChannelProfile{{Match: "discord:PARENT", Model: "grok-4.7"}})
	e.channelProfileForMessage(&Message{Platform: "discord", SessionKey: "discord:T99:u1", ChannelKey: "PARENT"})

	e.persistWorkspaceModelOverride("/ws:discord:T99:u1", "discord:T99:u1", &stubAgent{}, "grok-4.6-xhigh")
	if got := e.projectState.ChannelModelOverride("discord:PARENT"); got != "grok-4.6-xhigh" {
		t.Fatalf("channel override = %q", got)
	}
	if got := e.projectState.WorkspaceModelOverride("/ws"); got != "" {
		t.Fatalf("workspace override should be untouched, got %q", got)
	}
	reloaded := NewProjectStateStore(filepath.Join(dataDir, "state.json"))
	if got := reloaded.ChannelModelOverride("discord:PARENT"); got != "grok-4.6-xhigh" {
		t.Fatalf("override not persisted: %q", got)
	}
}

func TestChannelWorkDir_OverridesBinding(t *testing.T) {
	baseDir := t.TempDir()
	boundDir := filepath.Join(baseDir, "bound")
	pinnedDir := filepath.Join(baseDir, "pinned")
	for _, d := range []string{boundDir, pinnedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e := newTestEngineWithMultiWorkspaceAgent(t, baseDir)
	channelKey := "test-platform:C-pin"
	e.workspaceBindings.Bind("project:test", channelKey, "pin", boundDir)
	e.SetChannelProfiles([]ChannelProfile{{Match: channelKey, WorkDir: pinnedDir}})

	p := &mockChannelResolver{name: "test-platform", names: map[string]string{}}
	msg := &Message{Platform: "test-platform", ChannelKey: "C-pin", SessionKey: channelKey + ":U-001"}
	e.channelProfileForMessage(msg)
	_, _, interactiveKey, workspaceDir, err := e.commandContextWithWorkspace(p, msg)
	if err != nil {
		t.Fatal(err)
	}
	want := normalizeWorkspacePath(pinnedDir)
	if workspaceDir != want || interactiveKey != want+":"+msg.SessionKey {
		t.Fatalf("workspace = %q key = %q, want %q", workspaceDir, interactiveKey, want)
	}
	if got := e.interactiveKeyForSessionKey(msg.SessionKey); got != want+":"+msg.SessionKey {
		t.Fatalf("interactiveKeyForSessionKey = %q", got)
	}
}
