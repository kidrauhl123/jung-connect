package core

import (
	"strings"
	"sync"
)

// SessionModelSetter is implemented by agents that can start a session with a
// per-session model (set via `/model <x>` without --global). Like
// ChannelProfileInjector, the value is staged for the NEXT StartSession only
// and consumed there; it takes precedence over the channel profile model and
// the agent's global default. Passing "" clears any staged value.
type SessionModelSetter interface {
	SetSessionModel(model string)
}

// PendingSessionModel is an embeddable helper that implements
// SessionModelSetter. Agents call TakeSessionModel inside StartSession and,
// when it returns a non-empty value, use it instead of their resolved model.
type PendingSessionModel struct {
	pendingMu    sync.Mutex
	pendingModel string
}

// SetSessionModel stages a per-session model for the next StartSession.
func (p *PendingSessionModel) SetSessionModel(model string) {
	p.pendingMu.Lock()
	p.pendingModel = strings.TrimSpace(model)
	p.pendingMu.Unlock()
}

// TakeSessionModel returns and clears the staged per-session model.
func (p *PendingSessionModel) TakeSessionModel() string {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	m := p.pendingModel
	p.pendingModel = ""
	return m
}
