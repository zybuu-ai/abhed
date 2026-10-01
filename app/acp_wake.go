package app

import (
	"context"
	"errors"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// wakeTurn opens a turn the session starts itself for finished background
// work (contract §6.2): its updates stream as a prompt's do, its asks go to
// the editor, and session/cancel ends it. It reports whether it started.
func (c *acpConn) wakeTurn(s *acpSession, ids []string, run func(context.Context) (string, error)) bool {
	// A workspace file changed since the session opened waits for the next
	// prompt's decision about it; the result stays for that prompt.
	if st, err := config.InspectWorkspace(s.cwd); err == nil {
		s.mu.Lock()
		changed := st.SHA256 != s.trustSHA
		s.mu.Unlock()
		if changed {
			return false
		}
	}
	ctx, cancel := context.WithCancel(c.root())
	s.mu.Lock()
	if s.closed || s.readOnly != "" || s.cancel != nil || s.woken != nil {
		s.mu.Unlock()
		cancel()
		return false
	}
	done := make(chan struct{})
	s.woken = done
	s.beginTurnLocked(ctx, cancel)
	s.mu.Unlock()
	c.notification("_abhed/wake/started", map[string]any{"sessionId": s.id, "taskIds": ids})
	go func() {
		var release func()
		if c.busy != nil {
			release = c.busy()
		}
		if s.undo != nil {
			s.undo.BeginTurn()
		}
		_, err := run(ctx)
		// Every update of the turn goes out before the note that ends it.
		flushed, cancelFlush := context.WithTimeout(context.Background(), flushWait)
		_ = s.agent.Flush(flushed)
		cancelFlush()
		stop, reason := stopReason(ctx, err)
		if errors.Is(err, agent.ErrNothingToWake) {
			stop, reason = "end_turn", "nothing_to_wake"
		}
		cancel()
		s.endTurn()
		s.mu.Lock()
		s.woken = nil
		s.mu.Unlock()
		close(done)
		end := map[string]any{"sessionId": s.id, "taskIds": ids, "stopReason": stop}
		if reason != "" {
			end["reason"] = reason
		}
		c.notification("_abhed/wake/ended", end)
		if release != nil {
			release()
		}
	}()
	return true
}
