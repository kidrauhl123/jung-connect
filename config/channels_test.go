package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const channelsConfigTOML = `
[[projects]]
name = "grok1"

[projects.agent]
type = "acp"

[[projects.platforms]]
type = "discord"

[[projects.channels]]
match = "discord:1555888557007310899"
prompt = "频道规则"
prompt_file = "nian.md"
skills = ["niannian", "~/skills/extra"]
model = "grok-4.7"
reasoning_effort = "high"
work_dir = "/home/box/workspaces/NianNian"

[[projects.channels]]
match = "telegram:-100123"
prompt = "tg"
`

func TestLoad_ProjectChannels(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(channelsConfigTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	chans := cfg.Projects[0].Channels
	if len(chans) != 2 {
		t.Fatalf("channels = %d, want 2", len(chans))
	}
	c := chans[0]
	if c.Match != "discord:1555888557007310899" || c.Prompt != "频道规则" || c.PromptFile != "nian.md" {
		t.Fatalf("unexpected channel: %+v", c)
	}
	if len(c.Skills) != 2 || c.Skills[0] != "niannian" || c.Model != "grok-4.7" || c.ReasoningEffort != "high" || c.WorkDir != "/home/box/workspaces/NianNian" {
		t.Fatalf("unexpected channel fields: %+v", c)
	}
}

func TestLoad_ProjectChannelsValidation(t *testing.T) {
	cases := map[string]string{
		"missing match":   "[[projects.channels]]\nprompt = \"x\"\n",
		"bad match":       "[[projects.channels]]\nmatch = \"1555\"\n",
		"trailing colon":  "[[projects.channels]]\nmatch = \"discord:\"\n",
		"duplicate match": "[[projects.channels]]\nmatch = \"discord:1\"\n[[projects.channels]]\nmatch = \"discord:1\"\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			cfgPath := filepath.Join(dir, "config.toml")
			body := strings.SplitN(channelsConfigTOML, "[[projects.channels]]", 2)[0] + extra
			if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(cfgPath); err == nil || !strings.Contains(err.Error(), "channels") {
				t.Fatalf("expected channels validation error, got %v", err)
			}
		})
	}
}
