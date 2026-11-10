package acp

import (
	"context"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestAgent_ModelSwitcherContracts(t *testing.T) {
	var _ core.ModelSwitcher = (*Agent)(nil)
}

func TestAgent_AvailableModels_DynamicTakesPriority(t *testing.T) {
	a := &Agent{}

	// jung-connect: before any handshake AvailableModels falls back to the
	// xAI API / grok CLI / built-in list, so no pre-handshake assertion here.
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir()) // no grok CLI: server catalog must win over fallback

	// Session reports dynamic configOptions from server
	a.reportModels("kimi-code/k3", []core.ModelOption{
		{Name: "kimi-code/k3", Alias: "K3", Desc: "Dynamic K3"},
		{Name: "example/model-b", Alias: "Model B", Desc: "Dynamic Model B"},
	})

	dynamic := a.AvailableModels(context.Background())
	if len(dynamic) != 2 || dynamic[0].Name != "kimi-code/k3" {
		t.Fatalf("AvailableModels = %+v, want dynamic models", dynamic)
	}

	if got := a.GetModel(); got != "kimi-code/k3" {
		t.Fatalf("GetModel = %q, want kimi-code/k3", got)
	}

	a.SetModel("example/model-b")
	if got := a.GetModel(); got != "example/model-b" {
		t.Fatalf("GetModel after SetModel = %q, want example/model-b", got)
	}
}

// TestAgent_ModelOverrideSurvivesServerReport covers the /model switch flow:
// the engine closes the session on switch, and the next handshake's
// session/load reports the model persisted at creation time. That report
// must not revert the user's override.
func TestAgent_ModelOverrideSurvivesServerReport(t *testing.T) {
	a := &Agent{}

	a.SetModel("kimi-code/k3")

	// Next session handshake: server reports the stale persisted model.
	a.reportModels("example/model-b", []core.ModelOption{
		{Name: "example/model-b", Alias: "Model B"},
		{Name: "kimi-code/k3", Alias: "K3"},
	})

	if got := a.GetModel(); got != "kimi-code/k3" {
		t.Fatalf("GetModel after stale server report = %q, want override kimi-code/k3", got)
	}
	// jung-connect: the /model --global default lives in a.model.
	if a.model != "kimi-code/k3" {
		t.Fatalf("a.model = %q, want kimi-code/k3", a.model)
	}
	// The model list itself must still refresh from the server.
	if got := a.AvailableModels(context.Background()); len(got) != 2 {
		t.Fatalf("AvailableModels = %+v, want 2 server models", got)
	}
}
