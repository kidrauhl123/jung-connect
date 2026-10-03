package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// antigravitySession manages multi-turn conversations with the Antigravity CLI (agy).
type antigravitySession struct {
	cmd              string
	extraArgs        []string // extra args from cmd, prepended before agy args
	workDir          string
	model            string
	mode             string
	timeout          time.Duration
	extraEnv         []string
	events           chan core.Event
	closeOnce        sync.Once
	chatID           atomic.Value // stores string
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	alive            atomic.Bool
	permissionBridge *agyPermissionBridge
}

func newAntigravitySession(ctx context.Context, cmd string, extraArgs []string, workDir, model, mode, resumeID string, extraEnv []string, timeout time.Duration) (*antigravitySession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	as := &antigravitySession{
		cmd:       cmd,
		extraArgs: extraArgs,
		workDir:   workDir,
		model:     model,
		mode:      mode,
		timeout:   timeout,
		extraEnv:  extraEnv,
		events:    make(chan core.Event, 64),
		ctx:       sessionCtx,
		cancel:    cancel,
	}
	as.alive.Store(true)

	if mode == "default" {
		bridge, err := newAgyPermissionBridge(sessionCtx, as.events)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("antigravity: initialize permission bridge: %w", err)
		}
		as.permissionBridge = bridge
	}

	if resumeID != "" && resumeID != core.ContinueSession {
		as.chatID.Store(resumeID)
	}

	return as, nil
}

func (as *antigravitySession) Send(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	// Capture existing chat logs so we can identify a new session on first turn
	var preEntries map[string]bool
	sendStartedAt := time.Now()
	if as.CurrentSessionID() == "" {
		preEntries = scanPreEntries()
	}

	// Save images and files into the workspace
	attachDir := filepath.Join(as.workDir, ".cc-connect", "attachments")
	if (len(images) > 0 || len(files) > 0) && os.MkdirAll(attachDir, 0o755) != nil {
		attachDir = os.TempDir()
	}

	var imageRefs []string
	for i, img := range images {
		ext := ".png"
		switch img.MimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		case "image/webp":
			ext = ".webp"
		}
		fname := fmt.Sprintf("img_%d_%d%s", time.Now().UnixMilli(), i, ext)
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, img.Data, 0o600); err == nil {
			imageRefs = append(imageRefs, fpath)
		}
	}

	var fileRefs []string
	for i, f := range files {
		fname := filepath.Base(f.FileName)
		if fname == "" || fname == "." || fname == ".." {
			fname = fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), i)
		}
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, f.Data, 0o600); err == nil {
			fileRefs = append(fileRefs, fpath)
		}
	}

	chatID := as.CurrentSessionID()
	isResume := chatID != ""

	// Attach image and file references to prompt
	fullPrompt := prompt
	if len(imageRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached image(s)."
		}
		fullPrompt += "\n\n[Attached images saved at: " + strings.Join(imageRefs, ", ") + "]"
	}
	if len(fileRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached file(s)."
		}
		fullPrompt += "\n\n[Attached files saved at: " + strings.Join(fileRefs, ", ") + "]"
	}
	agyConfigDir := ""
	if as.permissionBridge != nil {
		agyConfigDir = as.permissionBridge.AgyConfigDir()
	}
	if err := ensureAgyCustomModelSettings(as.resolvedAgyModel(), as.extraEnv); err != nil {
		return fmt.Errorf("antigravity: prepare settings: %w", err)
	}
	args := as.buildAntigravityArgs(chatID, isResume, as.mode, agyConfigDir, fullPrompt)

	var ctx context.Context
	var cancel context.CancelFunc
	if as.timeout > 0 {
		ctx, cancel = context.WithTimeout(as.ctx, as.timeout)
	} else {
		ctx, cancel = context.WithCancel(as.ctx)
	}

	started := false
	defer func() {
		if !started {
			cancel()
		}
	}()

	slog.Debug("antigravitySession: launching", "resume", isResume, "args", core.RedactArgs(args))
	cmd := exec.CommandContext(ctx, as.cmd, args...)
	cmd.WaitDelay = 1 * time.Second
	cmd.Dir = as.workDir
	env := dropEnvKey(os.Environ(), "GOOGLE_API_KEY")
	if len(as.extraEnv) > 0 {
		env = core.MergeEnv(env, as.extraEnv)
	}
	if as.permissionBridge != nil {
		env = core.MergeEnv(env, as.permissionBridge.Env())
	}
	// agy prefers GOOGLE_API_KEY over GEMINI_API_KEY; keep only the latter.
	env = dropEnvKey(env, "GOOGLE_API_KEY")
	cmd.Env = env

	// Keep stdin disconnected: agy --print consumes piped stdin to EOF before
	// processing the prompt, so an open pipe would deadlock the turn.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("antigravitySession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("antigravitySession: start: %w", err)
	}

	started = true
	as.wg.Add(1)
	go func() {
		defer cancel()
		as.readLoop(ctx, cmd, stdout, &stderrBuf, append(imageRefs, fileRefs...), preEntries, sendStartedAt)
	}()

	return nil
}

func dropEnvKey(env []string, key string) []string {
	if key == "" {
		return env
	}
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if e == key || strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (as *antigravitySession) resolvedAgyModel() string {
	return strings.TrimSpace(as.model)
}

func envLookup(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}

func isAgyStockModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || strings.Contains(m, "://") {
		return false
	}
	return strings.HasPrefix(m, "gemini-") ||
		strings.HasPrefix(m, "claude-") ||
		strings.HasPrefix(m, "gpt-oss-")
}

// ensureAgyCustomModelSettings points agy at a Gemini-compatible gateway
// (New API) and registers non-stock slugs so --model grok-... is accepted.
// Stock Gemini/Claude/GPT-OSS ids must stay unregistered: agy's custom-model
// path 400s several of them. gemini-api://local/models/<id> is not a valid
// --model value on agy 1.2.x.
func ensureAgyCustomModelSettings(model string, extraEnv []string) error {
	if envLookup(extraEnv, "GEMINI_API_KEY") == "" && envLookup(extraEnv, "GOOGLE_GEMINI_BASE_URL") == "" {
		return nil
	}
	dataDir, err := antigravityDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create agy data dir: %w", err)
	}
	path := filepath.Join(dataDir, "settings.json")
	current := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	current["modelProvider"] = "gemini"
	model = strings.TrimSpace(model)
	if model != "" && !isAgyStockModel(model) {
		cfg, _ := current["customModelsConfig"].(map[string]any)
		if cfg == nil {
			cfg = map[string]any{}
		}
		models, _ := cfg["customModels"].(map[string]any)
		if models == nil {
			models = map[string]any{}
		}
		if existing, ok := models[model]; !ok {
			models[model] = map[string]any{"modelName": model}
		} else if m, ok := existing.(map[string]any); ok {
			if strings.TrimSpace(fmt.Sprint(m["modelName"])) == "" {
				m["modelName"] = model
				models[model] = m
			}
		}
		cfg["customModels"] = models
		current["customModelsConfig"] = cfg
	}

	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agy settings: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dataDir, "settings.json.*")
	if err != nil {
		return fmt.Errorf("create agy settings temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write agy settings temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod agy settings temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close agy settings temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	slog.Info("antigravitySession: agy settings ready", "path", path, "model", model, "stock", isAgyStockModel(model))
	return nil
}

func (as *antigravitySession) buildAntigravityArgs(chatID string, isResume bool, mode, agyConfigDir, fullPrompt string) []string {
	// Prepend extra args from cmd so wrappers like "timeout 3600 agy" work.
	// Keep "-p <prompt>" at the very end because agy consumes the immediate next arg.
	args := append([]string{}, as.extraArgs...)
	if agyConfigDir != "" {
		// Antigravity currently names this compatibility flag --gemini_dir.
		args = append(args, "--gemini_dir="+agyConfigDir, "--print-timeout=24h")
	}
	if isResume {
		args = append(args, "--conversation", chatID)
	}
	if model := strings.TrimSpace(as.resolvedAgyModel()); model != "" {
		args = append(args, "--model", model)
	}
	switch mode {
	case "yolo":
		args = append(args, "--dangerously-skip-permissions")
	case "plan":
		args = append(args, "--sandbox")
	}
	args = append(args, "-p", fullPrompt)
	return args
}

func (as *antigravitySession) readLoop(ctx context.Context, cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer, tempFiles []string, preEntries map[string]bool, sendStartedAt time.Time) {
	defer as.wg.Done()
	defer func() {
		for _, f := range tempFiles {
			_ = os.Remove(f)
		}

		err := cmd.Wait()

		// Detect conversation ID if this was the first turn of a fresh session.
		// agy may flush the chat file only while the process is exiting, so wait
		// for process reaping before scanning the chat directory.
		if as.CurrentSessionID() == "" {
			var sid string
			for attempt := 0; attempt < 25; attempt++ {
				sid = as.detectNewSessionID(preEntries, sendStartedAt)
				if sid != "" {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if sid != "" {
				as.chatID.Store(sid)
				slog.Debug("antigravitySession: detected session ID", "session_id", sid)
				// Emit an EventText carrying the session ID back to core.
				select {
				case as.events <- core.Event{Type: core.EventText, SessionID: sid}:
				case <-as.ctx.Done():
				}
			}
		}

		sid := as.CurrentSessionID()
		if err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("antigravitySession: process failed", "error", err, "stderr", stderrMsg)
				select {
				case as.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}:
				case <-as.ctx.Done():
				}
			}
		}

		// Finalize turn.
		select {
		case as.events <- core.Event{Type: core.EventResult, SessionID: sid, Done: true}:
		case <-as.ctx.Done():
		}
	}()

	go func() {
		<-ctx.Done()
		_ = stdout.Close()
	}()

	reader := bufio.NewReader(stdout)
	buf := make([]byte, 1024)

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			text := string(buf[:n])
			select {
			case as.events <- core.Event{Type: core.EventText, Content: text}:
			case <-as.ctx.Done():
				return
			}
		}
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "file already closed") {
				slog.Error("antigravitySession: read error", "error", err)
				select {
				case as.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}:
				case <-as.ctx.Done():
				}
			}
			return
		}
	}
}

func antigravityDataDir() (string, error) {
	if dir := os.Getenv("ANTIGRAVITY_APP_DATA_DIR"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("AGY_APP_DATA_DIR"); dir != "" {
		return dir, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("antigravity: cannot determine home dir: %w", err)
	}
	return filepath.Join(homeDir, ".gemini", "antigravity-cli"), nil
}

func scanPreEntries() map[string]bool {
	pre := make(map[string]bool)
	dataDir, err := antigravityDataDir()
	if err != nil {
		return pre
	}

	// Scan brain/ directory
	brainDir := filepath.Join(dataDir, "brain")
	if entries, err := os.ReadDir(brainDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				pre[e.Name()] = true
			}
		}
	}

	// Scan conversations/ directory
	convDir := filepath.Join(dataDir, "conversations")
	if entries, err := os.ReadDir(convDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".db") {
				id := strings.TrimSuffix(name, ".db")
				if id != "" && !strings.HasPrefix(id, ".") {
					pre[id] = true
				}
			}
		}
	}

	return pre
}

func (as *antigravitySession) detectNewSessionID(preEntries map[string]bool, sendStartedAt time.Time) string {
	dataDir, err := antigravityDataDir()
	if err != nil {
		return ""
	}

	type candidate struct {
		sessionID string
		modTime   time.Time
		diff      time.Duration
	}

	var candidates []candidate
	seen := make(map[string]bool)

	checkCandidate := func(id string, modTime time.Time) {
		if id == "" || strings.HasPrefix(id, ".") || preEntries[id] || seen[id] {
			return
		}
		seen[id] = true

		if !sendStartedAt.IsZero() && modTime.Before(sendStartedAt.Add(-2*time.Second)) {
			return
		}

		diff := time.Duration(0)
		if !sendStartedAt.IsZero() {
			if modTime.After(sendStartedAt) {
				diff = modTime.Sub(sendStartedAt)
			} else {
				diff = sendStartedAt.Sub(modTime)
			}
		}

		candidates = append(candidates, candidate{
			sessionID: id,
			modTime:   modTime,
			diff:      diff,
		})
	}

	// 1. Scan brain/
	brainDir := filepath.Join(dataDir, "brain")
	if entries, err := os.ReadDir(brainDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			id := entry.Name()
			if preEntries[id] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			modTime := info.ModTime()
			transcriptPath := filepath.Join(brainDir, id, ".system_generated", "logs", "transcript.jsonl")
			if tInfo, err := os.Stat(transcriptPath); err == nil {
				if tInfo.ModTime().After(modTime) {
					modTime = tInfo.ModTime()
				}
			}
			checkCandidate(id, modTime)
		}
	}

	// 2. Scan conversations/
	convDir := filepath.Join(dataDir, "conversations")
	if entries, err := os.ReadDir(convDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".db") {
				continue
			}
			id := strings.TrimSuffix(name, ".db")
			if preEntries[id] || seen[id] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			checkCandidate(id, info.ModTime())
		}
	}

	if len(candidates) == 0 {
		return ""
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].diff == candidates[j].diff {
			return candidates[i].modTime.After(candidates[j].modTime)
		}
		return candidates[i].diff < candidates[j].diff
	})

	return candidates[0].sessionID
}

func (as *antigravitySession) RespondPermission(requestID string, result core.PermissionResult) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	if as.permissionBridge == nil {
		return fmt.Errorf("antigravity: permission responses are only available in default mode")
	}
	return as.permissionBridge.RespondPermission(requestID, result)
}

func (as *antigravitySession) Events() <-chan core.Event {
	return as.events
}

func (as *antigravitySession) CurrentSessionID() string {
	v, _ := as.chatID.Load().(string)
	return v
}

func (as *antigravitySession) Alive() bool {
	return as.alive.Load()
}

func (as *antigravitySession) Close() error {
	as.alive.Store(false)
	as.cancel()
	if as.permissionBridge != nil {
		as.permissionBridge.Close()
	}
	done := make(chan struct{})
	go func() {
		as.wg.Wait()
		as.closeOnce.Do(func() {
			close(as.events)
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("antigravitySession: close timed out")
	}
	return nil
}
