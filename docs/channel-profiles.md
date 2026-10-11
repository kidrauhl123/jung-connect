# Per-channel profiles (`[[projects.channels]]`)

Hermes-style per-channel configuration: give a channel its own prompt, bound
skills, default model / reasoning effort and (optionally) a pinned workspace.
Channels without an entry are untouched — same argv, same behaviour.

```toml
[[projects.channels]]
match = "discord:1555888557007310899"   # platform:channelID; threads inherit the parent
prompt = "这个频道绑定仓库 kidrauhl123/NianNian……"
prompt_file = "prompts/niannian.md"     # optional; relative to the config file; appended after prompt
skills = ["niannian"]                   # skill names or paths to dirs containing SKILL.md
model = "grok-4.6-xhigh"                # optional channel default
reasoning_effort = "high"               # optional
work_dir = "/home/box/workspaces/NianNian"  # optional, multi-workspace mode only; wins over /workspace bind
```

`/config reload` re-reads the entries (new sessions pick them up; running
sessions keep what they started with — use `/new` to apply immediately).

## Matching

* Exact `platform:channelID` of the session first, then the parent channel the
  platform reports (Discord threads → parent channel). A thread can still have
  its own entry.
* Telegram: `telegram:<chat_id>` (topics: the platform's channel key).

## Skill lookup

Names are searched (first hit wins) in `<workdir>/.grok/skills`,
`<workdir>/.claude/skills`, `<workdir>/.agents/skills`, `~/.grok/skills`,
`~/.claude/skills`, `~/.agents/skills`, `~/.hermes/skills`, each directly and one
category level deeper (`<root>/<category>/<name>`, Hermes layout). Missing
skills are logged and skipped.

## How it is delivered

Artifacts live under `<data_dir>/channel-profiles/<hash>.md` and
`<data_dir>/channel-plugins/<hash>/` (hash of match + prompt + skills).

| Agent | Prompt | Skills | Model / effort |
|---|---|---|---|
| ACP · Grok Build (auto-detected when cmd/args mention `grok`, or `channel_profile_mode = "grok"`) | generated agent profile (`prompt_mode: full`, `model: inherit`, `agents_md: true`) via `--agent-profile` — appended to the default system prompt, survives resume and compaction | generated plugin dir (`skills/<name>/` copies + `.claude-plugin/plugin.json`) via `--plugin-dir`; also listed in the prompt | `-m` / `--reasoning-effort` + `session/set_config_option` |
| ACP · other agents (`channel_profile_mode = "prompt"`) | `<channel-context>` block prepended to the first prompt of a **new** session | listed in that block | same as above |
| Claude Code | appended to `--append-system-prompt` | `--plugin-dir` | `--model` / effort |
| everything else | `<channel-context>` block prepended to the first prompt of a new session (best effort; not re-sent after resume/compaction) | listed in that block | not applied |

The profile is staged right before `StartSession` for that session only and
consumed by the agent, so cron/relay sessions never inherit a stale channel
profile. Every session is its own agent process, so channels sharing one
workspace directory still get distinct processes with their own profiles; the
per-workspace session store is shared as before.

## `/model` in a configured channel

Persisted per channel (`channel_model_overrides` in the project state file,
keyed by `match`) instead of changing the workspace default. Precedence for new
sessions: per-channel `/model` choice → `model` from config → workspace/project
default.

Known limitation: the `/model` card's "current" marker shows the workspace
agent's model, not the channel override.
