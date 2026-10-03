package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"cc-connect", "cc-connect"},
		{"Daily", "daily"},
		{"My Project", "my-project"},
		{"hello_world", "hello-world"},
		{"Test.123", "test-123"},
		{"---weird---", "weird"},
		{"", "project"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := slugify(tt.input)
			if got != tt.want {
				t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"default", "default"},
		{"yolo", "yolo"},
		{"auto", "yolo"},
		{"force", "yolo"},
		{"plan", "plan"},
		{"sandbox", "plan"},
		{"invalid", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeMode(tt.input)
			if got != tt.want {
				t.Errorf("normalizeMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSession_ContinueSessionTreatedAsFresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "default", core.ContinueSession, nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := s.CurrentSessionID(); got != "" {
		t.Errorf("ContinueSession should be treated as fresh: chatID = %q, want empty", got)
	}
}

func TestBuildAntigravityArgs_PromptAtEnd(t *testing.T) {
	s, _ := newAntigravitySession(context.Background(), "echo", []string{"--verbose"}, "/tmp", "", "default", "", nil, 0)
	args := s.buildAntigravityArgs("sid-1", true, "plan", "/tmp/agy-config", "What is 1+1?")
	if len(args) < 2 {
		t.Fatalf("args too short: %v", args)
	}
	if args[len(args)-2] != "-p" || args[len(args)-1] != "What is 1+1?" {
		t.Fatalf("expected prompt to be final '-p <prompt>', got: %v", args)
	}
	if !contains(args, "--sandbox") {
		t.Fatalf("expected --sandbox in args, got: %v", args)
	}
	if !contains(args, "--gemini_dir=/tmp/agy-config") || !contains(args, "--print-timeout=24h") {
		t.Fatalf("expected isolated Agy config and extended print timeout, got: %v", args)
	}
	if !contains(args, "--verbose") {
		t.Fatalf("expected configured extra args, got: %v", args)
	}
	if contains(args, "-m") || contains(args, "--model") {
		t.Fatalf("did not expect model flags when model is empty, got: %v", args)
	}
}

func TestBuildAntigravityArgs_PassesModel(t *testing.T) {
	s, _ := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "gemini-3.8-flash-high", "yolo", "", nil, 0)
	args := s.buildAntigravityArgs("", false, "yolo", "", "hi")
	if !contains(args, "--model") || !contains(args, "gemini-3.8-flash-high") {
		t.Fatalf("expected --model gemini-3.8-flash-high in args, got: %v", args)
	}
	if !contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("expected yolo flag, got: %v", args)
	}
	if args[len(args)-2] != "-p" || args[len(args)-1] != "hi" {
		t.Fatalf("expected prompt last, got: %v", args)
	}
}

func TestBuildAntigravityArgs_DoesNotWrapCustomModelURL(t *testing.T) {
	s, _ := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "grok-4.6-xhigh", "yolo", "", []string{"GOOGLE_GEMINI_BASE_URL=https://ai.premsir.com"}, 0)
	args := s.buildAntigravityArgs("", false, "yolo", "", "hi")
	if contains(args, "gemini-api://local/models/grok-4.6-xhigh") {
		t.Fatalf("did not expect gemini-api:// wrap, got: %v", args)
	}
	if !contains(args, "--model") || !contains(args, "grok-4.6-xhigh") {
		t.Fatalf("expected --model grok-4.6-xhigh, got: %v", args)
	}
}

func TestEnsureAgyCustomModelSettings_RegistersGrok(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", dir)

	if err := ensureAgyCustomModelSettings("grok-4.6-xhigh", []string{
		"GEMINI_API_KEY=test-key",
		"GOOGLE_GEMINI_BASE_URL=https://ai.premsir.com",
	}); err != nil {
		t.Fatalf("ensureAgyCustomModelSettings: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["modelProvider"] != "gemini" {
		t.Fatalf("modelProvider = %v, want gemini", got["modelProvider"])
	}
	cfg, _ := got["customModelsConfig"].(map[string]any)
	models, _ := cfg["customModels"].(map[string]any)
	entry, _ := models["grok-4.6-xhigh"].(map[string]any)
	if fmt.Sprint(entry["modelName"]) != "grok-4.6-xhigh" {
		t.Fatalf("custom model entry = %v", models)
	}

	if err := ensureAgyCustomModelSettings("gemini-3.8-flash-high", []string{
		"GEMINI_API_KEY=test-key",
		"GOOGLE_GEMINI_BASE_URL=https://ai.premsir.com",
	}); err != nil {
		t.Fatalf("stock model: %v", err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "settings.json"))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal after stock: %v", err)
	}
	cfg, _ = got["customModelsConfig"].(map[string]any)
	models, _ = cfg["customModels"].(map[string]any)
	if _, ok := models["gemini-3.8-flash-high"]; ok {
		t.Fatalf("stock gemini id must not be registered, got %v", models)
	}
	if _, ok := models["grok-4.6-xhigh"]; !ok {
		t.Fatalf("existing grok registration was dropped: %v", models)
	}
}

func TestAntigravitySession_ResumePassesConversationID(t *testing.T) {
	if os.Getenv("GO_WANT_ANTIGRAVITY_HELPER") == "1" {
		argsPath := os.Getenv("CC_ANTIGRAVITY_ARGS_FILE")
		if argsPath == "" {
			os.Exit(2)
		}
		if err := os.WriteFile(argsPath, []byte(strings.Join(os.Args, "\x00")), 0o600); err != nil {
			os.Exit(2)
		}
		_, _ = io.WriteString(os.Stdout, "ok\n")
		os.Exit(0)
	}

	argsPath := t.TempDir() + string(os.PathSeparator) + "args"
	workDir := t.TempDir()
	s, err := newAntigravitySession(
		context.Background(),
		os.Args[0],
		[]string{"-test.run=TestAntigravitySession_ResumePassesConversationID", "--"},
		workDir,
		"",
		"default",
		"conversation-1",
		[]string{
			"GO_WANT_ANTIGRAVITY_HELPER=1",
			"CC_ANTIGRAVITY_ARGS_FILE=" + argsPath,
		},
		0,
	)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Send("second turn", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case event := <-s.Events():
		if event.Type != core.EventText {
			t.Fatalf("first event type = %q, want %q", event.Type, core.EventText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for helper output")
	}
	select {
	case event := <-s.Events():
		if event.Type != core.EventResult || !event.Done {
			t.Fatalf("completion event = %+v, want done EventResult", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for EventResult")
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	args := strings.Split(string(data), "\x00")
	if !contains(args, "--conversation") || !contains(args, "conversation-1") {
		t.Fatalf("resume args = %v, want --conversation conversation-1", args)
	}
	if !contains(args, "-p") || !contains(args, "second turn") {
		t.Fatalf("prompt args = %v, want -p second turn", args)
	}
}

func TestDefaultModeCreatesPermissionBridge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "default", "", nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.permissionBridge == nil {
		t.Fatal("permissionBridge = nil, want default-mode permission bridge")
	}
	if _, err := os.Stat(filepath.Join(s.permissionBridge.AgyConfigDir(), "config", "hooks.json")); err != nil {
		t.Fatalf("stat Agy hook overlay: %v", err)
	}
}

func TestNonDefaultModesDoNotCreatePermissionBridge(t *testing.T) {
	for _, mode := range []string{"yolo", "plan"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", mode, "", nil, 0)
			if err != nil {
				t.Fatalf("newAntigravitySession: %v", err)
			}
			defer func() { _ = s.Close() }()

			if s.permissionBridge != nil {
				t.Fatalf("permissionBridge = %v, want nil", s.permissionBridge)
			}
		})
	}
}

func TestRespondPermissionRequiresDefaultMode(t *testing.T) {
	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "plan", "", nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	err = s.RespondPermission("req", core.PermissionResult{Behavior: "allow"})
	if err == nil || !strings.Contains(err.Error(), "only available in default mode") {
		t.Fatalf("RespondPermission() error = %v, want default-mode error", err)
	}
}

func TestSendDoesNotHoldStdinOpen(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := t.TempDir()
	cmdPath := filepath.Join(t.TempDir(), "fake-agy.sh")
	script := "#!/bin/sh\ncat >/dev/null\nprintf 'done\\n'\n"
	if err := os.WriteFile(cmdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake agy: %v", err)
	}

	s, err := newAntigravitySession(context.Background(), cmdPath, nil, workDir, "", "default", "", nil, 2*time.Second)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Send("hello", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.After(3 * time.Second)
	var text strings.Builder
	for {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				t.Fatal("unexpected permission request from unstructured stdout")
			case core.EventText:
				text.WriteString(ev.Content)
			case core.EventResult:
				if !strings.Contains(text.String(), "done") {
					t.Fatalf("text = %q, want done", text.String())
				}
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for agy process to receive stdin EOF")
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) == want {
			return true
		}
	}
	return false
}

func TestMergeSessionExtraEnv_ConfigOverridesProviderBaseURL(t *testing.T) {
	got := mergeSessionExtraEnv(
		[]string{"GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:18765", "GEMINI_API_KEY=from-config"},
		[]string{"GEMINI_API_KEY=from-provider", "GOOGLE_GEMINI_BASE_URL=https://ai.premsir.com"},
		[]string{"CC_PROJECT=antigravity"},
	)
	m := map[string]string{}
	for _, e := range got {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			m[k] = v
		}
	}
	if m["GOOGLE_GEMINI_BASE_URL"] != "http://127.0.0.1:18765" {
		t.Fatalf("config proxy URL lost, env=%v", got)
	}
	if m["GEMINI_API_KEY"] != "from-config" {
		t.Fatalf("config API key lost, env=%v", got)
	}
	if m["CC_PROJECT"] != "antigravity" {
		t.Fatalf("session env lost, env=%v", got)
	}
}

func TestDropEnvKey_RemovesGoogleAPIKey(t *testing.T) {
	got := dropEnvKey([]string{"GOOGLE_API_KEY=should-drop", "GEMINI_API_KEY=keep", "PATH=/bin"}, "GOOGLE_API_KEY")
	for _, e := range got {
		if strings.HasPrefix(e, "GOOGLE_API_KEY=") {
			t.Fatalf("GOOGLE_API_KEY still present: %v", got)
		}
	}
}

func TestParsePbtxtTitleAndCleanPromptSummary(t *testing.T) {
	pbtxtData := []byte("title:\"My Session Title\"\nother_field: 123\n")
	if got := parsePbtxtTitle(pbtxtData); got != "My Session Title" {
		t.Fatalf("parsePbtxtTitle = %q, want %q", got, "My Session Title")
	}

	contentWithTags := "<USER_REQUEST>\nHello world from user\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime\n</ADDITIONAL_METADATA>"
	if got := cleanPromptSummary(contentWithTags); got != "Hello world from user" {
		t.Fatalf("cleanPromptSummary = %q, want %q", got, "Hello world from user")
	}

	contentPlain := "First line summary\nSecond line"
	if got := cleanPromptSummary(contentPlain); got != "First line summary" {
		t.Fatalf("cleanPromptSummary plain = %q, want %q", got, "First line summary")
	}
}

func TestScanPreEntriesAndDetectNewSessionID(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", tempDir)

	brainDir := filepath.Join(tempDir, "brain")
	convDir := filepath.Join(tempDir, "conversations")
	annotDir := filepath.Join(tempDir, "annotations")
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(annotDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create an existing session
	oldID := "11111111-1111-1111-1111-111111111111"
	if err := os.MkdirAll(filepath.Join(brainDir, oldID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(convDir, oldID+".db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	pre := scanPreEntries()
	if !pre[oldID] {
		t.Fatalf("expected oldID %q in preEntries", oldID)
	}

	as := &antigravitySession{workDir: "/tmp"}
	sendStartedAt := time.Now()

	// Before new session created, detectNewSessionID returns ""
	if got := as.detectNewSessionID(pre, sendStartedAt); got != "" {
		t.Fatalf("expected empty session ID, got %q", got)
	}

	// Create a new session
	newID := "22222222-2222-2222-2222-222222222222"
	newBrainDir := filepath.Join(brainDir, newID, ".system_generated", "logs")
	if err := os.MkdirAll(newBrainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(newBrainDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"USER_INPUT","content":"<USER_REQUEST>\nHow are you?\n</USER_REQUEST>"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(convDir, newID+".db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(annotDir, newID+".pbtxt"), []byte(`title:"Greetings Conversation"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Now detectNewSessionID should return newID
	detected := as.detectNewSessionID(pre, sendStartedAt)
	if detected != newID {
		t.Fatalf("detected ID = %q, want %q", detected, newID)
	}

	// Test listAntigravitySessions
	sessions, err := listAntigravitySessions("/tmp")
	if err != nil {
		t.Fatalf("listAntigravitySessions failed: %v", err)
	}
	if len(sessions) < 2 {
		t.Fatalf("expected at least 2 sessions, got %d", len(sessions))
	}
	var foundNew bool
	for _, s := range sessions {
		if s.ID == newID {
			foundNew = true
			if s.Summary != "Greetings Conversation" {
				t.Fatalf("session summary = %q, want %q", s.Summary, "Greetings Conversation")
			}
			if s.MessageCount != 1 {
				t.Fatalf("message count = %d, want 1", s.MessageCount)
			}
		}
	}
	if !foundNew {
		t.Fatalf("newID %q not found in listAntigravitySessions", newID)
	}

	// Test DeleteSession
	agent := &Agent{workDir: "/tmp"}
	if err := agent.DeleteSession(context.Background(), newID); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, newID)); !os.IsNotExist(err) {
		t.Fatalf("brain dir still exists after delete")
	}
	if _, err := os.Stat(filepath.Join(convDir, newID+".db")); !os.IsNotExist(err) {
		t.Fatalf("db file still exists after delete")
	}
	if _, err := os.Stat(filepath.Join(annotDir, newID+".pbtxt")); !os.IsNotExist(err) {
		t.Fatalf("annot file still exists after delete")
	}

	// Delete again should return error
	if err := agent.DeleteSession(context.Background(), newID); err == nil {
		t.Fatal("expected error deleting already deleted session")
	}

	// Delete invalid ID should return error
	if err := agent.DeleteSession(context.Background(), "../invalid"); err == nil {
		t.Fatal("expected error deleting invalid session ID")
	}
}

