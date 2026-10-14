package core

import (
	"strings"
)

// SessionContextDescriber is an optional Platform interface that describes
// where a message came from, e.g. `#general (123) · thread 「foo」(456)`.
// Used for the per-turn [context] line sent to the agent.
type SessionContextDescriber interface {
	DescribeSessionContext(msg *Message) string
}

// sessionContextLine returns a one-line "[context] ..." header prepended to
// every prompt so the agent knows which platform/channel/thread it is in and
// whether a [[projects.channels]] binding applies. Never shown in IM.
func (e *Engine) sessionContextLine(p Platform, msg *Message) string {
	if e.display.NoContextLine || msg == nil {
		return ""
	}
	where := ""
	if d, ok := p.(SessionContextDescriber); ok {
		where = strings.TrimSpace(d.DescribeSessionContext(msg))
	}
	if where == "" {
		id := msg.ChannelID
		if id == "" {
			id = extractWorkspaceChannelKey(msg.SessionKey)
		}
		if name := strings.TrimSpace(msg.ChatName); name != "" {
			where = name + " (" + id + ")"
		} else if id != "" {
			where = id
		}
	}
	binding := "none"
	if cp := e.channelProfileForMessage(msg); cp != nil && cp.Match != "" {
		binding = cp.Match
		if cp.WorkDir != "" {
			binding += " (work_dir " + cp.WorkDir + ")"
		}
	}
	platform := msg.Platform
	if platform == "" && p != nil {
		platform = p.Name()
	}
	parts := []string{platform}
	if where != "" {
		parts = append(parts, where)
	}
	parts = append(parts, "binding: "+binding)
	line := strings.ReplaceAll(strings.Join(parts, " · "), "\n", " ")
	return "[context] " + line + "\n"
}
