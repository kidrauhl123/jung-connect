---
name: jung-connect
description: Use when configuring or operating jung-connect (cc-connect fork) — per-channel prompt/skills/model, allow_from, /workspace, /model, reload vs restart.
version: 1.1.0
author: kidrauhl123
license: MIT
tags: [jung-connect, cc-connect, discord, telegram, config]
---

# jung-connect

## When to Use

用户问 jung-connect / cc-connect 怎么配、怎么生效：给某个频道单独绑提示词、技能、模型或目录；`allow_from` 是什么；`/workspace`、`/model`、`/new`、`/config reload`；该重载还是重启。

jung-connect（`kidrauhl123/jung-connect`）是 cc-connect（`chenhg5/cc-connect`）的 fork：把 Telegram、Discord、飞书等聊天平台接到本机 agent CLI（ACP/Grok Build、Claude Code、Codex、opencode、Pi/omp、Antigravity…）。二进制和命令仍叫 `cc-connect`。比上游多：`[[projects.channels]]` 频道配置；Grok/ACP 的 `/model`（从 CLI 或 OpenAI 兼容 `/v1/models` 拉列表、按厂商分组）和 `/reasoning`；多工作区 `fallback_work_dir`；Discord 的 👀/✅/❌ 进度灯、`reply_to_mode`、表格转列表。本机若另有 bridge 运维技能，机器、路径、机器人以那份为准；这里只讲通用语义。答案从代码和配置来，不要猜。

## 先找到正在跑的那一份

- 一台机可同时跑多份，每份一个配置。`ps -eo pid,ppid,args | grep '[c]c-connect'` 看 `--config`；没写时依次是进程工作目录（`/proc/<pid>/cwd`）下的 `config.toml`、`~/.cc-connect/config.toml`。
- 谁在管它：`systemctl status <pid>` 能显示所属 unit（`cc-connect daemon install` 装的叫 `cc-connect`，也可能是自写 unit 或 `--user` unit）；父进程是 shell 循环脚本的，被杀后会自动拉起；都不是就是手动起的。
- 日志：systemd 用 `journalctl -u <unit>`；循环脚本看它重定向的文件。会话、项目状态、工作区绑定在 `data_dir`（默认 `~/.cc-connect`）。

## 配置要点（TOML）

```toml
[[projects]]
name = "my-bot"
[projects.agent]
type = "acp"                        # acp / claudecode / codex / opencode / pi / antigravity …
[projects.agent.options]
work_dir = "/path/to/dir"
cmd = "grok"
[[projects.platforms]]
type = "discord"
[projects.platforms.options]
token = "${DISCORD_BOT_TOKEN}"      # 支持 ${环境变量}，别写明文
allow_from = "<你的用户ID>"          # 逗号分隔的用户 ID；不能留空，不能 "*"
```

- 一个 `[[projects]]` = 一个 agent + 若干平台。`allow_from`、`token` 和平台选项都在 `[projects.platforms.options]` 里，不在 `[[projects.platforms]]` 本身。
- `allow_from`：**用户 ID** 白名单（谁能驱动 agent），**不是频道 ID**。ID 让本人在聊天里发 `/whoami` 拿。空或 `"*"` 都等于任何人能在这台机上跑 agent，**绝不要这样设，示例里也别写"可空或 *"**，也不要为了"让某个频道能用"去放宽它。频道 ID 只出现在 `[[projects.channels]]` 的 `match` 里。
- `admin_from`：能用 `/shell` `/show` `/dir` `/restart` `/upgrade` `/web` 的用户 ID；空 = 谁都不行。
- 多工作区：`mode = "multi-workspace"` + `base_dir`，频道名对上 `base_dir/<频道名>` 自动绑定；`fallback_work_dir` 给没绑的频道。

## 频道配置 `[[projects.channels]]`

写在所属 `[[projects]]` 之下；没配的频道行为完全不变。

```toml
[[projects.channels]]
match = "discord:<频道ID>"     # 平台:频道ID；Telegram 是 telegram:<chat_id>
prompt = "这个频道只做 X 仓库的代码评审……"
prompt_file = "prompts/x.md"   # 可选，相对配置文件，接在 prompt 后
skills = ["code-review"]       # 技能名，或含 SKILL.md 的目录（可用 ~ 或绝对路径）
model = "<模型ID>"             # 可选，频道默认模型
reasoning_effort = "high"      # 可选
work_dir = "/path/to/repo"     # 可选，仅 multi-workspace；优先于 /workspace 绑定
```

- 匹配：先会话所在的精确 `平台:频道ID`（Discord 帖子是帖子自己的 ID），再父频道。Discord 帖子要继承父频道，需要平台开 `thread_isolation = true`。
- 技能名依次找 `<workdir>/.grok|.claude|.agents/skills`、`~/.grok/skills`、`~/.claude/skills`、`~/.agents/skills`、`~/.hermes/skills`（各含一层分类目录），先找到先用；找不到只记日志跳过。
- 怎么送到 agent：Grok Build（ACP）用生成的 agent profile + plugin 目录，续聊和压缩后仍在；Claude Code 用 `--append-system-prompt` + `--plugin-dir`；其它 agent 只在**新会话**第一条消息前加 `<channel-context>`。

## 写频道提示词

替用户写 `prompt` / `prompt_file` 时，除了频道职责，固定带上一段"主动沉淀"，因为 agent 没有自动记忆，不写就不会做：

```markdown
## 维护本频道
- 做完重要的事、定下决定、发现新情况时，主动更新本频道的笔记文件（写明路径），只记结论和现状，不记过程。
- 反复用到的做法整理成技能（`<技能目录>/<名>/SKILL.md`），并加进本频道 `skills`。
- 职责或规则变了就改本频道提示词文件，然后 `/config reload`。
- 改动前备份或用 git 提交；不写入 token、密码等秘密。
```

把笔记、提示词、技能的实际路径填进去；路径放在 git 管理的目录里，方便回滚和多机同步。

## 改完怎么生效

- 聊天里 `/config reload` 重读：`[[projects.channels]]`、display、providers、自定义命令和别名、`admin_from`、`users`、`disabled_commands`、`banned_words` 等。频道配置只作用于**新会话**，当前会话发 `/new`。
- 必须重启进程：平台块（token、`allow_from`、平台选项）、`[projects.agent]`、`mode`/`base_dir`、增删项目。systemd 用 `systemctl restart <unit>`（用户级加 `--user`，unit 名以上面查到的为准）；`cc-connect daemon restart` 只管 `cc-connect daemon install` 装的 `cc-connect` unit，自写的 unit 别用它；循环脚本就给那个 PID 发 `SIGTERM` 让脚本拉起，**不要再手动起第二份**（同一 token 两路连接会互踢或 409）。聊天里 `/restart` 需要 `admin_from`。重启会打断正在跑的回合，先告诉用户。

## 常用聊天命令（支持前缀匹配）

`/new [名称]` 新会话 · `/list` `/switch <n>` `/current` · `/stop` 停当前回合、保留会话 · `/model [名称]`、`/reasoning [级别]`、`/mode` · `/workspace [bind <名>|route <绝对路径>|init <git URL>|unbind|list|shared …]`（仅 multi-workspace）· `/config [get|set|reload]` · `/skills` · `/whoami` · `/status` `/doctor` · `/cron` `/timer` · `/restart`（admin）

- `/workspace bind <名>` 绑 `base_dir/<名>`，`route` 绑任意已存在的绝对路径。Discord 开 `thread_isolation` 时绑定记在父频道，所有帖子共用。
- `/model` 在有 `[[projects.channels]]` 的频道里只改这个频道（存在项目状态的 `channel_model_overrides`）。新会话的优先级：频道 `/model` > 配置里的 `model` > 工作区或项目默认。切换会关掉当前 agent 进程，下一条消息用新模型接着聊。卡片上"当前"标的是工作区模型，不是频道覆盖（已知问题）。

## 例子：给 Discord 频道单独绑提示词和技能

1. 拿频道 ID（Discord 开发者模式 → 右键频道 → 复制 ID）。确认技能目录里有 `SKILL.md`。
2. 备份这份配置：`cp config.toml config.toml.bak-$(date +%Y%m%d-%H%M%S)`。
3. 在对应 `[[projects]]` 下加上面的 `[[projects.channels]]` 块。
4. 聊天里 `/config reload`，再在该频道 `/new`（或新开帖）。不用重启，不用动 `allow_from`。
5. 验证：在频道里问它这个频道的规则；日志里没有技能找不到的警告。

## 安全

- 配置里有 bot token、API key、management/bridge token。**不要贴进聊天或回复**；展示配置先打码，例如 `sed -E 's/^([[:space:]]*[A-Za-z_]*(token|key|secret|password)[A-Za-z_]*[[:space:]]*=).*/\1 "***"/I' config.toml`。
- 改前备份；只动用户说的那一份配置和那一个进程。
- 不放宽 `allow_from` / `admin_from`，不设 `"*"`。
- 能 `/config reload` 就不重启；要重启先说明影响。
