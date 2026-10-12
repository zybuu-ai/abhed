package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// TurnHandOver is implemented by stores that let a process hand a session's
// running turn to another: HandOver leaves the open session held by no
// process and written by none, and ClaimHandedOver takes it, one claimer
// winning.
type TurnHandOver interface {
	HandOver(ctx context.Context, sessionID, holder string) (bool, error)
	ClaimHandedOver(ctx context.Context, sessionID, holder string) (bool, error)
}

// ErrNothingToTakeOver is TakeOver of a session with nothing left open.
var ErrNothingToTakeOver = errors.New("the session has no run left open to take over")

// The other reasons TakeOver takes nothing: a live process holds the session
// (this one included), there is no such session, or this process is draining.
var (
	ErrSessionBusy = errBusySession
	ErrNoSession   = errNoSession
	ErrDraining    = errDraining
)

// HandOverRunning stops every turn running here so that another process can
// take it over (TakeOver): the run stops without an end, a question it was
// waiting on is left open to be asked again there, background children end
// as a shutdown since they cannot move, and the session is marked handed over
// and leaves this process. It reports how many it handed over. A store that
// cannot hand over leaves the turns running, for the caller to end.
func (s *Server) HandOverRunning(ctx context.Context) int {
	h, ok := s.sessions.(TurnHandOver)
	if !ok {
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
	// Children's ends are written before the session is marked, which fences it.
	for _, live := range lives {
		if live.Loop.Background.Owed() > 0 {
			live.Loop.Background.Close(agent.TermShutdown)
		}
	}
	deadline := time.NewTimer(turnEndWait)
	defer deadline.Stop()
	for _, ch := range ran {
		select {
		case <-ch:
		case <-deadline.C:
			s.log.Warn("turns did not stop in time to be handed over")
		}
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
			stop() // the claim is not released: the mark below replaces it
		}
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), turnEndWait)
		marked, err := h.HandOver(mctx, live.ID, s.holder)
		cancel()
		if err != nil || !marked {
			s.log.Error("could not hand a session over; another process recovers it once its claim is stale",
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
		s.log.Info("handed running turns over", "sessions", n)
	}
	return n
}

// TakeOver continues here a session whose run another process left going:
// one it handed over, or one whose holder's heartbeat has gone stale. Lost
// background children are recorded as such; a call that never ran is asked
// again, through policy and approval; a call that may have run is answered as
// outcome unknown, not run again, and asks a person before it runs again;
// then the turn goes on, on the session owner's behalf while they may act. A
// session whose run had ended is reconciled instead and left ended. It is
// the edition's hook for moving sessions between replicas.
func (s *Server) TakeOver(ctx context.Context, id string) error {
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
		return errors.New("the store cannot hand sessions between processes")
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
		return ErrNothingToTakeOver
	}
	events, err := s.store.Events(id)
	if err != nil {
		return err
	}
	if !agent.Orphaned(events) {
		return ErrNothingToTakeOver
	}
	claimed := false
	if h, ok := s.sessions.(TurnHandOver); ok {
		if claimed, err = h.ClaimHandedOver(ctx, id, s.holder); err != nil {
			return err
		}
	}
	if oc, ok := s.sessions.(OrphanClaimer); ok && !claimed {
		if claimed, err = oc.ClaimOrphan(ctx, id, s.holder, nodeStale); err != nil {
			return err
		}
	}
	if !claimed {
		return errBusySession
	}
	return s.continueClaimed(ctx, id)
}

// continueClaimed continues a session this process has just claimed from a
// process that stopped; see TakeOver.
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
	s.startRunLocked(live, "taken-over turn", func(ctx context.Context) (agent.TerminalReason, error) {
		return live.Loop.ContinueTurn(ctx, events, open)
	})
	s.log.Info("took over a running turn", "session", id, "through_seq", open.ThroughSeq, "open_calls", len(open.Calls))
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
	for _, c := range open.Calls {
		if c.Request.ID == a.RequestID || a.RequestID == "" {
			_ = d.EndApproval(ctx, a.ID)
			return
		}
	}
}
