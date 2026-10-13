package acp

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/chenhg5/cc-connect/core"
)

// acpUsageState holds the latest context-window load reported by the agent.
// Sources (in priority for UsedTokens when several arrive):
//   - standard ACP usage_update
//   - grok _x.ai/session_notification response_completed / turn_completed
//   - session/prompt result _meta.totalTokens (final call context size)
type acpUsageState struct {
	mu     sync.Mutex
	usage  *core.ContextUsage
	window int // context window for the active model (from session/new models)
}

func (s *acpSession) usageState() *acpUsageState { return &s.usage }

func (s *acpSession) setContextWindow(size int) {
	if size <= 0 {
		return
	}
	st := s.usageState()
	st.mu.Lock()
	st.window = size
	if st.usage == nil {
		st.usage = &core.ContextUsage{ContextWindow: size}
	} else {
		st.usage.ContextWindow = size
	}
	st.mu.Unlock()
}

func (s *acpSession) recordContextUsed(used int, source string) {
	if used <= 0 {
		return
	}
	st := s.usageState()
	st.mu.Lock()
	window := st.window
	if st.usage != nil && st.usage.ContextWindow > 0 {
		window = st.usage.ContextWindow
	}
	st.usage = &core.ContextUsage{UsedTokens: used, ContextWindow: window}
	st.mu.Unlock()
	slog.Info("acp: context usage",
		"session_id", s.currentACPSessionID(),
		"source", source,
		"used", used,
		"window", window,
	)
}

// GetContextUsage implements core.ContextUsageReporter.
func (s *acpSession) GetContextUsage() *core.ContextUsage {
	st := s.usageState()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.usage == nil || st.usage.UsedTokens <= 0 {
		return nil
	}
	u := *st.usage
	return &u
}

var _ core.ContextUsageReporter = (*acpSession)(nil)

// maybeAbsorbUsageUpdate records standard ACP usage_update notifications.
func (s *acpSession) maybeAbsorbUsageUpdate(params json.RawMessage) {
	used, size, ok := parseUsageUpdate(params)
	if !ok {
		return
	}
	if size > 0 {
		s.setContextWindow(size)
	}
	s.recordContextUsed(used, "usage_update")
}

func parseUsageUpdate(params json.RawMessage) (used, size int, ok bool) {
	var wrap struct {
		Update struct {
			SessionUpdate string   `json:"sessionUpdate"`
			Used          *float64 `json:"used"`
			Size          *float64 `json:"size"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &wrap) != nil || wrap.Update.SessionUpdate != "usage_update" || wrap.Update.Used == nil {
		return 0, 0, false
	}
	used = int(*wrap.Update.Used)
	if wrap.Update.Size != nil {
		size = int(*wrap.Update.Size)
	}
	return used, size, true
}

// maybeAbsorbGrokUsageNotification handles grok's
// `_x.ai/session_notification` variants:
//   - response_completed: per-call usage (snake_case); live ctx = input + cache_read
//   - turn_completed: turn cumulative (camelCase); live ctx ≈ last call via total-output
func (s *acpSession) maybeAbsorbGrokUsageNotification(params json.RawMessage) {
	var wrap struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Usage         json.RawMessage `json:"usage"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &wrap) != nil || len(wrap.Update.Usage) == 0 {
		return
	}
	switch wrap.Update.SessionUpdate {
	case "response_completed":
		in, out, cacheRead, ok := parseSnakeUsage(wrap.Update.Usage)
		if !ok {
			return
		}
		used := in + cacheRead
		if used <= 0 {
			used = in + out
		}
		s.recordContextUsed(used, "response_completed")
	case "turn_completed":
		u, ok := parseCamelUsage(wrap.Update.Usage)
		if !ok {
			return
		}
		// Prefer last-call context estimate: totalTokens - outputTokens ≈ input+cache.
		used := u.total - u.output
		if used <= 0 {
			used = u.total
		}
		if used > 0 {
			s.recordContextUsed(used, "turn_completed")
		}
	}
}

func parseSnakeUsage(raw json.RawMessage) (input, output, cacheRead int, ok bool) {
	var u struct {
		InputTokens              *float64 `json:"input_tokens"`
		OutputTokens             *float64 `json:"output_tokens"`
		CacheReadInputTokens     *float64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *float64 `json:"cache_creation_input_tokens"`
	}
	if json.Unmarshal(raw, &u) != nil || u.InputTokens == nil {
		return 0, 0, 0, false
	}
	input = int(*u.InputTokens)
	if u.OutputTokens != nil {
		output = int(*u.OutputTokens)
	}
	if u.CacheReadInputTokens != nil {
		cacheRead = int(*u.CacheReadInputTokens)
	}
	return input, output, cacheRead, true
}

type camelUsage struct {
	total, input, output, cachedRead, modelCalls int
}

func parseCamelUsage(raw json.RawMessage) (camelUsage, bool) {
	var u struct {
		TotalTokens      *float64 `json:"totalTokens"`
		InputTokens      *float64 `json:"inputTokens"`
		OutputTokens     *float64 `json:"outputTokens"`
		CachedReadTokens *float64 `json:"cachedReadTokens"`
		ModelCalls       *float64 `json:"modelCalls"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return camelUsage{}, false
	}
	n := func(p *float64) int {
		if p == nil {
			return 0
		}
		return int(*p)
	}
	cu := camelUsage{
		total: n(u.TotalTokens), input: n(u.InputTokens), output: n(u.OutputTokens),
		cachedRead: n(u.CachedReadTokens), modelCalls: n(u.ModelCalls),
	}
	if cu.total <= 0 && cu.input <= 0 && cu.output <= 0 {
		return camelUsage{}, false
	}
	return cu, true
}

// absorbModelsMeta records the active model's context window from a
// session/new / session/load / _x.ai/models/update payload.
func (s *acpSession) absorbModelsMeta(raw json.RawMessage) {
	window, modelID, ok := contextWindowFromModels(raw)
	if !ok {
		return
	}
	s.setContextWindow(window)
	slog.Info("acp: model context window",
		"session_id", s.currentACPSessionID(),
		"source", "models_meta",
		"model", modelID,
		"window", window,
	)
}

func contextWindowFromModels(raw json.RawMessage) (window int, modelID string, ok bool) {
	var root struct {
		Models *struct {
			CurrentModelID  string `json:"currentModelId"`
			AvailableModels []struct {
				ModelID string `json:"modelId"`
				Meta    *struct {
					TotalContextTokens *float64 `json:"totalContextTokens"`
				} `json:"_meta"`
			} `json:"availableModels"`
		} `json:"models"`
		// _x.ai/models/update is flat (no "models" wrapper).
		CurrentModelID  string `json:"currentModelId"`
		AvailableModels []struct {
			ModelID string `json:"modelId"`
			Meta    *struct {
				TotalContextTokens *float64 `json:"totalContextTokens"`
			} `json:"_meta"`
		} `json:"availableModels"`
	}
	if json.Unmarshal(raw, &root) != nil {
		return 0, "", false
	}
	cur := root.CurrentModelID
	models := root.AvailableModels
	if root.Models != nil {
		if cur == "" {
			cur = root.Models.CurrentModelID
		}
		if len(models) == 0 {
			models = root.Models.AvailableModels
		}
	}
	if cur == "" || len(models) == 0 {
		return 0, "", false
	}
	for _, m := range models {
		if m.ModelID == cur && m.Meta != nil && m.Meta.TotalContextTokens != nil {
			w := int(*m.Meta.TotalContextTokens)
			if w > 0 {
				return w, cur, true
			}
		}
	}
	// Fallback: first model with a window.
	for _, m := range models {
		if m.Meta != nil && m.Meta.TotalContextTokens != nil {
			w := int(*m.Meta.TotalContextTokens)
			if w > 0 {
				return w, m.ModelID, true
			}
		}
	}
	return 0, cur, false
}

// promptUsage is the Done-line token total extracted from a PromptResponse.
type promptUsage struct {
	TurnTokens int    // preferred: _meta.usage.totalTokens (turn cumulative)
	Source     string // "meta.usage" | "meta.total" | "usage" | "absent"
	CtxUsed    int    // last-call context size (_meta.totalTokens)
}

// parsePromptUsage logs token counts and returns the turn total for the
// status Done line. Grok puts numbers under `_meta.usage` (turn) and
// `_meta.totalTokens` (last call context); standard ACP uses top-level `usage`.
func parsePromptUsage(sid string, res json.RawMessage) promptUsage {
	var r struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			TotalTokens       *float64 `json:"totalTokens"`
			InputTokens       *float64 `json:"inputTokens"`
			OutputTokens      *float64 `json:"outputTokens"`
			ThoughtTokens     *float64 `json:"thoughtTokens"`
			CachedReadTokens  *float64 `json:"cachedReadTokens"`
			CachedWriteTokens *float64 `json:"cachedWriteTokens"`
		} `json:"usage"`
		Meta json.RawMessage `json:"_meta"`
	}
	out := promptUsage{Source: "absent"}
	if len(res) == 0 || json.Unmarshal(res, &r) != nil {
		slog.Info("acp: prompt usage", "session_id", sid, "usage", "absent")
		return out
	}
	n := func(p *float64) int {
		if p == nil {
			return -1
		}
		return int(*p)
	}
	metaKeys := metaKeyList(r.Meta)

	// Grok: _meta.usage = turn cumulative; _meta.totalTokens = last-call context.
	if mu, ctx, ok := parseGrokMetaUsage(r.Meta); ok {
		out.TurnTokens = mu.total
		out.CtxUsed = ctx
		out.Source = "meta.usage"
		if out.TurnTokens <= 0 {
			out.TurnTokens = mu.input + mu.output
			out.Source = "meta.usage.sum"
		}
		slog.Info("acp: prompt usage", "session_id", sid, "stop_reason", r.StopReason,
			"source", out.Source,
			"turn_total", out.TurnTokens, "turn_input", mu.input, "turn_output", mu.output,
			"turn_cached_read", mu.cachedRead, "model_calls", mu.modelCalls,
			"ctx_total", out.CtxUsed, "meta_keys", metaKeys)
		return out
	}

	if r.Usage != nil {
		u := r.Usage
		out.Source = "usage"
		if t := n(u.TotalTokens); t > 0 {
			out.TurnTokens = t
		} else {
			if v := n(u.InputTokens); v > 0 {
				out.TurnTokens += v
			}
			if v := n(u.OutputTokens); v > 0 {
				out.TurnTokens += v
			}
			out.Source = "usage.sum"
		}
		slog.Info("acp: prompt usage", "session_id", sid, "stop_reason", r.StopReason,
			"source", out.Source,
			"total", n(u.TotalTokens), "input", n(u.InputTokens), "output", n(u.OutputTokens),
			"thought", n(u.ThoughtTokens), "cached_read", n(u.CachedReadTokens),
			"cached_write", n(u.CachedWriteTokens), "meta_keys", metaKeys)
		return out
	}

	// Fallback: bare _meta.totalTokens as both ctx and turn.
	if ctx := metaInt(r.Meta, "totalTokens"); ctx > 0 {
		out.TurnTokens = ctx
		out.CtxUsed = ctx
		out.Source = "meta.total"
		slog.Info("acp: prompt usage", "session_id", sid, "stop_reason", r.StopReason,
			"source", out.Source, "total", ctx, "meta_keys", metaKeys)
		return out
	}

	slog.Info("acp: prompt usage", "session_id", sid, "stop_reason", r.StopReason,
		"usage", "absent", "meta_keys", metaKeys)
	return out
}

func parseGrokMetaUsage(meta json.RawMessage) (cu camelUsage, ctxUsed int, ok bool) {
	if len(meta) == 0 {
		return camelUsage{}, 0, false
	}
	var m struct {
		TotalTokens *float64        `json:"totalTokens"`
		Usage       json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(meta, &m) != nil || len(m.Usage) == 0 {
		return camelUsage{}, 0, false
	}
	cu, ok = parseCamelUsage(m.Usage)
	if !ok {
		return camelUsage{}, 0, false
	}
	if m.TotalTokens != nil {
		ctxUsed = int(*m.TotalTokens)
	}
	return cu, ctxUsed, true
}

func metaInt(meta json.RawMessage, key string) int {
	if len(meta) == 0 {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(meta, &m) != nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	}
	return 0
}

func metaKeyList(meta json.RawMessage) []string {
	if len(meta) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(meta, &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
