package discord

import (
	"fmt"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/chenhg5/cc-connect/core"
)

var _ core.SessionContextDescriber = (*Platform)(nil)

type channelInfo struct {
	name     string
	parentID string
	thread   bool
	dm       bool
}

var channelInfoCache sync.Map // channelID -> channelInfo

func lookupChannelInfo(ops discordThreadOps, id string) (channelInfo, bool) {
	if v, ok := channelInfoCache.Load(id); ok {
		return v.(channelInfo), true
	}
	if ops == nil {
		return channelInfo{}, false
	}
	ch, err := ops.ResolveChannel(id)
	if err != nil || ch == nil {
		return channelInfo{}, false
	}
	info := channelInfo{
		name:     ch.Name,
		parentID: ch.ParentID,
		thread:   isThreadChannelType(ch.Type),
		dm:       ch.Type == discordgo.ChannelTypeDM || ch.Type == discordgo.ChannelTypeGroupDM,
	}
	if !info.thread { // thread names change (auto-rename); don't pin them
		channelInfoCache.Store(id, info)
	}
	return info, true
}

// DescribeSessionContext implements core.SessionContextDescriber:
// "#parent (id) · thread 「name」(id)" or "#channel (id)" or "DM (id)".
func (p *Platform) DescribeSessionContext(msg *core.Message) string {
	var ops discordThreadOps
	if p.session != nil {
		ops = sessionThreadOps{session: p.session}
	}
	return describeDiscordContext(ops, msg)
}

func describeDiscordContext(ops discordThreadOps, msg *core.Message) string {
	if msg == nil {
		return ""
	}
	id := msg.ChannelID
	if rc, ok := msg.ReplyCtx.(replyContext); ok && rc.threadID != "" {
		id = rc.threadID
	}
	if id == "" {
		return ""
	}
	info, ok := lookupChannelInfo(ops, id)
	if !ok {
		return fmt.Sprintf("channel %s", id)
	}
	if info.dm {
		return fmt.Sprintf("DM (%s)", id)
	}
	if info.thread && info.parentID != "" {
		parent := info.parentID
		if pi, ok := lookupChannelInfo(ops, info.parentID); ok && pi.name != "" {
			parent = "#" + pi.name
		}
		return fmt.Sprintf("%s (%s) · thread 「%s」(%s)", parent, info.parentID, info.name, id)
	}
	return fmt.Sprintf("#%s (%s)", info.name, id)
}
