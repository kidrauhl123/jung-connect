package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ChannelProfile is a Hermes-style per-channel configuration
// ([[projects.channels]]). It is matched against "platform:channelID";
// Discord threads inherit the profile of their parent channel.
type ChannelProfile struct {
	Match           string
	Prompt          string
	PromptFile      string
	Skills          []string
	Model           string
	ReasoningEffort string
	WorkDir         string
}

// ResolvedSkill is a skill reference resolved to a directory containing SKILL.md.
type ResolvedSkill struct {
	Name string
	Dir  string
}

// ChannelSessionProfile is what an agent receives right before StartSession
// for a session that belongs to a configured channel.
type ChannelSessionProfile struct {
	Match           string
	Prompt          string // channel prompt plus the bound-skill section
	Model           string // effective default model (per-channel /model override wins)
	ReasoningEffort string
	Skills          []ResolvedSkill
	// AgentProfilePath is a generated Grok agent profile (.md with YAML
	// frontmatter, prompt_mode: full) for ACP agents that accept
	// --agent-profile.
	AgentProfilePath string
	// PluginDir is a generated plugin directory whose skills/ folder links
	// the bound skills; usable with --plugin-dir (Grok Build, Claude Code).
	PluginDir string
}

// ChannelProfileInjector is implemented by agents that can apply a channel
// profile natively (system prompt / agent profile / plugin dir / model).
//
// SetChannelProfile stages the profile for the NEXT StartSession call only;
// StartSession must consume (clear) it so that sessions started from other
// code paths (cron, relay, …) never inherit a stale channel profile. Passing
// nil clears any staged profile.
type ChannelProfileInjector interface {
	SetChannelProfile(p *ChannelSessionProfile)
}

type channelProfileStore struct {
	mu      sync.RWMutex
	byMatch map[string]*ChannelProfile
	// sessionChannel remembers the parent channel key ("discord:<parent>")
	// observed for a raw session key, so thread sessions resolve to their
	// parent channel's profile from command/session-start paths that only
	// have the session key.
	sessionChannel map[string]string
}

// NormalizeChannelProfiles trims fields, expands ~, reads prompt_file
// (relative paths resolve against baseDir) and drops empty matches.
func NormalizeChannelProfiles(in []ChannelProfile, baseDir string) ([]ChannelProfile, error) {
	out := make([]ChannelProfile, 0, len(in))
	for _, cp := range in {
		cp.Match = strings.TrimSpace(cp.Match)
		if cp.Match == "" {
			continue
		}
		cp.Prompt = strings.TrimSpace(cp.Prompt)
		if pf := strings.TrimSpace(cp.PromptFile); pf != "" {
			path := expandHomePath(pf)
			if !filepath.IsAbs(path) && baseDir != "" {
				path = filepath.Join(baseDir, path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("channel %s: read prompt_file: %w", cp.Match, err)
			}
			text := strings.TrimSpace(string(data))
			if text != "" {
				if cp.Prompt != "" {
					cp.Prompt += "\n\n" + text
				} else {
					cp.Prompt = text
				}
			}
			cp.PromptFile = path
		}
		var skills []string
		for _, s := range cp.Skills {
			if s = strings.TrimSpace(s); s != "" {
				skills = append(skills, s)
			}
		}
		cp.Skills = skills
		cp.Model = strings.TrimSpace(cp.Model)
		cp.ReasoningEffort = strings.TrimSpace(cp.ReasoningEffort)
		if wd := strings.TrimSpace(cp.WorkDir); wd != "" {
			wd = expandHomePath(wd)
			if abs, err := filepath.Abs(wd); err == nil {
				wd = abs
			}
			cp.WorkDir = wd
		} else {
			cp.WorkDir = ""
		}
		out = append(out, cp)
	}
	return out, nil
}

func expandHomePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// SetChannelProfiles replaces the engine's channel profiles (startup and
// /config reload). Profiles must already be normalized.
func (e *Engine) SetChannelProfiles(profiles []ChannelProfile) {
	m := make(map[string]*ChannelProfile, len(profiles))
	for i := range profiles {
		cp := profiles[i]
		m[cp.Match] = &cp
	}
	e.channelProfiles.mu.Lock()
	e.channelProfiles.byMatch = m
	e.channelProfiles.mu.Unlock()
	if len(m) > 0 {
		slog.Info("channel profiles loaded", "project", e.name, "count", len(m))
	}
}

// ChannelProfileCount reports how many channel profiles are configured.
func (e *Engine) ChannelProfileCount() int {
	e.channelProfiles.mu.RLock()
	defer e.channelProfiles.mu.RUnlock()
	return len(e.channelProfiles.byMatch)
}

// channelProfileFor returns the first profile matching one of keys, in order.
func (e *Engine) channelProfileFor(keys ...string) *ChannelProfile {
	e.channelProfiles.mu.RLock()
	defer e.channelProfiles.mu.RUnlock()
	if len(e.channelProfiles.byMatch) == 0 {
		return nil
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if cp := e.channelProfiles.byMatch[k]; cp != nil {
			return cp
		}
	}
	return nil
}

// rememberSessionChannel records the parent channel key for a raw session key.
func (e *Engine) rememberSessionChannel(sessionKey, channelKey string) {
	if sessionKey == "" || channelKey == "" {
		return
	}
	e.channelProfiles.mu.Lock()
	defer e.channelProfiles.mu.Unlock()
	if e.channelProfiles.sessionChannel == nil {
		e.channelProfiles.sessionChannel = make(map[string]string)
	}
	e.channelProfiles.sessionChannel[sessionKey] = channelKey
}

// channelKeysForMessage returns match candidates for a message: the exact
// channel/thread first, then the platform-provided parent channel key.
func channelKeysForMessage(msg *Message) []string {
	if msg == nil {
		return nil
	}
	exact := extractWorkspaceChannelKey(msg.SessionKey)
	parent := effectiveWorkspaceChannelKey(msg)
	if exact == parent {
		return []string{exact}
	}
	return []string{exact, parent}
}

// channelProfileForMessage resolves the profile for an inbound message and
// remembers the session→channel mapping for later session-key lookups.
func (e *Engine) channelProfileForMessage(msg *Message) *ChannelProfile {
	if msg == nil {
		return nil
	}
	keys := channelKeysForMessage(msg)
	if len(keys) > 0 {
		e.rememberSessionChannel(msg.SessionKey, keys[len(keys)-1])
	}
	return e.channelProfileFor(keys...)
}

// channelProfileForSessionKey resolves a profile from a raw (or
// workspace-prefixed) session key.
func (e *Engine) channelProfileForSessionKey(sessionKey string) *ChannelProfile {
	if sessionKey == "" {
		return nil
	}
	e.channelProfiles.mu.RLock()
	empty := len(e.channelProfiles.byMatch) == 0
	parent := e.channelProfiles.sessionChannel[sessionKey]
	if parent == "" {
		// Workspace-prefixed interactive key ("<workspace>:<sessionKey>").
		for raw, ch := range e.channelProfiles.sessionChannel {
			if strings.HasSuffix(sessionKey, ":"+raw) {
				parent = ch
				sessionKey = raw
				break
			}
		}
	}
	e.channelProfiles.mu.RUnlock()
	if empty {
		return nil
	}
	return e.channelProfileFor(extractWorkspaceChannelKey(sessionKey), parent)
}

// channelWorkDirForKey returns a configured work_dir for a channel key, if any.
func (e *Engine) channelWorkDirForKey(keys ...string) string {
	if cp := e.channelProfileFor(keys...); cp != nil {
		return cp.WorkDir
	}
	return ""
}

// effectiveChannelModel returns the per-channel /model override, else the
// configured channel model.
func (e *Engine) effectiveChannelModel(cp *ChannelProfile) string {
	if cp == nil {
		return ""
	}
	if e.projectState != nil {
		if m := e.projectState.ChannelModelOverride(cp.Match); m != "" {
			return m
		}
	}
	return cp.Model
}

// skillSearchRoots lists directories searched for a skill name, highest
// priority first.
func skillSearchRoots(workDir string) []string {
	var roots []string
	if workDir != "" {
		for _, d := range []string{".grok/skills", ".claude/skills", ".agents/skills"} {
			roots = append(roots, filepath.Join(workDir, d))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{".grok/skills", ".claude/skills", ".agents/skills", ".hermes/skills"} {
			roots = append(roots, filepath.Join(home, d))
		}
	}
	return roots
}

func isSkillDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !st.IsDir()
}

// resolveSkill resolves a skill reference (name or path) to a directory.
// Names are searched directly under each root and one category level deeper
// (Hermes keeps skills in <root>/<category>/<name>).
func resolveSkill(ref string, roots []string) (ResolvedSkill, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ResolvedSkill{}, false
	}
	if strings.HasPrefix(ref, "~") || filepath.IsAbs(ref) || strings.ContainsRune(ref, os.PathSeparator) {
		dir := expandHomePath(ref)
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if isSkillDir(dir) {
			return ResolvedSkill{Name: filepath.Base(dir), Dir: dir}, true
		}
		return ResolvedSkill{}, false
	}
	for _, root := range roots {
		if dir := filepath.Join(root, ref); isSkillDir(dir) {
			return ResolvedSkill{Name: ref, Dir: dir}, true
		}
	}
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*", ref))
		sort.Strings(matches)
		for _, dir := range matches {
			if isSkillDir(dir) {
				return ResolvedSkill{Name: ref, Dir: dir}, true
			}
		}
	}
	return ResolvedSkill{}, false
}

// renderChannelPrompt combines the channel prompt with a section listing the
// bound skills, so every agent type learns about them even without native
// plugin support.
func renderChannelPrompt(prompt string, skills []ResolvedSkill) string {
	var sb strings.Builder
	if p := strings.TrimSpace(prompt); p != "" {
		sb.WriteString(p)
	}
	if len(skills) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("## Channel skills\n")
		sb.WriteString("This channel has the following skills bound. Before working on a task here, read the matching SKILL.md and follow it:\n")
		for _, s := range skills {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", s.Name, filepath.Join(s.Dir, "SKILL.md")))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderGrokAgentProfile renders a Grok Build agent profile. prompt_mode:
// full appends the body to the default system prompt (tools and identity
// are kept); model: inherit keeps -m / session model selection in charge.
func renderGrokAgentProfile(name, match, prompt string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("name: " + name + "\n")
	sb.WriteString("description: " + strconvQuote("cc-connect channel profile for "+match) + "\n")
	sb.WriteString("prompt_mode: full\n")
	sb.WriteString("model: inherit\n")
	sb.WriteString("agents_md: true\n")
	sb.WriteString("---\n\n")
	sb.WriteString(prompt)
	sb.WriteString("\n")
	return sb.String()
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// channelProfileHash identifies the generated artifacts for a profile.
func channelProfileHash(match, prompt string, skills []ResolvedSkill) string {
	h := sha256.New()
	h.Write([]byte(match))
	h.Write([]byte{0})
	h.Write([]byte(prompt))
	for _, s := range skills {
		h.Write([]byte{0})
		h.Write([]byte(s.Name + "=" + s.Dir))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (e *Engine) channelArtifactsDir() string {
	if e.dataDir != "" {
		return e.dataDir
	}
	return filepath.Join(os.TempDir(), "cc-connect-"+sanitizeArtifactName(e.name))
}

func sanitizeArtifactName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// writeFileIfChanged writes data to path only when content differs.
func writeFileIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// buildChannelSessionProfile resolves skills and materializes the generated
// Grok agent profile and plugin directory under
// <data_dir>/channel-profiles/<hash>.md and <data_dir>/channel-plugins/<hash>/.
func (e *Engine) buildChannelSessionProfile(cp *ChannelProfile, workDir string) *ChannelSessionProfile {
	if cp == nil {
		return nil
	}
	roots := skillSearchRoots(workDir)
	var skills []ResolvedSkill
	seen := map[string]bool{}
	for _, ref := range cp.Skills {
		s, ok := resolveSkill(ref, roots)
		if !ok {
			slog.Warn("channel profile: skill not found", "match", cp.Match, "skill", ref)
			continue
		}
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		skills = append(skills, s)
	}
	prompt := renderChannelPrompt(cp.Prompt, skills)
	sp := &ChannelSessionProfile{
		Match:           cp.Match,
		Prompt:          prompt,
		Model:           e.effectiveChannelModel(cp),
		ReasoningEffort: cp.ReasoningEffort,
		Skills:          skills,
	}
	hash := channelProfileHash(cp.Match, prompt, skills)
	base := e.channelArtifactsDir()
	if prompt != "" {
		path := filepath.Join(base, "channel-profiles", hash+".md")
		body := renderGrokAgentProfile("cc-channel-"+hash, cp.Match, prompt)
		if err := writeFileIfChanged(path, []byte(body)); err != nil {
			slog.Warn("channel profile: write agent profile failed", "match", cp.Match, "path", path, "err", err)
		} else {
			sp.AgentProfilePath = path
		}
	}
	if len(skills) > 0 {
		dir := filepath.Join(base, "channel-plugins", hash)
		if err := materializeSkillPlugin(dir, "cc-channel-"+hash, skills); err != nil {
			slog.Warn("channel profile: build plugin dir failed", "match", cp.Match, "dir", dir, "err", err)
		} else {
			sp.PluginDir = dir
		}
	}
	return sp
}

// materializeSkillPlugin builds a minimal plugin directory:
//
//	<dir>/.claude-plugin/plugin.json   (name/description; accepted by Grok and Claude Code)
//	<dir>/skills/<name>/...            (copy of each bound skill directory)
//
// Skills are copied (not symlinked) because plugin loaders may skip symlinks.
func materializeSkillPlugin(dir, name string, skills []ResolvedSkill) error {
	manifest := map[string]any{
		"name":        name,
		"description": "cc-connect channel skills",
		"version":     "0.0.0",
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	if err := writeFileIfChanged(filepath.Join(dir, ".claude-plugin", "plugin.json"), append(data, '\n')); err != nil {
		return err
	}
	skillsDir := filepath.Join(dir, "skills")
	// Rebuild only when the source skill trees changed, so a running agent
	// never sees its skills directory swapped underneath it needlessly.
	stamp := skillTreesStamp(skills)
	stampPath := filepath.Join(dir, ".cc-connect-stamp")
	if old, err := os.ReadFile(stampPath); err == nil && string(old) == stamp {
		if st, err := os.Stat(skillsDir); err == nil && st.IsDir() {
			return nil
		}
	}
	tmp := skillsDir + ".tmp"
	_ = os.RemoveAll(tmp)
	for _, s := range skills {
		if err := copySkillTree(s.Dir, filepath.Join(tmp, s.Name)); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	_ = os.RemoveAll(skillsDir)
	if err := os.Rename(tmp, skillsDir); err != nil {
		return err
	}
	return writeFileIfChanged(stampPath, []byte(stamp))
}

// skillTreesStamp fingerprints skill sources by relative path, size and mtime.
func skillTreesStamp(skills []ResolvedSkill) string {
	h := sha256.New()
	for _, s := range skills {
		fmt.Fprintf(h, "skill %s %s\n", s.Name, s.Dir)
		_ = filepath.Walk(s.Dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() && info.Name() == ".git" {
				return filepath.SkipDir
			}
			if info.Mode().IsRegular() {
				rel, _ := filepath.Rel(s.Dir, path)
				fmt.Fprintf(h, "%s %d %d\n", rel, info.Size(), info.ModTime().UnixNano())
			}
			return nil
		})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// copySkillTree copies a skill directory (regular files and dirs only,
// skipping .git and symlinks).
func copySkillTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		default:
			return nil
		}
	})
}

// channelContextBlock is the first-message fallback for agents without
// native channel-profile support.
func channelContextBlock(sp *ChannelSessionProfile) string {
	if sp == nil || strings.TrimSpace(sp.Prompt) == "" {
		return ""
	}
	return "<channel-context>\n" + sp.Prompt + "\n</channel-context>\n\n"
}
