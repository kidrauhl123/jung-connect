package discord

import (
	"fmt"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/chenhg5/cc-connect/core"
)

func TestDescribeDiscordContext(t *testing.T) {
	chans := map[string]*discordgo.Channel{
		"P1": {ID: "P1", Name: "注册机", Type: discordgo.ChannelTypeGuildText},
		"T1": {ID: "T1", Name: "本父频道绑定项目了吗", Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "P1"},
	}
	ops := &fakeThreadOps{resolveChannel: func(id string) (*discordgo.Channel, error) {
		if c, ok := chans[id]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("nope")
	}}
	got := describeDiscordContext(ops, &core.Message{ChannelID: "T1"})
	want := "#注册机 (P1) · thread 「本父频道绑定项目了吗」(T1)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Thread created from a parent-channel message: reply ctx carries thread.
	got = describeDiscordContext(ops, &core.Message{ChannelID: "P1", ReplyCtx: replyContext{threadID: "T1"}})
	if got != want {
		t.Fatalf("reply-ctx thread: got %q", got)
	}
	if got := describeDiscordContext(ops, &core.Message{ChannelID: "P1"}); got != "#注册机 (P1)" {
		t.Fatalf("channel: got %q", got)
	}
}
