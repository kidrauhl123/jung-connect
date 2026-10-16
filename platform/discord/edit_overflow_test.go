package discord

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckDiscordEditFits(t *testing.T) {
	if err := checkDiscordEditFits(strings.Repeat("字", maxDiscordLen)); err != nil {
		t.Fatalf("at limit should fit: %v", err)
	}
	if err := checkDiscordEditFits(strings.Repeat("字", maxDiscordLen+1)); !errors.Is(err, errDiscordEditTooLong) {
		t.Fatalf("over limit should error, got %v", err)
	}
}

func TestUpdateMessageRejectsOverflowBeforeEdit(t *testing.T) {
	p := &Platform{} // nil session: must fail before any API call
	h := &discordPreviewHandle{channelID: "c", messageID: "m"}
	err := p.UpdateMessage(nil, h, strings.Repeat("a", maxDiscordLen+10))
	if !errors.Is(err, errDiscordEditTooLong) {
		t.Fatalf("want errDiscordEditTooLong, got %v", err)
	}
}
