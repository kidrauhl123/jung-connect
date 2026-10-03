package antigravity

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("antigravity", New)
}

// Agent drives the Antigravity CLI (agy) in headless mode.
//
// Modes (maps to agy approval and sandbox flags):
//   - "default":   ask for each tool through the cc-connect permission bridge
//   - "yolo":      auto-approve all tools (--dangerously-skip-permissions)
//   - "plan":      read-only plan mode with terminal sandbox constraints (--sandbox)
type Agent struct {
	workDir      string
	model        string
	mode         string
	cmd          string   // CLI binary name, default "agy"
	cliExtraArgs []string // extra args from cmd after the binary name
	configEnv    []string // env vars from [projects.agent.options.env]
	timeout      time.Duration
	providers    []core.ProviderConfig
	activeIdx    int
	sessionEnv   []string
	mu           sync.RWMutex
}

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	mode, _ := opts["mode"].(string)
	mode = normalizeMode(mode)
	cmd, extraArgs := core.ParseCmdOpts(opts, "agy")

	var timeoutMins int64
	switch v := opts["timeout_mins"].(type) {
	case int64:
		timeoutMins = v
	case int:
		timeoutMins = int64(v)
	case float64:
		timeoutMins = int64(v)
	default:
		if v != nil {
			slog.Debug("antigravity: timeout_mins has unexpected type", "type", fmt.Sprintf("%T", v))
		}
	}
	var timeout time.Duration
	if timeoutMins > 0 {
		timeout = time.Duration(timeoutMins) * time.Minute
	}

	if _, err := exec.LookPath(cmd); err != nil {
		return nil, fmt.Errorf("antigravity: %q CLI not found in PATH, install from: https://antigravity.google/docs/cli-overview", cmd)
	}

	return &Agent{
		workDir:      workDir,
		model:        model,
		mode:         mode,
		cmd:          cmd,
		cliExtraArgs: extraArgs,
		configEnv:    core.ParseConfigEnv(opts),
		timeout:      timeout,
		activeIdx:    -1,
	}, nil
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yolo", "auto", "force", "bypasspermissions":
		return "yolo"
	case "plan", "sandbox":
		return "plan"
	default:
		return "default"
	}
}

func (a *Agent) Name() string { return "antigravity" }

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
	slog.Info("antigravity: work_dir changed", "work_dir", dir)
}

func (a *Agent) GetWorkDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workDir
}

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("antigravity: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return core.GetProviderModel(a.providers, a.activeIdx, a.model)
}

func (a *Agent) configuredModels() []core.ModelOption {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return core.GetProviderModels(a.providers, a.activeIdx)
}

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	configured := a.configuredModels()
	// Prefer New API /v1/models (provider base_url+key or env); toml entries stay as aliases.
	if models := a.fetchModelsFromAPI(ctx); len(models) > 0 {
		return core.MergeModelAliases(models, configured)
	}
	if len(configured) > 0 {
		return configured
	}
	return []core.ModelOption{
		{Name: "gemini-3.1-pro-preview", Desc: "Gemini 3.1 Pro Preview"},
		{Name: "gemini-3-flash-preview", Desc: "Gemini 3 Flash Preview"},
		{Name: "gemini-2.5-pro", Desc: "Gemini 2.5 Pro"},
		{Name: "gemini-2.5-flash", Desc: "Gemini 2.5 Flash"},
	}
}

func (a *Agent) fetchModelsFromAPI(ctx context.Context) []core.ModelOption {
	a.mu.RLock()
	apiKey := ""
	baseURL := ""
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		apiKey = a.providers[a.activeIdx].APIKey
		baseURL = a.providers[a.activeIdx].BaseURL
	}
	a.mu.RUnlock()

	baseURL, apiKey = core.ResolveModelsAPICreds(baseURL, apiKey)
	if apiKey != "" && baseURL != "" {
		if models := core.FetchOpenAICompatibleModels(ctx, baseURL, apiKey); len(models) > 0 {
			return models
		}
	}

	// Legacy fallback: Google Generative Language API (agy native path).
	gkey := os.Getenv("GEMINI_API_KEY")
	if gkey == "" {
		gkey = os.Getenv("GOOGLE_API_KEY")
	}
	if gkey == "" {
		return nil
	}

	url := "https://generativelanguage.googleapis.com/v1beta/models?key=" + gkey
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("antigravity: failed to fetch models", "error", err)
		return nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, m := range result.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if !strings.HasPrefix(id, "gemini-") {
			continue
		}
		models = append(models, core.ModelOption{Name: id, Desc: m.DisplayName})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name > models[j].Name })
	return models
}

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	model := a.model
	mode := a.mode
	cmd := a.cmd
	extraArgs := append([]string{}, a.cliExtraArgs...)
	workDir := a.workDir
	timeout := a.timeout
	// Provider env first, then config env so [projects.agent.options.env]
	// can pin GOOGLE_GEMINI_BASE_URL to the local rewrite proxy without
	// being overwritten by the provider OpenAI /v1 base URL.
	extraEnv := mergeSessionExtraEnv(a.configEnv, a.providerEnvLocked(), a.sessionEnv)
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	a.mu.Unlock()

	return newAntigravitySession(ctx, cmd, extraArgs, workDir, model, mode, sessionID, extraEnv, timeout)
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listAntigravitySessions(a.workDir)
}

func (a *Agent) GetLLMSessionTitle(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "/") || strings.Contains(sessionID, "\\") || sessionID == "." || sessionID == ".." {
		return ""
	}
	dataDir, err := antigravityDataDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "annotations", sessionID+".pbtxt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsePbtxtTitle(data))
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "/") || strings.Contains(sessionID, "\\") || sessionID == "." || sessionID == ".." {
		return fmt.Errorf("invalid session ID: %s", sessionID)
	}

	dataDir, err := antigravityDataDir()
	if err != nil {
		return fmt.Errorf("antigravity: cannot determine data dir: %w", err)
	}

	found := false

	// 1. Remove brain/<sessionID>
	brainPath := filepath.Join(dataDir, "brain", sessionID)
	if _, err := os.Stat(brainPath); err == nil {
		if err := os.RemoveAll(brainPath); err == nil {
			found = true
		}
	}

	// 2. Remove conversations/<sessionID>.db*
	convPattern := filepath.Join(dataDir, "conversations", sessionID+".db*")
	if matches, err := filepath.Glob(convPattern); err == nil {
		for _, m := range matches {
			if err := os.Remove(m); err == nil {
				found = true
			}
		}
	}

	// 3. Remove annotations/<sessionID>.pbtxt
	annotPath := filepath.Join(dataDir, "annotations", sessionID+".pbtxt")
	if _, err := os.Stat(annotPath); err == nil {
		if err := os.Remove(annotPath); err == nil {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("session file not found: %s", sessionID)
	}
	return nil
}

func (a *Agent) Stop() error { return nil }

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("antigravity: mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "Ask for approval through cc-connect on each tool use", DescZh: "每次工具调用都通过 cc-connect 请求确认"},
		{Key: "yolo", Name: "YOLO", NameZh: "全自动", Desc: "Auto-approve all tool calls", DescZh: "自动批准所有工具调用"},
		{Key: "plan", Name: "Plan", NameZh: "规划模式", Desc: "Read-only plan mode in sandbox", DescZh: "只读沙箱规划模式"},
	}
}

func (a *Agent) CommandDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".gemini", "commands")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "commands"))
	}
	return dirs
}

func (a *Agent) SkillDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".gemini", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "skills"))
	}
	return dirs
}

func (a *Agent) CompressCommand() string { return "" }

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	return filepath.Join(absDir, "GEMINI.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".gemini", "GEMINI.md")
}

func (a *Agent) SetProviders(providers []core.ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = providers
}

func (a *Agent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name == "" {
		a.activeIdx = -1
		slog.Info("antigravity: provider cleared")
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			slog.Info("antigravity: provider switched", "provider", name)
			return true
		}
	}
	return false
}

func (a *Agent) GetActiveProvider() *core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	return &p
}

func (a *Agent) ListProviders() []core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]core.ProviderConfig, len(a.providers))
	copy(result, a.providers)
	return result
}

func mergeSessionExtraEnv(configEnv, providerEnv, sessionEnv []string) []string {
	return core.MergeEnv(core.MergeEnv(providerEnv, configEnv), sessionEnv)
}

func (a *Agent) providerEnvLocked() []string {
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	var env []string
	if p.APIKey != "" {
		env = append(env, "GEMINI_API_KEY="+p.APIKey)
	}
	if base := strings.TrimSpace(p.BaseURL); base != "" {
		// agy Gemini-API mode talks to GOOGLE_GEMINI_BASE_URL + /v1beta/...
		// New API OpenAI-compatible URLs end in /v1; strip that suffix.
		geminiBase := strings.TrimRight(base, "/")
		geminiBase = strings.TrimSuffix(geminiBase, "/v1")
		geminiBase = strings.TrimSuffix(geminiBase, "/v1beta")
		if geminiBase != "" {
			env = append(env, "GOOGLE_GEMINI_BASE_URL="+geminiBase)
		}
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func antigravityProjectSlug(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return slugify(filepath.Base(abs))
	}

	data, err := os.ReadFile(filepath.Join(homeDir, ".gemini", "projects.json"))
	if err == nil {
		var registry struct {
			Projects map[string]string `json:"projects"`
		}
		if json.Unmarshal(data, &registry) == nil {
			normalized := filepath.Clean(abs)
			if slug, ok := registry.Projects[normalized]; ok {
				return slug
			}
		}
	}

	return slugify(filepath.Base(abs))
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	result := b.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	result = strings.Trim(result, "-")
	if result == "" {
		result = "project"
	}
	return result
}

func parsePbtxtTitle(data []byte) string {
	s := string(data)
	const prefix = `title:"`
	idx := strings.Index(s, prefix)
	if idx == -1 {
		return ""
	}
	rest := s[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		return ""
	}
	return rest[:end]
}

func cleanPromptSummary(content string) string {
	if start := strings.Index(content, "<USER_REQUEST>"); start != -1 {
		content = content[start+len("<USER_REQUEST>"):]
		if end := strings.Index(content, "</USER_REQUEST>"); end != -1 {
			content = content[:end]
		}
	}
	for _, chunk := range strings.Split(content, "\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk != "" && !strings.HasPrefix(chunk, "<") {
			return chunk
		}
	}
	return ""
}

func listAntigravitySessions(_ string) ([]core.AgentSessionInfo, error) {
	dataDir, err := antigravityDataDir()
	if err != nil {
		return nil, fmt.Errorf("antigravity: cannot determine data dir: %w", err)
	}

	brainDir := filepath.Join(dataDir, "brain")
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("antigravity: read brain dir: %w", err)
	}

	var sessions []core.AgentSessionInfo
	annotDir := filepath.Join(dataDir, "annotations")
	convDir := filepath.Join(dataDir, "conversations")

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		sessionID := entry.Name()
		var summary string
		var modTime time.Time

		// Check directory mod time as initial fallback
		if info, err := entry.Info(); err == nil {
			modTime = info.ModTime()
		}

		// Try to read summary from annotations/<sessionID>.pbtxt
		annotPath := filepath.Join(annotDir, sessionID+".pbtxt")
		if data, err := os.ReadFile(annotPath); err == nil {
			summary = parsePbtxtTitle(data)
			if aInfo, err := os.Stat(annotPath); err == nil && aInfo.ModTime().After(modTime) {
				modTime = aInfo.ModTime()
			}
		}

		// Check db file mod time
		dbPath := filepath.Join(convDir, sessionID+".db")
		if dbInfo, err := os.Stat(dbPath); err == nil && dbInfo.ModTime().After(modTime) {
			modTime = dbInfo.ModTime()
		}

		// Parse transcript.jsonl for message count and fallback summary
		transcriptPath := filepath.Join(brainDir, sessionID, ".system_generated", "logs", "transcript.jsonl")
		msgCount := 0
		if file, err := os.Open(transcriptPath); err == nil {
			if tInfo, err := file.Stat(); err == nil && tInfo.ModTime().After(modTime) {
				modTime = tInfo.ModTime()
			}
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for scanner.Scan() {
				line := scanner.Bytes()
				if len(line) == 0 {
					continue
				}
				var item struct {
					Type    string `json:"type"`
					Content string `json:"content"`
				}
				if json.Unmarshal(line, &item) == nil {
					if item.Type == "USER_INPUT" || item.Type == "PLANNER_RESPONSE" {
						msgCount++
					}
					if summary == "" && item.Type == "USER_INPUT" && item.Content != "" {
						summary = cleanPromptSummary(item.Content)
					}
				}
			}
			_ = file.Close()
		}

		if summary == "" {
			summary = sessionID
		}
		if utf8.RuneCountInString(summary) > 60 {
			summary = string([]rune(summary)[:60]) + "..."
		}

		sessions = append(sessions, core.AgentSessionInfo{
			ID:           sessionID,
			Summary:      summary,
			MessageCount: msgCount,
			ModifiedAt:   modTime,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})

	return sessions, nil
}
