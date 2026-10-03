package core

import "testing"

func TestClassifyModelVendor(t *testing.T) {
	tests := []struct {
		name, desc, group, want string
	}{
		{"claude-sonnet-4-6", "", "", "c"},
		{"anthropic/claude-3-5-sonnet", "", "", "c"},
		{"gpt-5.6", "", "", "o"},
		{"gpt-5.6-luna", "", "", "o"},
		{"o3-mini", "", "", "o"},
		{"codex-auto-review", "", "", "o"},
		{"grok-4.7", "", "", "x"},
		{"xai/grok-code-fast-1", "", "", "x"},
		{"gemini-3-flash", "", "", "g"},
		{"google/gemini-pro", "", "", "g"},
		{"deepseek-v4-pro", "", "", "d"},
		{"mimo-v2.6-flash", "", "", "m"},
		{"mimo-v2.6-pro", "", "", "m"},
		{"xiaomi/mimo-v2.6-flash", "", "", "m"},
		{"glm-4.7", "", "", "l"},
		{"zhipu/glm-4.7", "", "", "l"},
		{"chatglm-turbo", "", "", "l"},
		{"opaque", "", "glm", "l"},
		{"qwen-max", "Qwen Max", "", "q"},
		{"qwq-32b", "", "", "q"},
		{"qvq-72b", "", "", "q"},
		{"tongyi-qwen-plus", "", "", "q"},
		{"kimi-k2", "", "", "k"},
		{"moonshot-v1-8k", "", "", "k"},
		{"minimax-m1", "", "", "n"},
		{"abab6.5s-chat", "", "", "n"},
		{"doubao-pro-32k", "", "", "b"},
		{"seed-1.6", "", "", "b"},
		{"yi-large", "", "", "y"},
		{"baichuan2-turbo", "", "", "h"},
		{"ernie-4.0-turbo", "", "", "e"},
		{"wenxin-character", "", "", "e"},
		{"hunyuan-lite", "", "", "u"},
		{"step-1v-8k", "", "", "s"},
		{"mistral-large-latest", "", "", "r"},
		{"mixtral-8x7b", "", "", "r"},
		{"codestral-latest", "", "", "r"},
		{"pixtral-12b", "", "", "r"},
		{"llama-3.3-70b", "", "", "p"},
		{"meta-llama/Llama-3.1-8B", "", "", "p"},
		{"custom-1", "Claude Opus", "", "c"},
		{"opaque", "", "mimo", "m"},
		{"opaque", "", "m", "m"},
		{"opaque", "", "OpenAI", "o"},
		{"opaque", "", "qwen", "q"},
		{"mystery", "", "", "z"},
	}
	for _, tt := range tests {
		got := ClassifyModelVendor(tt.name, tt.desc, tt.group)
		if got != tt.want {
			t.Errorf("ClassifyModelVendor(%q,%q,%q)=%q want %q", tt.name, tt.desc, tt.group, got, tt.want)
		}
	}
}

func TestGroupModelsByVendor(t *testing.T) {
	models := []ModelOption{
		{Name: "grok-4.5"},
		{Name: "gpt-4o"},
		{Name: "claude-3-5"},
		{Name: "mimo-v2.6-flash"},
		{Name: "mystery"},
		{Name: "something", Group: "mimo"},
		{Name: "glm-4.7"},
		{Name: "qwen-max"},
		{Name: "kimi-k2"},
	}
	groups := GroupModelsByVendor(models)
	want := []struct {
		key     string
		label   string
		indices []int
	}{
		{"c", "Anthropic/Claude", []int{2}},
		{"o", "OpenAI", []int{1}},
		{"x", "xAI/Grok", []int{0}},
		{"m", "mimo", []int{3, 5}},
		{"l", "Zhipu/GLM", []int{6}},
		{"q", "Qwen", []int{7}},
		{"k", "Moonshot/Kimi", []int{8}},
		{"z", "Other", []int{4}},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups len=%d want %d: %+v", len(groups), len(want), groups)
	}
	for i, g := range groups {
		w := want[i]
		if g.Key != w.key || g.Label != w.label {
			t.Fatalf("groups[%d]=%+v want key=%s label=%s", i, g, w.key, w.label)
		}
		if len(g.Indices) != len(w.indices) {
			t.Fatalf("groups[%d].Indices=%v want %v", i, g.Indices, w.indices)
		}
		for j := range w.indices {
			if g.Indices[j] != w.indices[j] {
				t.Fatalf("groups[%d].Indices=%v want %v", i, g.Indices, w.indices)
			}
		}
	}
}

func TestModelVendorOrder(t *testing.T) {
	want := []string{"c", "o", "x", "g", "d", "m", "l", "q", "k", "n", "b", "y", "h", "e", "u", "s", "r", "p", "z"}
	if len(ModelVendors) != len(want) {
		t.Fatalf("ModelVendors len=%d want %d", len(ModelVendors), len(want))
	}
	for i, v := range ModelVendors {
		if v.Key != want[i] {
			t.Fatalf("ModelVendors[%d].Key=%q want %q", i, v.Key, want[i])
		}
	}
	if ModelVendorLabel("m") != "mimo" {
		t.Fatalf("mimo label = %q", ModelVendorLabel("m"))
	}
	if ModelVendorLabel("l") != "Zhipu/GLM" {
		t.Fatalf("glm label = %q", ModelVendorLabel("l"))
	}
	if ModelVendorLabel("q") != "Qwen" {
		t.Fatalf("qwen label = %q", ModelVendorLabel("q"))
	}
	if ModelVendorLabel("p") != "Meta/Llama" {
		t.Fatalf("meta label = %q", ModelVendorLabel("p"))
	}
	if ModelVendors[len(ModelVendors)-1].Key != "z" {
		t.Fatalf("Other must be last, got %q", ModelVendors[len(ModelVendors)-1].Key)
	}
}
