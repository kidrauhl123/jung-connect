package acp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAvailableModels_FromAPI(t *testing.T) {
	apiKey := os.Getenv("XAI_API_KEY")
	base := os.Getenv("GROK_MODELS_BASE_URL")
	if apiKey == "" || base == "" {
		t.Skip("env not set")
	}
	a, err := New(map[string]any{
		"command": "true",
		"env": map[string]any{
			"XAI_API_KEY":          apiKey,
			"GROK_MODELS_BASE_URL": base,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms := a.(core.ModelSwitcher)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models := ms.AvailableModels(ctx)
	if len(models) < 3 {
		t.Fatalf("expected API models, got %#v", models)
	}
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
		if !isGrokChatModel(m.Name) {
			t.Fatalf("unfiltered model leaked: %s", m.Name)
		}
	}
	t.Logf("count=%d names=%v", len(names), names)
}
