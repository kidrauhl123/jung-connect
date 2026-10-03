package core

import (
	"strings"
)

// ModelVendor describes a first-level /model vendor bucket.
type ModelVendor struct {
	Key   string // short key for callbacks (c/o/x/g/d/m/l/q/k/n/b/y/h/e/u/s/r/p/z)
	Label string // UI label (matches grok2tg)
}

// ModelVendors is the stable first-level order for the /model menu.
// Only vendors with ≥1 model should be shown.
var ModelVendors = []ModelVendor{
	{Key: "c", Label: "Anthropic/Claude"},
	{Key: "o", Label: "OpenAI"},
	{Key: "x", Label: "xAI/Grok"},
	{Key: "g", Label: "Google"},
	{Key: "d", Label: "DeepSeek"},
	{Key: "m", Label: "mimo"},
	{Key: "l", Label: "Zhipu/GLM"},
	{Key: "q", Label: "Qwen"},
	{Key: "k", Label: "Moonshot/Kimi"},
	{Key: "n", Label: "MiniMax"},
	{Key: "b", Label: "Doubao"},
	{Key: "y", Label: "Yi"},
	{Key: "h", Label: "Baichuan"},
	{Key: "e", Label: "ERNIE"},
	{Key: "u", Label: "Hunyuan"},
	{Key: "s", Label: "StepFun"},
	{Key: "r", Label: "Mistral"},
	{Key: "p", Label: "Meta/Llama"},
	{Key: "z", Label: "Other"},
}

var modelVendorByKey = func() map[string]ModelVendor {
	m := make(map[string]ModelVendor, len(ModelVendors))
	for _, v := range ModelVendors {
		m[v.Key] = v
	}
	return m
}()

// ModelVendorLabel returns the UI label for a vendor key.
func ModelVendorLabel(key string) string {
	if v, ok := modelVendorByKey[key]; ok {
		return v.Label
	}
	return "Other"
}

// IsModelVendorKey reports whether key is a known vendor key.
func IsModelVendorKey(key string) bool {
	_, ok := modelVendorByKey[key]
	return ok
}

// ResolveModelVendorKey maps an explicit group string (toml `group`, label, or key)
// onto a vendor key. Empty input yields "".
func ResolveModelVendorKey(group string) string {
	g := strings.TrimSpace(strings.ToLower(group))
	if g == "" {
		return ""
	}
	if IsModelVendorKey(g) {
		return g
	}
	for _, v := range ModelVendors {
		if strings.EqualFold(v.Label, group) || strings.EqualFold(v.Key, g) {
			return v.Key
		}
	}
	// Common aliases
	switch g {
	case "claude", "anthropic", "anthropic/claude":
		return "c"
	case "openai", "gpt":
		return "o"
	case "xai", "grok", "xai/grok":
		return "x"
	case "google", "gemini":
		return "g"
	case "deepseek":
		return "d"
	case "mimo", "xiaomi", "xiaomimimo":
		return "m"
	case "glm", "zhipu", "chatglm", "zhipu/glm":
		return "l"
	case "qwen", "alibaba", "tongyi", "qwq", "qvq":
		return "q"
	case "moonshot", "kimi", "moonshot/kimi":
		return "k"
	case "minimax", "abab":
		return "n"
	case "doubao", "bytedance", "seed":
		return "b"
	case "yi", "01.ai", "01ai", "lingyi":
		return "y"
	case "baichuan":
		return "h"
	case "ernie", "baidu", "wenxin":
		return "e"
	case "hunyuan", "tencent":
		return "u"
	case "stepfun", "step":
		return "s"
	case "mistral", "mixtral", "codestral", "pixtral":
		return "r"
	case "meta", "llama", "meta/llama", "meta-llama":
		return "p"
	case "other":
		return "z"
	}
	return ""
}

// ClassifyModelVendor classifies a model into a vendor key.
// Explicit Group (when set) wins; otherwise heuristics match grok2tg:
// prefer last path segment of the model id, then the full id+desc haystack.
func ClassifyModelVendor(name, desc, explicitGroup string) string {
	if key := ResolveModelVendorKey(explicitGroup); key != "" {
		return key
	}
	id := strings.ToLower(strings.TrimSpace(name))
	base := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		base = id[i+1:]
	}
	base = strings.TrimLeft(base, " \t-_.")
	// strip leading non-alnum roughly like grok2tg
	for len(base) > 0 {
		c := base[0]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			break
		}
		base = base[1:]
	}
	hay := strings.ToLower(id + " " + desc)

	if startsWithAny(base, "claude", "anthropic") || containsWord(hay, "claude", "anthropic") {
		return "c"
	}
	if startsWithAny(base, "gpt", "o1", "o3", "o4", "codex", "openai") ||
		containsWord(hay, "gpt-", "openai", "codex") ||
		hasOFamily(hay) {
		return "o"
	}
	if startsWithAny(base, "grok", "xai") || containsWord(hay, "grok", "xai") {
		return "x"
	}
	if startsWithAny(base, "gemini", "google") || containsWord(hay, "gemini", "google") {
		return "g"
	}
	if startsWithAny(base, "deepseek") || containsWord(hay, "deepseek") {
		return "d"
	}
	if startsWithAny(base, "mimo", "xiaomi", "xiaomimimo") || containsWord(hay, "mimo", "xiaomi", "xiaomimimo") {
		return "m"
	}
	if startsWithAny(base, "glm", "chatglm", "zhipu") || containsWord(hay, "glm-", "chatglm", "zhipu") {
		return "l"
	}
	if startsWithAny(base, "qwen", "qwq", "qvq", "tongyi") || containsWord(hay, "qwen", "qwq", "qvq", "tongyi") {
		return "q"
	}
	if startsWithAny(base, "kimi", "moonshot") || containsWord(hay, "kimi", "moonshot") {
		return "k"
	}
	if startsWithAny(base, "minimax", "abab") || containsWord(hay, "minimax", "abab") {
		return "n"
	}
	if startsWithAny(base, "doubao", "seed-") || containsWord(hay, "doubao", "seed-") {
		return "b"
	}
	if startsWithAny(base, "yi-") || containsWord(hay, "yi-") {
		return "y"
	}
	if startsWithAny(base, "baichuan") || containsWord(hay, "baichuan") {
		return "h"
	}
	if startsWithAny(base, "ernie", "wenxin") || containsWord(hay, "ernie", "wenxin") {
		return "e"
	}
	if startsWithAny(base, "hunyuan") || containsWord(hay, "hunyuan") {
		return "u"
	}
	if startsWithAny(base, "step-") || containsWord(hay, "step-", "stepfun") {
		return "s"
	}
	if startsWithAny(base, "mistral", "mixtral", "codestral", "pixtral") ||
		containsWord(hay, "mistral", "mixtral", "codestral", "pixtral") {
		return "r"
	}
	if startsWithAny(base, "llama", "meta-llama") || containsWord(hay, "llama", "meta-llama") {
		return "p"
	}
	return "z"
}

// ModelVendorGroup holds global indices of models belonging to one vendor.
type ModelVendorGroup struct {
	Key     string
	Label   string
	Indices []int // indices into the original models slice
}

// GroupModelsByVendor groups models by vendor; only non-empty vendors are returned,
// in ModelVendors order. Global indices stay stable for /model switch N.
func GroupModelsByVendor(models []ModelOption) []ModelVendorGroup {
	buckets := make(map[string][]int, len(ModelVendors))
	for _, v := range ModelVendors {
		buckets[v.Key] = nil
	}
	for i, m := range models {
		key := ClassifyModelVendor(m.Name, m.Desc, m.Group)
		buckets[key] = append(buckets[key], i)
	}
	out := make([]ModelVendorGroup, 0, len(ModelVendors))
	for _, v := range ModelVendors {
		indices := buckets[v.Key]
		if len(indices) == 0 {
			continue
		}
		out = append(out, ModelVendorGroup{Key: v.Key, Label: v.Label, Indices: indices})
	}
	return out
}

func startsWithAny(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func containsWord(hay string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(hay, w) {
			return true
		}
	}
	return false
}

func hasOFamily(hay string) bool {
	// Match o1-/o3-/o4- style ids similar to grok2tg /\bo[134][-.]/
	for _, p := range []string{"o1-", "o1.", "o3-", "o3.", "o4-", "o4."} {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}
