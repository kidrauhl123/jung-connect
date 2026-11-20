package acp

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// backgroundTurnIdle is the fallback quiet period used to close an
// out-of-turn text stream when the agent does not emit turn_completed
// (for example kimi acp's single "Compaction completed" chunk).
//
// Sessions that have advertised a Grok-style explicit boundary
// (turn_completed or response_completed) wait for turn_completed
// instead of this timer, so a streaming reply is not split into one
// platform message per chunk.
var backgroundTurnIdle = 1500 * time.Millisecond

// cancelSettleIdle is how long after the last leftover update from a
// cancelled turn we keep dropping out-of-turn events, then resume
// genuine background work.
var cancelSettleIdle = 1500 * time.Millisecond

// backgroundTurn aggregates out-of-turn ACP stream chunks into a single
// engine turn (one EventResult) and suppresses leftovers after CancelTurn.
type backgroundTurn struct {
	mu          sync.Mutex
	open        bool
	hasText     bool
	explicitEnd bool // saw response_completed or turn_completed on this session
	dropping    bool // drop leftover updates after CancelTurn
	idleTimer   *time.Timer
	settleTimer *time.Timer
}

func sessionUpdateKind(params json.RawMessage) string {
	var wrap struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &wrap) != nil {
		return ""
	}
	return wrap.Update.SessionUpdate
}

func isExplicitTurnEnd(kind string) bool {
	return kind == "turn_completed"
}

func isExplicitTurnSignal(kind string) bool {
	return kind == "turn_completed" || kind == "response_completed"
}

func (s *acpSession) noteBackgroundVisibleText() {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	if s.bg.dropping {
		return
	}
	s.bg.open = true
	s.bg.hasText = true
	s.armIdleLocked()
}

func (s *acpSession) markExplicitTurnSignal() {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	s.bg.explicitEnd = true
	// Once the agent has advertised a real turn boundary, do not close
	// mid-stream on a quiet gap. Wait for turn_completed.
	s.stopIdleLocked()
}

func (s *acpSession) closeBackgroundTurn(reason string) {
	s.bg.mu.Lock()
	open, hasText, dropping := s.bg.open, s.bg.hasText, s.bg.dropping
	s.stopIdleLocked()
	s.bg.open = false
	s.bg.hasText = false
	s.bg.mu.Unlock()
	if dropping || !open || !hasText {
		return
	}
	slog.Debug("acp: closing unsolicited background turn", "reason", reason, "session_id", s.currentACPSessionID())
	s.emit(core.Event{Type: core.EventResult, SessionID: s.currentACPSessionID(), Done: true})
}

func (s *acpSession) beginCancelDrop() {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	s.bg.dropping = true
	s.stopIdleLocked()
	s.bg.open = false
	s.bg.hasText = false
	s.armSettleLocked()
}

func (s *acpSession) endCancelDrop() {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	s.bg.dropping = false
	s.stopSettleLocked()
}

func (s *acpSession) isDroppingCancelled() bool {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	return s.bg.dropping
}

func (s *acpSession) noteCancelledUpdate() {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	if s.bg.dropping {
		s.armSettleLocked()
	}
}

func (s *acpSession) armIdleLocked() {
	if s.bg.explicitEnd {
		return
	}
	if s.bg.idleTimer == nil {
		s.bg.idleTimer = time.AfterFunc(backgroundTurnIdle, func() {
			s.closeBackgroundTurn("idle")
		})
		return
	}
	s.bg.idleTimer.Reset(backgroundTurnIdle)
}

func (s *acpSession) stopIdleLocked() {
	if s.bg.idleTimer != nil {
		s.bg.idleTimer.Stop()
		s.bg.idleTimer = nil
	}
}

func (s *acpSession) armSettleLocked() {
	if s.bg.settleTimer == nil {
		s.bg.settleTimer = time.AfterFunc(cancelSettleIdle, func() {
			s.endCancelDrop()
		})
		return
	}
	s.bg.settleTimer.Reset(cancelSettleIdle)
}

func (s *acpSession) stopSettleLocked() {
	if s.bg.settleTimer != nil {
		s.bg.settleTimer.Stop()
		s.bg.settleTimer = nil
	}
}

func (s *acpSession) stopBackgroundTimers() {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	s.stopIdleLocked()
	s.stopSettleLocked()
	s.bg.open = false
	s.bg.hasText = false
	s.bg.dropping = false
}
