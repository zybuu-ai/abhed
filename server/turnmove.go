package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// TurnMover is set by an edition that moves running turns between processes
// on one store. Without one, a turn whose process stops is ended, recorded
// as interrupted, and a person continues the session from there.
type TurnMover interface {
	// HandOver is called as a drain ends, before the turns still running are
	// ended as a shutdown; it reports how many it moved (see SuspendRunning).
	HandOver(ctx context.Context, s *Server) int
	// TakeOver is called for an open session whose run another process left
	// going, by the orphan sweep and by a message to it; and at start-up, with
	// held true, for one this node held before it restarted and has taken
	// back (see ContinueOpenTurn).
	TakeOver(ctx context.Context, s *Server, id string, held bool) error
}

// RowClaim changes the holder of a session's row for holder, reporting
// whether it did: an edition's own mark or claim, under its own protocol.
type RowClaim func(ctx context.Context, sessionID, holder string) (bool, error)

// ErrNoOpenTurn is ContinueOpenTurn of a session with no run left open.
var ErrNoOpenTurn = errors.New("the session has no run left open")

// The other reasons ContinueOpenTurn continues nothing: a live process holds
// the session (this one included), there is no such session, or this
// process is draining.
var (
	ErrSessionBusy = errBusySession
	ErrNoSession   = errNoSession
	ErrDraining    = errDraining
)

// SuspendRunning stops every turn running here without ending it, for a
// TurnMover: the run stops where it is, a question it was waiting on is left
// open, background children end as a shutdown since they cannot move, and
// park marks each row for another process before the session leaves this
// one. It reports how many it parked; a turn it could not stop is left for
// the caller to end.
func (s *Server) SuspendRunning(ctx context.Context, park RowClaim) int {
	if park == nil {
		return 0
	}
	s.mu.Lock()
	var lives []*liveSession
	var ran []chan struct{}
	for _, live := range s.running {
		live.mu.Lock()
		if live.ran != nil && live.cancelCause != nil {
			lives, ran = append(lives, live), append(ran, live.ran)
			live.cancelCause(agent.ErrHandOver)
		}
		live.mu.Unlock()
	}
	s.mu.Unlock()
	// Children's and terminals' ends are written before the session is
	// marked, which fences it.
	var closing []<-chan struct{}
	for _, live := range lives {
		if live.Loop.Background.Owed() > 0 {
			live.Loop.Background.Close(agent.TermShutdown)
		}
		closing = append(closing, live.closeTerminals(closedWithSession)...)
	}
	// One deadline for all: a timer's channel fires once, a context's stays closed.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), turnEndWait)
	defer cancel()
	for _, ch := range ran {
		select {
		case <-ch:
		case <-wctx.Done():
			s.log.Warn("turns did not stop in time to be suspended")
		}
	}
	if !waitClosed(closing, wctx.Done()) {
		s.log.Warn("terminals did not record their end before the turn was suspended")
	}
	n := 0
	for _, live := range lives {
		live.mu.Lock()
		stopped := live.ran == nil && live.Reason == agent.TermHandedOver
		stop := live.beatStop
		if stopped {
			live.beatStop = nil
		}
		live.mu.Unlock()
		if !stopped {
			continue // it ended on its own first, or is still stopping
		}
		if stop != nil {
			stop() // the claim is kept, not released: the mark below replaces it
		}
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), turnEndWait)
		marked, err := park(mctx, live.ID, s.holder)
		cancel()
		if err != nil || !marked {
			s.log.Error("could not park a suspended session; another process recovers it once its claim is stale",
				"session", live.ID, "error", err)
		}
		live.fenced.Store(true)
		s.mu.Lock()
		if s.running[live.ID] == live {
			delete(s.running, live.ID)
		}
		s.mu.Unlock()
		live.closeTerminals(closedWithSession)
		s.releaseBash(live)
		s.sessionEnded(live.ID)
		if err == nil && marked {
			n++
		}
	}
	if n > 0 {
		s.log.Info("suspended running turns", "sessions", n)
	}
	return n
}

// ContinueOpenTurn continues here, for a TurnMover, a session whose run
// another process left going, once claim takes its row for this process (nil
// when it holds the row already). Lost background children are recorded as
// such; a call that never ran is asked again, through policy and approval; a
// call that may have run is answered as outcome unknown, not run again, and
// asks a person before it runs again; then the turn goes on, on the session
// owner's behalf while they may act. A session whose run had ended is
// reconciled instead and left ended.
func (s *Server) ContinueOpenTurn(ctx context.Context, id string, claim RowClaim) error {
	if s.draining.Load() {
		return errDraining
	}
	s.mu.RLock()
	_, here := s.running[id]
	s.mu.RUnlock()
	if here {
		return errBusySession
	}
	g, ok := s.sessions.(sessionGetter)
	if !ok {
		return errors.New("the store cannot continue sessions between processes")
	}
	rec, err := g.GetSession(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errNoSession
	case err != nil:
		return err
	case rec.ParentID != "":
		return errNoSession // a subagent goes on only through its parent
	case rec.EndedAt != nil:
		return ErrNoOpenTurn
	}
	events, err := s.store.Events(id)
	if err != nil {
		return err
	}
	if !agent.Orphaned(events) {
		return ErrNoOpenTurn
	}
	if claim != nil {
		claimed, err := claim(ctx, id, s.holder)
		if err != nil {
			return err
		}
		if !claimed {
			return errBusySession
		}
	}
	return s.continueClaimed(ctx, id)
}

// ReleaseClaim lets go of this process's claim on session id, unless a run
// of it is live here: for a TurnMover whose continue of a session it took
// back at start-up ended early, so the row is not left held until stale.
func (s *Server) ReleaseClaim(id string) {
	s.mu.RLock()
	_, here := s.running[id]
	s.mu.RUnlock()
	if !here {
		s.releaseNode(id)
	}
}

// continueClaimed continues a session this process has just claimed from a
// process that stopped; see ContinueOpenTurn.
func (s *Server) continueClaimed(ctx context.Context, id string) error {
	g, _ := s.sessions.(sessionGetter)
	var rec store.SessionRecord
	var err error
	if g != nil {
		rec, err = g.GetSession(ctx, id)
	}
	events, eerr := s.store.Events(id)
	if err == nil {
		err = eerr
	}
	if err == nil && len(events) == 0 {
		err = errNoSession
	}
	if err != nil {
		s.releaseNode(id)
		return err
	}
	open, inFlight := agent.InFlight(events)
	if !inFlight {
		// Nothing was running: closed as a recovered crash is, and left ended.
		s.reconcile(id, events)
		s.releaseNode(id)
		return nil
	}
	fail := func(err error) error {
		// Not left open with nobody to go on with it: ended as recovered.
		if evs, rerr := s.store.Events(id); rerr == nil {
			s.reconcile(id, evs)
		}
		s.releaseNode(id)
		return err
	}
	if err := agent.ReconcileChildren(s.store, id, events); err != nil {
		return fail(fmt.Errorf("record the lost background tasks: %w", err))
	}
	if events, err = s.store.Events(id); err != nil {
		return fail(err)
	}
	vault, red, err := s.sessionSecrets(StartSpec{User: rec.User, Tenant: rec.Tenant})
	if err != nil {
		return fail(err)
	}
	live, err := s.liveFromRecord(ctx, id, rec, events, "", rec.User, rec.Tenant, vault, red, true, true)
	if err != nil {
		return fail(err)
	}
	s.endOpenApprovals(ctx, id, open)

	s.mu.Lock()
	if s.draining.Load() {
		s.mu.Unlock()
		return fail(errDraining)
	}
	if _, already := s.running[id]; already {
		s.mu.Unlock()
		return errBusySession
	}
	s.running[id] = live
	s.mu.Unlock()
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	if err := s.holdNode(live); err != nil {
		s.dropLive(live)
		return fail(fmt.Errorf("%w: %w", errHoldFailed, err))
	}
	for _, note := range []struct {
		typ     agent.EventType
		payload any
		set     bool
	}{{agent.EvModelSwitched, live.fallback, live.fallback != nil}, {agent.EvPolicyLayer, live.layerNote, live.layerNote != nil}} {
		if !note.set {
			continue
		}
		if _, err := live.Loop.Recorder.Record(note.typ, agent.ActorSystem, agent.Trusted, note.payload); err != nil {
			s.dropLive(live)
			s.releaseNodeNow(live)
			return fail(err)
		}
	}
	live.fallback, live.layerNote = nil, nil
	live.mu.Lock()
	if s.draining.Load() {
		live.mu.Unlock()
		s.dropLive(live)
		s.releaseNodeNow(live)
		return fail(errDraining)
	}
	s.startRunLocked(live, "continued turn", func(ctx context.Context) (agent.TerminalReason, error) {
		return live.Loop.ContinueTurn(ctx, events, open)
	})
	s.log.Info("continued an open turn", "session", id, "through_seq", open.ThroughSeq, "open_calls", len(open.Calls))
	return nil
}

// dropLive removes a session this process was about to run.
func (s *Server) dropLive(live *liveSession) {
	s.mu.Lock()
	if s.running[live.ID] == live {
		delete(s.running, live.ID)
	}
	s.mu.Unlock()
	s.releaseBash(live)
}

// endOpenApprovals closes the row of a question the stopped process was
// waiting on, which is asked again as a new call; an answer to it is refused.
func (s *Server) endOpenApprovals(ctx context.Context, id string, open agent.OpenTurn) {
	d := s.approvalStore()
	if d == nil {
		return
	}
	a, ok, err := d.PendingApproval(ctx, id)
	if err != nil || !ok || a.Ended {
		return
	}
	for _, c := range append(open.Calls, open.Others...) {
		if c.Request.ID == a.RequestID || a.RequestID == "" {
			_ = d.EndApproval(ctx, a.ID)
			return
		}
	}
}
