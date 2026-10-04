# Discord Setup Guide

This guide walks you through connecting **cc-connect** to Discord, so you can chat with your local Claude Code via a Discord bot.

## Prerequisites

- A Discord account
- A machine that can run cc-connect (no public IP needed)
- Claude Code installed and configured

> 💡 **Advantage**: Uses Gateway (WebSocket) — no public IP, no domain, no reverse proxy needed.

---

## Step 1: Create a Discord Application

### 1.1 Open the Developer Portal

Go to [Discord Developer Portal](https://discord.com/developers/applications) and sign in.

### 1.2 Create a New Application

1. Click "New Application" in the top right
2. Enter an application name (e.g. `cc-connect`)
3. Agree to the Terms of Service
4. Click "Create"

---

## Step 2: Create a Bot User

### 2.1 Go to Bot Settings

In the left sidebar, click "Bot".

### 2.2 Add a Bot

1. Click "Add Bot"
2. Confirm the action

### 2.3 Configure Bot Info

| Field | Suggested Value |
|-------|----------------|
| Username | `cc-connect` |
| Avatar | Upload an icon you like |

---

## Step 3: Get the Bot Token

### 3.1 Generate Token

On the Bot page:

1. Click "Reset Token"
2. You may need to enter a 2FA code
3. Click "Copy" to copy the token

> ⚠️ The token is only shown once — save it immediately! Format: `MTk4NjIyNDgzNDcOTY3NDUxMg.G8vKqh.xxx...`

### 3.2 Lost Your Token?

Click "Reset Token" at any time to regenerate. The old token will be invalidated immediately.

---

## Step 4: Configure Privileged Intents (Important!)

### 4.1 What Are Intents?

Intents control which events your bot can receive from Discord's Gateway.

### 4.2 Enable Required Intents

On the Bot page, under "Privileged Gateway Intents", enable:

| Intent | Purpose | Required? |
|--------|---------|-----------|
| **Message Content Intent** | Read message content | ✅ **Required** |
| Presence Intent | Read user status | Optional |
| Server Members Intent | Read server members | Optional |

> ⚠️ **You must enable Message Content Intent**, or the bot won't be able to read messages!

### 4.3 Save Changes

Click "Save Changes".

---

## Step 5: Configure cc-connect

Add the token to your `config.toml`:

```toml
[[projects]]
name = "my-project"

[projects.agent]
type = "claudecode"

[projects.agent.options]
work_dir = "/path/to/your/project"
mode = "default"

[[projects.platforms]]
type = "discord"

[projects.platforms.options]
token = "MTk4NjIyNDgzNDcOTY3NDUxMg.G8vKqh.xxx..."
# thread_isolation = true  # Optional: isolate each agent session in its own Discord thread
# thread_require_mention = false  # Optional: keep @mention required inside threads (multi-bot)
# reactions = true          # Optional: 👀/✅/❌ progress light on the user's message (default true)
# reply_to_mode = "first"   # Optional: off | first | all (Hermes; default first)
# progress_style = "legacy" # Optional: legacy | compact | card
```

> cc-connect automatically configures the required Intents (MESSAGE_CONTENT, GUILD_MESSAGES, DIRECT_MESSAGES).
> With `thread_isolation = true`, cc-connect creates or reuses a Discord thread for each session and routes follow-up messages by thread channel ID. Timer/cron jobs set **inside a thread** deliver back to **that same thread**. They do not open a sibling thread under the parent channel. Jobs set in a parent text channel still get a fresh standalone thread so the parent timeline is not spammed.
> After **this bot** has participated in a thread (it opened the thread, or it replied there once), follow-ups in **that thread** do not need another `@mention`. This is independent of `multi-workspace` and is **not** `group_reply_all` — parent text channels stay mention-gated. Set `thread_require_mention = true` if several bots share one thread and each should fire only on `@mention`.
> Threads this bot **just created** start with a placeholder name (the user's first message, mentions stripped). After the first agent turn, if Claude/Antigravity produces an LLM session title, jung-connect renames that thread — only while Discord still shows the placeholder. A human rename, a forum post the bot did not create, or a later title update is left alone.
> `progress_style = "compact"` merges thinking/tool updates into one editable message; `progress_style = "card"` renders a Discord-native embed progress card and still sends the final answer as a normal message.
> Progress light (default on, `reactions = true`): 👀 while working, ✅ on success, ❌ on failure. Cancel only removes 👀. The bot never reads reactions you add.
> `reply_to_mode` (default `first`): the first chunk of each **final Send** is a Discord reply to your message, including inside `thread_isolation` threads. Progress/preview (`SendPreviewStart`) never replies — Hermes does not thread commentary. The reply's `channel_id` is the **thread** (the send target), not the parent that owns the starter — Discord rejects `REPLIES_CANNOT_REFERENCE_OTHER_CHANNEL` otherwise, and the answer never lands. Later chunks in that send are not replies. `off` never replies; `all` replies on every chunk. A rejected reference (deleted target, system message, or cross-channel) is retried without the reply.
> Discord does not render GFM pipe tables. Outbound `Send` rewrites them into bold headings + bullets (Hermes `convert_table_to_bullets`). The model is not told to avoid markdown tables.

---

## Step 6: Generate an Invite Link

### 6.1 Go to OAuth2 Settings

In the left sidebar, click "OAuth2" → "URL Generator".

### 6.2 Select Scopes

Under "Scopes", check:
- ✅ `bot`

### 6.3 Select Permissions

Under "Bot Permissions", check:

| Permission | Purpose |
|------------|---------|
| Read Messages/View Channels | Read messages |
| Send Messages | Send messages |
| Create Public Threads | Create a new thread for a fresh agent session |
| Send Messages in Threads | Send messages in threads |
| Read Message History | Read message history |
| Add Reactions | Progress light 👀/✅/❌ on the user's message |

### 6.4 Copy the Link

1. The invite link will be generated at the bottom of the page
2. Click "Copy"

---

## Step 7: Invite the Bot to Your Server

### 7.1 Open the Invite Link

Open the copied URL in your browser and sign in to Discord.

### 7.2 Select a Server

Choose the server you want to add the bot to from the dropdown.

### 7.3 Authorize

Review the permissions and click "Authorize". Complete the CAPTCHA if prompted.

---

## Step 8: Start cc-connect

### 8.1 Launch

```bash
cc-connect
# Or specify a config file
cc-connect -config /path/to/config.toml
```

### 8.2 Verify Connection

You should see logs like:

```
level=INFO msg="discord: connected" bot=cc-connect#0000
level=INFO msg="platform started" project=my-project platform=discord
level=INFO msg="cc-connect is running" projects=1
```

---

## Step 9: Start Chatting

### 9.1 Channel Usage

Send a message in any channel where the bot has permissions.

### 9.2 Direct Message

1. Click the bot's avatar
2. Send a DM

---

## Usage Example

```
User: Help me analyze the current project structure

cc-connect: 🤔 Thinking...
cc-connect: 🔧 Tool: Bash(ls -la)
cc-connect: Here's the project structure...
```

If you enable `progress_style = "card"`, Discord shows one editable progress embed during the turn, then the final answer arrives as a separate normal message. This reduces channel noise compared with the legacy multi-message flow.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Discord Cloud                           │
│                                                              │
│   User Message ──→ Discord Gateway ◄── WebSocket             │
│                         │                                    │
└─────────────────────────┼────────────────────────────────────┘
                          │
                          │ WebSocket (no public IP needed)
                          ▼
┌─────────────────────────────────────────────────────────────┐
│                    Your Local Machine                         │
│                                                              │
│   cc-connect ◄──► Claude Code CLI ◄──► Your Project Code    │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

---

## Discord Gateway Features

| Feature | Details |
|---------|---------|
| **Connection** | WebSocket |
| **Public IP** | ❌ Not needed |
| **Heartbeat** | Automatic keepalive |
| **Reconnection** | Automatic on disconnect |
| **Intents** | Must declare required event types |
| **Message limit** | 2000 characters per message (auto-split by cc-connect) |
| **Markdown** | Full native support |

---

## FAQ

### Q: Bot can't read message content?

**Most common issue**: Message Content Intent is not enabled!

Fix:
1. Go to Discord Developer Portal
2. Select your app → Bot
3. Enable "Message Content Intent"
4. Save changes
5. Restart cc-connect

### Q: Bot connects then immediately disconnects?

Check:
1. Is the bot token correct?
2. Are intents configured properly?
3. Are you hitting Discord rate limits? (from frequent reconnects)

### Q: Bot doesn't appear in the server?

1. Make sure you used the invite link to add the bot
2. Check if the bot was kicked from the server

### Q: How to regenerate the token?

1. Go to Discord Developer Portal
2. Select your app → Bot
3. Click "Reset Token"
4. Update your config.toml

### Q: Why does the bot ignore messages unless I @ it?

Guild text channels require `@mention` unless `group_reply_all = true` (do **not** turn that on for public text channels — every message would be ingested, and with `thread_isolation` each one may become a thread).

After **this bot** has participated in a thread (it opened the thread, replied there, answered a slash command there, or its own message was seen there), follow-ups **in that thread** do not need another `@mention`. This is independent of `multi-workspace`. Set `thread_require_mention = true` if several bots share one thread.

Existing threads are not backfilled: after a deploy, @ once (or send a slash command) in that thread so the bot records participation.

### Q: What are the 👀 / ✅ / ❌ reactions?

The bot puts them on **your** message as a progress light (Hermes-style). 👀 while it works, ✅ when the turn succeeds, ❌ if it fails. Cancel only removes 👀. The bot does **not** read reactions you add. Needs the Add Reactions permission; set `reactions = false` to turn this off.

`reply_to_mode` (default `first`) controls Discord "reply" on the **final answer**: the first chunk of each Send references your message — including inside a `thread_isolation` thread, with the reference channel set to the thread (not the parent). Progress/preview messages do not reply. Later chunks in that send do not. `off` never replies; `all` replies on every chunk. If Discord rejects the reference, the chunk is sent again without it.

### Q: Where do /timer and /cron replies go?

Back to the conversation that created the job. A job set in a thread/sub-post stays in that thread. A job set in a parent text channel (with `thread_isolation`) still opens one standalone thread for that run, instead of posting on the parent timeline.

### Q: Does renaming a channel or thread wake the model?

No. Discord's "name changed to …" system message is not a user turn (same as Hermes). Archive/slowmode/tag updates are also ignored. Only normal messages and replies become model turns.

### Q: Does the bot rename Discord threads?

Yes, Hermes-style: only threads **this bot just opened**. The first name is a placeholder from the user's message. After the first turn, an LLM session title replaces it — unless someone already renamed the thread. Forum posts and existing threads are not rewritten.

Claude Code 2.1+ print/stream-json no longer emits `ai-title`. cc-connect generates that title once from the first user prompt (provider Anthropic-compatible API; model prefers `CC_CONNECT_SESSION_TITLE_MODEL`, then `ANTHROPIC_DEFAULT_HAIKU_MODEL`, then `ANTHROPIC_MODEL`) and still applies a late `EventSessionTitle` after the first turn. It does **not** PATCH the user's first message as the title. Map haiku to a model the token can call — an unmapped `haiku` / `claude-3-5-haiku-latest` name 403s. Do not use a thinking model (`grok-4.6-high`) for titles: `/v1/messages` can exceed the title timeout and only warn (`session title generation failed`). Prefer a fast text model via `CC_CONNECT_SESSION_TITLE_MODEL` (this fleet: `gemini-3.8-flash-low`). Title HTTP failures log at warn (`session title generation failed` / `title client is nil`), not debug. Placeholder names persist under `data_dir/discord_auto_thread_placeholders_<project>.json` so a restart does not forget which threads this bot just opened. `/stop` stops Discord typing immediately (does not wait for the event loop).

### Q: Bot has insufficient permissions?

1. Generate a new invite link with the correct permissions
2. Re-invite the bot to the server

---

## References

- [Discord Developer Portal](https://discord.com/developers/applications)
- [Discord API Documentation](https://discord.com/developers/docs/intro)
- [Bot Getting Started Guide](https://discord.com/developers/docs/getting-started)
- [Gateway Intents](https://discord.com/developers/docs/topics/gateway#privileged-intents)
- [OAuth2 Scopes](https://discord.com/developers/docs/topics/oauth2#shared-resources-oauth2-scopes)

---

## See Also

- [Feishu Setup](./feishu.md)
- [DingTalk Setup](./dingtalk.md)
- [Weibo Setup](./weibo.md)
- [Telegram Setup](./telegram.md)
- [Slack Setup](./slack.md)
- [Back to README](../README.md)
