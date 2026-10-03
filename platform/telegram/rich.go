package telegram

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// richMessageMaxChars is the Bot API 10.1 cap for one sendRichMessage. Past it
// the message is split and sent the old way.
const richMessageMaxChars = 32768

// A finished reply uses sendRichMessage only when it actually contains one of
// the four constructs MarkdownV2 cannot render. Ordinary prose, bold, lists
// and headings stay on the existing HTML path. Same rule as Hermes
// (_needs_rich_rendering) and grok2tg (needsRichRendering).
var (
	richTableSeparator = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)+\|?\s*$`)
	richTaskList       = regexp.MustCompile(`(?m)^\s*[-*]\s+\[[ xX]\]\s+`)
	richDetailsLine    = regexp.MustCompile(`(?m)^<details\b|^</details>|^<summary\b|^</summary>`)
	richDetailsBlock   = regexp.MustCompile(`(?is)<details\b[^>]*>[\s\S]*?</details>`)
	richMathInDetails  = regexp.MustCompile(`(?is)\$\$[\s\S]*?\$\$|\\\[[\s\S]*?\\\]|\\\([\s\S]*?\\\)|\\(?:sum|frac|alpha|beta|gamma|delta|theta|lambda|mu|pi|sigma|int|prod|sqrt|lim|infty|begin\{(?:equation|align|matrix|cases)\})`)
)

// needsRichRendering reports whether the finished text contains a pipe table, a
// GFM task list, a collapsible details block, or block math.
func needsRichRendering(content string) bool {
	if content == "" {
		return false
	}
	for _, line := range splitLines(content) {
		if richTableSeparator.MatchString(line) {
			return true
		}
	}
	if richTaskList.MatchString(content) {
		return true
	}
	if richDetailsLine.MatchString(content) {
		return true
	}
	return containsDoubleDollar(content)
}

// richDetailsContainMath reports math nested inside <details>. Telegram Desktop
// 6.9.1 crashes on that shape even though the Bot API accepts the payload, so
// rich delivery is skipped and the message falls back to the old path.
func richDetailsContainMath(content string) bool {
	if content == "" {
		return false
	}
	for _, block := range richDetailsBlock.FindAllString(content, -1) {
		if richMathInDetails.MatchString(block) {
			return true
		}
	}
	return false
}

// richEligible is the single gate for a finished message: it has to contain a
// construct the old path degrades, fit one message, and not nest math in a
// collapsible block.
func richEligible(content string) bool {
	if len(content) > richMessageMaxChars {
		return false
	}
	return needsRichRendering(content) && !richDetailsContainMath(content)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 8)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func containsDoubleDollar(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '$' && s[i+1] == '$' {
			return true
		}
	}
	return false
}

// sendRich sends a finished message with sendRichMessage when it contains a
// construct the HTML path degrades: a pipe table, a task list, a collapsible
// block, or block math. The markdown goes out raw, the way the model wrote it.
// Returns false for ordinary text, an oversized body, math nested in details,
// or a rejection, so the caller falls back to the HTML send. Never both.
func (p *Platform) sendRich(ctx context.Context, bot telegramBot, rc replyContext, content string, reply bool) bool {
	text := strings.TrimSpace(content)
	if !richEligible(text) {
		return false
	}
	params := &tgbot.SendRichMessageParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
		RichMessage:     models.InputRichMessage{Markdown: text},
	}
	if reply && rc.messageID != 0 {
		params.ReplyParameters = &models.ReplyParameters{MessageID: rc.messageID}
	}
	if _, err := bot.SendRichMessage(ctx, params); err != nil {
		slog.Warn("telegram: sendRichMessage rejected, falling back to HTML",
			"error", err.Error(), "len", len(text))
		return false
	}
	return true
}

// editRich rewrites the streamed preview in place when the finished text needs
// rich rendering. Bot API has no way to convert an existing message through
// sendRichMessage, but editMessageText accepts the same rich payload, so the
// preview becomes the final message instead of a second one. Returns false
// for ordinary text or a rejection, and the caller keeps the HTML edit.
func (p *Platform) editRich(ctx context.Context, bot telegramBot, h *telegramPreviewHandle, content string) bool {
	text := strings.TrimSpace(content)
	if !richEligible(text) {
		return false
	}
	params := &tgbot.EditMessageTextParams{
		ChatID:      h.chatID,
		MessageID:   h.messageID,
		RichMessage: &models.InputRichMessage{Markdown: text},
	}
	if _, err := bot.EditMessageText(ctx, params); err != nil {
		if strings.Contains(err.Error(), "not modified") {
			return true
		}
		slog.Warn("telegram: rich edit rejected, falling back to HTML",
			"error", err.Error(), "len", len(text))
		return false
	}
	return true
}
