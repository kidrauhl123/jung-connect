package telegram

import (
	"context"
	"errors"
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestNeedsRichRendering(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"prose", "Hello **there**\n\nA normal reply.", false},
		{"heading and bullets", "## Heading\n\n- one\n- two", false},
		{"pipe table", "| Name | Qty |\n| --- | --- |\n| Apple | 2 |", true},
		{"task list", "- [ ] buy milk\n- [x] done", true},
		{"details", "<details>\n<summary>Notes</summary>\nHi\n</details>", true},
		{"block math", "Block:\n\n$$x^2$$\n", true},
		{"empty", "", false},
		{"table inside fence stays", "```\n| a | b |\n| - | - |\n```", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsRichRendering(c.in); got != c.want {
				t.Fatalf("needsRichRendering(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRichDetailsContainMath(t *testing.T) {
	crash := "<details><summary>Proof</summary>\n\n$$x^2$$\n\n</details>"
	if !needsRichRendering(crash) {
		t.Fatal("a details block with math still needs rich rendering")
	}
	if !richDetailsContainMath(crash) {
		t.Fatal("math inside details must be detected")
	}
	if richDetailsContainMath("<details><summary>Notes</summary>\nNo equations.\n</details>") {
		t.Fatal("details without math must not match")
	}
	if richDetailsContainMath("$$x^2$$") {
		t.Fatal("block math outside details must not match")
	}
}

func TestRichEligible(t *testing.T) {
	if !richEligible("| a | b |\n| - | - |\n| 1 | 2 |") {
		t.Fatal("a short table is eligible")
	}
	if richEligible("just prose") {
		t.Fatal("prose is not eligible")
	}
	if richEligible("<details><summary>Proof</summary>\n$$x^2$$\n</details>") {
		t.Fatal("math inside details is not eligible")
	}
	big := make([]byte, richMessageMaxChars+1)
	for i := range big {
		big[i] = 'a'
	}
	copy(big, []byte("| a | b |\n| - | - |\n"))
	if richEligible(string(big)) {
		t.Fatal("a message past the cap is not eligible")
	}
}

func newRichPlatform(bot *stubTelegramBot) *Platform {
	return &Platform{platformName: "telegram", bot: bot}
}

func TestSendUsesRichForTable(t *testing.T) {
	bot := newStubTelegramBot()
	p := newRichPlatform(bot)
	rc := replyContext{chatID: 1, threadID: 7, messageID: 42}
	table := "| Name | Qty |\n| --- | --- |\n| Apple | 2 |"

	if err := p.Send(context.Background(), rc, table); err != nil {
		t.Fatal(err)
	}
	if bot.sendRichCalls != 1 {
		t.Fatalf("rich calls = %d, want 1", bot.sendRichCalls)
	}
	if bot.sendMessageCalls != 0 {
		t.Fatalf("html send calls = %d, want 0", bot.sendMessageCalls)
	}
	if bot.lastRich != table {
		t.Fatalf("markdown sent = %q", bot.lastRich)
	}
}

func TestReplyQuotesUserOnRich(t *testing.T) {
	bot := newStubTelegramBot()
	p := newRichPlatform(bot)
	rc := replyContext{chatID: 1, messageID: 42}

	if err := p.Reply(context.Background(), rc, "- [ ] one\n- [x] two"); err != nil {
		t.Fatal(err)
	}
	if bot.sendRichCalls != 1 || bot.sendMessageCalls != 0 {
		t.Fatalf("rich=%d html=%d", bot.sendRichCalls, bot.sendMessageCalls)
	}
}

func TestSendKeepsHTMLForProse(t *testing.T) {
	bot := newStubTelegramBot()
	p := newRichPlatform(bot)

	if err := p.Send(context.Background(), replyContext{chatID: 1}, "Hello **there**"); err != nil {
		t.Fatal(err)
	}
	if bot.sendRichCalls != 0 {
		t.Fatalf("prose must not use rich, got %d", bot.sendRichCalls)
	}
	if bot.sendMessageCalls != 1 {
		t.Fatalf("html send calls = %d, want 1", bot.sendMessageCalls)
	}
}

func TestSendFallsBackWhenRichRejected(t *testing.T) {
	bot := newStubTelegramBot()
	bot.richErr = errors.New("Bad Request: can't parse rich message")
	p := newRichPlatform(bot)

	if err := p.Send(context.Background(), replyContext{chatID: 1}, "| a | b |\n| - | - |\n| 1 | 2 |"); err != nil {
		t.Fatal(err)
	}
	if bot.sendRichCalls != 1 {
		t.Fatalf("rich calls = %d, want 1", bot.sendRichCalls)
	}
	if bot.sendMessageCalls != 1 {
		t.Fatalf("fallback html calls = %d, want 1", bot.sendMessageCalls)
	}
}

func TestUpdateMessageUsesRichForTable(t *testing.T) {
	bot := newStubTelegramBot()
	p := newRichPlatform(bot)
	handle := &telegramPreviewHandle{chatID: 5, messageID: 9}
	table := "| Name | Qty |\n| --- | --- |\n| Apple | 2 |"

	if err := p.UpdateMessage(context.Background(), handle, table); err != nil {
		t.Fatal(err)
	}
	if bot.editMessageTextCalls != 1 {
		t.Fatalf("edit calls = %d, want 1", bot.editMessageTextCalls)
	}
	if bot.lastEdit == nil || bot.lastEdit.RichMessage == nil {
		t.Fatal("final edit must carry rich markdown")
	}
	if bot.lastEdit.RichMessage.Markdown != table {
		t.Fatalf("markdown edited = %q", bot.lastEdit.RichMessage.Markdown)
	}
	if bot.lastEdit.ParseMode != "" || bot.lastEdit.Text != "" {
		t.Fatal("rich edit must not also set HTML text")
	}
}

func TestUpdateMessageKeepsHTMLForProse(t *testing.T) {
	bot := newStubTelegramBot()
	p := newRichPlatform(bot)
	handle := &telegramPreviewHandle{chatID: 5, messageID: 9}

	if err := p.UpdateMessage(context.Background(), handle, "Hello **there**"); err != nil {
		t.Fatal(err)
	}
	if bot.lastEdit == nil || bot.lastEdit.RichMessage != nil {
		t.Fatal("prose edit must stay on the HTML path")
	}
	if bot.lastEdit.ParseMode != models.ParseModeHTML {
		t.Fatalf("parse mode = %q, want HTML", bot.lastEdit.ParseMode)
	}
}

func TestUpdateMessageFallsBackWhenRichEditRejected(t *testing.T) {
	bot := newStubTelegramBot()
	bot.editErr = errors.New("Bad Request: message can't be edited")
	bot.editErrOnce = true
	p := newRichPlatform(bot)
	handle := &telegramPreviewHandle{chatID: 5, messageID: 9}

	if err := p.UpdateMessage(context.Background(), handle, "| a | b |\n| - | - |\n| 1 | 2 |"); err != nil {
		t.Fatal(err)
	}
	if bot.editMessageTextCalls != 2 {
		t.Fatalf("edit calls = %d, want rich attempt plus HTML fallback", bot.editMessageTextCalls)
	}
	if bot.lastEdit == nil || bot.lastEdit.RichMessage != nil {
		t.Fatal("fallback edit must be HTML, not rich")
	}
	if bot.lastEdit.ParseMode != models.ParseModeHTML {
		t.Fatalf("fallback parse mode = %q", bot.lastEdit.ParseMode)
	}
}
