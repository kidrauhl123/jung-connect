package core

import "log/slog"

// liveSwitchResult reports how a /model or /reasoning change was applied.
type liveSwitchResult int

const (
	// liveSwitchNone: no live session (or unsupported); the change applies
	// when the next turn starts/resumes the session.
	liveSwitchNone liveSwitchResult = iota
	// liveSwitchApplied: applied in place to the running, idle session.
	liveSwitchApplied
	// liveSwitchDeferred: a turn is running; applied before the next turn.
	liveSwitchDeferred
	// liveSwitchFailed: the live session rejected it; caller should restart
	// the session (it will resume with the new setting).
	liveSwitchFailed
)

// applyLiveSessionChange tries to apply fn to the live agent session of
// sessionKey without restarting it. fn returns false when the session does
// not support or rejected the change.
func (e *Engine) applyLiveSessionChange(sessionKey string, sessions *SessionManager, what string, fn func(AgentSession) bool) liveSwitchResult {
	iKey := e.interactiveKeyForSessionKey(sessionKey)
	e.interactiveMu.Lock()
	state := e.interactiveStates[iKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return liveSwitchNone
	}
	state.mu.Lock()
	as := state.agentSession
	state.mu.Unlock()
	if as == nil || !as.Alive() {
		return liveSwitchNone
	}
	busy := false
	if sessions != nil {
		if s := sessions.GetOrCreateActive(sessionKey); s != nil {
			busy = s.Busy()
		}
	}
	if busy {
		state.mu.Lock()
		state.pendingLiveChanges = append(state.pendingLiveChanges, pendingLiveChange{what: what, fn: fn})
		state.mu.Unlock()
		slog.Info("live switch deferred to next turn", "session", sessionKey, "change", what)
		return liveSwitchDeferred
	}
	if fn(as) {
		slog.Info("live switch applied in place", "session", sessionKey, "change", what)
		return liveSwitchApplied
	}
	slog.Warn("live switch rejected; session will restart and resume", "session", sessionKey, "change", what)
	return liveSwitchFailed
}

type pendingLiveChange struct {
	what string
	fn   func(AgentSession) bool
}

// applyPendingLiveChanges runs changes deferred while a turn was running.
// Called at the start of the next turn, before the prompt is sent.
func applyPendingLiveChanges(state *interactiveState) {
	state.mu.Lock()
	changes := state.pendingLiveChanges
	state.pendingLiveChanges = nil
	as := state.agentSession
	state.mu.Unlock()
	if as == nil {
		return
	}
	for _, c := range changes {
		if !c.fn(as) {
			slog.Warn("deferred live switch failed", "change", c.what)
		} else {
			slog.Info("deferred live switch applied", "change", c.what)
		}
	}
}

func liveModelFn(model string) func(AgentSession) bool {
	return func(as AgentSession) bool {
		sw, ok := as.(LiveModelSwitcher)
		return ok && sw.SetLiveModel(model)
	}
}

func liveReasoningFn(effort string) func(AgentSession) bool {
	return func(as AgentSession) bool {
		sw, ok := as.(LiveReasoningSwitcher)
		return ok && sw.SetLiveReasoningEffort(effort)
	}
}

// needsRestart reports whether the caller must drop the live session so the
// next turn restarts (and resumes) it with the new setting.
func (r liveSwitchResult) needsRestart() bool {
	return r == liveSwitchNone || r == liveSwitchFailed
}
