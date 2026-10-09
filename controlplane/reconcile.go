package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// driverCallTimeout bounds one driver call made by the reconciler.
const driverCallTimeout = 2 * time.Minute

// defaultGrace is the stop grace when the server no longer knows the one asked for.
const defaultGrace = 10 * time.Second

// errStale ends a pass whose sandbox moved on while the driver worked; the
// next pass sees the new state.
var errStale = errors.New("sandbox changed during reconcile")

// errUnchanged skips a write that would change nothing.
var errUnchanged = errors.New("nothing to change")

// Kick asks the reconciler to look at id soon. It never blocks: when the
// queue is full the periodic pass finds the sandbox instead.
func (s *Service) Kick(id string) {
	select {
	case s.kicks <- id:
	default:
	}
}

// Running reports whether Run is reconciling.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Ready reports whether the control plane can serve: its reconciler runs
// and its store answers.
func (s *Service) Ready(ctx context.Context) error {
	if !s.Running() {
		return fmt.Errorf("%w: the reconciler is not running", ErrUnavailable)
	}
	if _, _, err := s.cfg.Store.List(ctx, ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("%w: the store does not answer", ErrUnavailable)
	}
	return nil
}

// Run reconciles until ctx ends: every sandbox each interval, and a kicked
// one at once. It returns after the passes it started have finished.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("control plane: the reconciler is already running")
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.work.Wait()
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	every := s.cfg.Settings.For(nil).Seconds(SetReconcileInterval)
	tick := time.NewTicker(every)
	defer tick.Stop()
	s.all(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-s.kicks:
			s.dispatch(ctx, id)
		case <-tick.C:
			s.all(ctx)
		}
	}
}

// all dispatches every sandbox that may need work.
func (s *Service) all(ctx context.Context) {
	after := ""
	for ctx.Err() == nil {
		items, next, err := s.cfg.Store.List(ctx, ListOptions{Limit: MaxLimit, After: after})
		if err != nil {
			s.cfg.Log.Warn("control plane: listing sandboxes to reconcile failed", "err", err)
			return
		}
		for _, sb := range items {
			if sb.State != StateStopped {
				s.dispatch(ctx, sb.ID)
			}
		}
		if next == "" {
			return
		}
		after = next
	}
}

// dispatch reconciles id on a goroutine of its own, at most one per id and
// a bounded number at once. A dispatch while one runs asks for one more pass.
func (s *Service) dispatch(ctx context.Context, id string) {
	s.mu.Lock()
	if s.inflight[id] {
		s.again[id] = true
		s.mu.Unlock()
		return
	}
	s.inflight[id] = true
	s.mu.Unlock()
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		for {
			select {
			case s.slots <- struct{}{}:
			case <-ctx.Done():
				s.forget(id)
				return
			}
			if err := s.reconcile(ctx, id); err != nil && ctx.Err() == nil {
				s.cfg.Log.Warn("control plane: reconcile failed", "sandbox", id, "err", err)
			}
			<-s.slots
			if !s.done(id) {
				return
			}
		}
	}()
}

// done clears id's in-flight mark, or keeps it and reports true when another
// pass was asked for meanwhile.
func (s *Service) done(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.again[id] {
		delete(s.again, id)
		return true
	}
	delete(s.inflight, id)
	return false
}

// forget clears id's marks when its pass will not run.
func (s *Service) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
	delete(s.again, id)
}

// Reconcile makes one pass over id now, unless a pass over it is running.
func (s *Service) Reconcile(ctx context.Context, id string) error {
	s.mu.Lock()
	if s.inflight[id] {
		s.mu.Unlock()
		return nil
	}
	s.inflight[id] = true
	s.mu.Unlock()
	defer s.forget(id)
	return s.reconcile(ctx, id)
}

func (s *Service) reconcile(ctx context.Context, id string) error {
	sb, err := s.cfg.Store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.probe != nil {
		s.probe(id, sb.State)
	}
	d, derr := s.cfg.Drivers.Get(sb.Driver)
	call, cancel := context.WithTimeout(ctx, driverCallTimeout)
	defer cancel()
	switch sb.State {
	case StateProvisioning, StateStarting:
		if derr != nil {
			return s.settle(ctx, sb, driver.Status{}, fmt.Errorf("%w: driver %s is not registered", driver.ErrUnavailable, sb.Driver))
		}
		spec, err := s.spec(sb)
		if err != nil {
			return s.settle(ctx, sb, driver.Status{}, err)
		}
		var st driver.Status
		if sb.State == StateProvisioning {
			st, err = d.Create(call, spec)
			// Created before a restart of this server: taken as it is.
			if errors.Is(err, driver.ErrExists) {
				st, err = d.Status(call, id)
				if err == nil && st.Phase != driver.PhaseRunning {
					st, err = d.Start(call, id)
				}
			}
		} else {
			st, err = d.Start(call, id)
			// An instance the driver no longer has, after a server restart
			// say, is built again for the new generation.
			if errors.Is(err, driver.ErrNotFound) {
				st, err = d.Create(call, spec)
			}
		}
		return s.settle(ctx, sb, st, err)
	case StateStopping:
		if derr != nil {
			return s.observe(ctx, sb, StateError, ConditionFalse, "DriverMissing", derr.Error())
		}
		s.mu.Lock()
		grace, ok := s.graces[id]
		delete(s.graces, id)
		s.mu.Unlock()
		if !ok {
			grace = defaultGrace
		}
		_, err := d.Stop(call, id, grace)
		if err != nil && !errors.Is(err, driver.ErrNotFound) {
			return s.observe(ctx, sb, StateError, ConditionFalse, driver.Code(err), err.Error())
		}
		return s.observe(ctx, sb, StateStopped, ConditionFalse, "Stopped", "")
	case StateDeleting:
		return s.remove(ctx, call, sb, d, derr)
	case StateReady:
		if derr != nil {
			return s.observe(ctx, sb, StateError, ConditionFalse, "DriverMissing", derr.Error())
		}
		st, err := d.Status(call, id)
		return s.watch(ctx, sb, st, err)
	case StateError, StateCompleted:
		return s.restart(ctx, sb)
	}
	return nil
}

// spec is what the driver is asked to build for sb.
func (s *Service) spec(sb Sandbox) (driver.Spec, error) {
	ws, err := s.cfg.Workspace(sb.ID)
	if err != nil {
		return driver.Spec{}, fmt.Errorf("%w: the sandbox's workspace: %w", driver.ErrUnavailable, err)
	}
	vals := s.cfg.Settings.For(sb.Settings)
	minTier := vals[SetMinTier]
	if minTier == "none" {
		minTier = ""
	}
	spec := driver.Spec{ID: sb.ID, Generation: sb.Generation, Template: sb.Template, PolicyRev: sb.PolicyRev,
		Workspace: ws, WriteProtected: slices.Clone(s.cfg.WriteProtected), MinTier: minTier, Labels: sb.Labels,
		Config: resourceConfig(vals)}
	if s.cfg.Egress != nil && s.cfg.Egress.For(sb.Driver) {
		// Without its proxy the sandbox is not built, rather than built with no way out it was promised.
		eg, err := s.cfg.Egress.Open(sb.ID)
		if err != nil {
			return driver.Spec{}, fmt.Errorf("%w: the sandbox's egress proxy: %w", driver.ErrUnavailable, err)
		}
		spec.Egress = &eg
	}
	return spec, nil
}

// guarded writes fn's change only while sb is still in the state and
// generation the pass acted on.
func (s *Service) guarded(ctx context.Context, sb Sandbox, fn func(*Sandbox) error) (Sandbox, error) {
	out, err := Mutate(ctx, s.cfg.Store, sb.ID, "", func(cur *Sandbox) error {
		if cur.State != sb.State || cur.Generation != sb.Generation {
			return errStale
		}
		return fn(cur)
	})
	if errors.Is(err, errStale) || errors.Is(err, errUnchanged) || errors.Is(err, ErrNotFound) {
		return sb, nil
	}
	return out, err
}

func (s *Service) note(ctx context.Context, sb Sandbox, from State, reason string) {
	if sb.State == from {
		return
	}
	_ = s.record(ctx, AuditEvent{Principal: SystemPrincipal, Action: "reconcile", SandboxID: sb.ID,
		Generation: sb.Generation, Outcome: OutcomeOK,
		Detail: map[string]string{"from": string(from), "to": string(sb.State), "reason": reason}})
}

// settle records how a create or start went. Anything but a running
// instance at the tier the sandbox requires is Error; nothing falls back.
func (s *Service) settle(ctx context.Context, sb Sandbox, st driver.Status, err error) error {
	now := s.now()
	if err == nil && st.Phase != driver.PhaseRunning {
		err = driver.Errorf(driver.ErrUnavailable, "the driver reports the instance %s, not running", st.Phase)
	}
	minTier := s.cfg.Settings.For(sb.Settings)[SetMinTier]
	weak := err == nil && !TierMeets(st.Tier, minTier)
	if weak {
		// Not left running weaker than asked: stopped before it is recorded.
		if d, derr := s.cfg.Drivers.Get(sb.Driver); derr == nil {
			stopCtx, cancel := context.WithTimeout(ctx, driverCallTimeout)
			_, _ = d.Stop(stopCtx, sb.ID, 0)
			cancel()
		}
	}
	reason := "Started"
	out, werr := s.guarded(ctx, sb, func(cur *Sandbox) error {
		switch {
		case err != nil:
			reason = driver.Code(err)
			if terr := cur.Transition(StateError, now); terr != nil {
				return terr
			}
			return cur.SetCondition(Condition{Type: CondDriverReady, Status: ConditionFalse, Reason: reason, Message: err.Error()}, now)
		case weak:
			reason = "WeakerThanRequired"
			cur.Tier = st.Tier
			if terr := cur.Transition(StateError, now); terr != nil {
				return terr
			}
			return cur.SetCondition(Condition{Type: CondIsolationVerified, Status: ConditionFalse, Reason: reason,
				Message: fmt.Sprintf("the driver reports %q, weaker than the required %q", st.Tier, minTier)}, now)
		}
		cur.Tier = st.Tier
		if terr := cur.Transition(StateReady, now); terr != nil {
			return terr
		}
		if serr := cur.SetCondition(Condition{Type: CondDriverReady, Status: ConditionTrue, Reason: reason}, now); serr != nil {
			return serr
		}
		return cur.SetCondition(Condition{Type: CondIsolationVerified, Status: ConditionTrue, Reason: "TierMeetsPolicy",
			Message: "tier " + st.Tier}, now)
	})
	if werr != nil {
		return werr
	}
	s.note(ctx, out, sb.State, reason)
	if out.State == StateError {
		s.Kick(sb.ID)
	}
	return nil
}

// observe moves sb to state with its DriverReady condition, when it is still where the
// pass found it.
func (s *Service) observe(ctx context.Context, sb Sandbox, to State, status ConditionStatus, reason, msg string) error {
	now := s.now()
	out, err := s.guarded(ctx, sb, func(cur *Sandbox) error {
		if cur.State != to {
			if err := cur.Transition(to, now); err != nil {
				return err
			}
		}
		return cur.SetCondition(Condition{Type: CondDriverReady, Status: status, Reason: reason, Message: msg}, now)
	})
	if err != nil {
		return err
	}
	s.note(ctx, out, sb.State, reason)
	if out.State == StateError {
		s.Kick(sb.ID)
	}
	return nil
}

// watch compares a Ready sandbox with what its driver reports.
func (s *Service) watch(ctx context.Context, sb Sandbox, st driver.Status, err error) error {
	switch {
	case errors.Is(err, driver.ErrNotFound):
		return s.observe(ctx, sb, StateError, ConditionFalse, "InstanceLost", err.Error())
	case err != nil:
		// The driver cannot tell: the condition says so, and nothing that
		// accepts work is assumed either way.
		if c, ok := sb.Condition(CondDriverReady); ok && c.Status == ConditionUnknown {
			return err
		}
		if oerr := s.observe(ctx, sb, StateReady, ConditionUnknown, "StatusFailed", err.Error()); oerr != nil {
			return oerr
		}
		return err
	case st.Phase == driver.PhaseExited:
		return s.observe(ctx, sb, StateCompleted, ConditionFalse, "Exited", st.Message)
	case st.Phase == driver.PhaseFailed:
		return s.observe(ctx, sb, StateError, ConditionFalse, "Failed", st.Message)
	case st.Phase != driver.PhaseRunning:
		return s.observe(ctx, sb, StateError, ConditionFalse, "StoppedOutsideControlPlane",
			"the driver reports the instance "+string(st.Phase))
	case !TierMeets(st.Tier, s.cfg.Settings.For(sb.Settings)[SetMinTier]):
		return s.settle(ctx, sb, st, nil)
	}
	now := s.now()
	_, werr := s.guarded(ctx, sb, func(cur *Sandbox) error {
		changed := false
		if c, ok := cur.Condition(CondDriverReady); !ok || c.Status != ConditionTrue {
			if err := cur.SetCondition(Condition{Type: CondDriverReady, Status: ConditionTrue, Reason: "Running"}, now); err != nil {
				return err
			}
			changed = true
		}
		// A sandbox that has run stably since its last restart starts its
		// restart budget again.
		if c, ok := cur.Condition(CondDriverReady); cur.Restarts > 0 && ok && c.Status == ConditionTrue && now.Sub(c.LastTransition) >= StableAfter {
			cur.Restarts = 0
			changed = true
		}
		if !changed {
			return errUnchanged
		}
		cur.UpdatedAt = now
		return nil
	})
	return werr
}

// restart applies the restart policy to a sandbox that ended.
func (s *Service) restart(ctx context.Context, sb Sandbox) error {
	vals := s.cfg.Settings.For(sb.Settings)
	now := s.now()
	if !RestartDue(vals[SetRestartPolicy], sb.State) {
		if sb.NextRestart == nil {
			return nil
		}
		_, err := s.guarded(ctx, sb, func(cur *Sandbox) error {
			cur.NextRestart = nil
			return nil
		})
		return err
	}
	if int64(sb.Restarts) >= vals.Int(SetMaxRestarts) {
		_, err := s.guarded(ctx, sb, func(cur *Sandbox) error {
			if c, ok := cur.Condition(CondRestartScheduled); ok && c.Status == ConditionFalse && c.Reason == "LimitReached" && cur.NextRestart == nil {
				return errUnchanged
			}
			cur.NextRestart = nil
			return cur.SetCondition(Condition{Type: CondRestartScheduled, Status: ConditionFalse, Reason: "LimitReached",
				Message: fmt.Sprintf("restarted %d times; restart.max_restarts is %d", cur.Restarts, vals.Int(SetMaxRestarts))}, now)
		})
		return err
	}
	if sb.NextRestart == nil {
		at := now.Add(Backoff(vals.Seconds(SetRestartBackoff), sb.Restarts))
		_, err := s.guarded(ctx, sb, func(cur *Sandbox) error {
			cur.NextRestart = &at
			return cur.SetCondition(Condition{Type: CondRestartScheduled, Status: ConditionTrue, Reason: "Backoff",
				Message: "restarting at " + at.Format(time.RFC3339)}, now)
		})
		if err == nil {
			time.AfterFunc(at.Sub(now), func() { s.Kick(sb.ID) })
		}
		return err
	}
	if now.Before(*sb.NextRestart) {
		return nil
	}
	ev := AuditEvent{Principal: SystemPrincipal, Action: "restart", SandboxID: sb.ID, Generation: sb.Generation,
		Detail: map[string]string{"policy": vals[SetRestartPolicy], "restarts": fmt.Sprint(sb.Restarts + 1), "from": string(sb.State)}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return err
	}
	out, err := s.guarded(ctx, sb, func(cur *Sandbox) error {
		if err := cur.Transition(StateStarting, now); err != nil {
			return err
		}
		cur.Restarts++
		cur.NextRestart = nil
		return cur.SetCondition(Condition{Type: CondRestartScheduled, Status: ConditionFalse, Reason: "Restarted"}, now)
	})
	ev.Generation = out.Generation
	s.outcome(ctx, ev, err)
	if err == nil && out.State == StateStarting {
		s.Kick(sb.ID)
	}
	return err
}

// remove deletes the instance and then the object. A failed driver delete
// leaves the sandbox in Deleting with DeleteFailed and is tried again.
func (s *Service) remove(ctx, call context.Context, sb Sandbox, d driver.Driver, derr error) error {
	err := derr
	if err == nil {
		err = d.Delete(call, sb.ID)
		if errors.Is(err, driver.ErrNotFound) {
			err = nil
		}
	}
	if err == nil && s.cfg.ReleaseWorkspace != nil {
		err = s.cfg.ReleaseWorkspace(sb.ID)
	}
	if err == nil && s.cfg.Egress != nil && s.cfg.Egress.For(sb.Driver) {
		err = s.cfg.Egress.Close(sb.ID)
	}
	if err != nil {
		now := s.now()
		_, werr := s.guarded(ctx, sb, func(cur *Sandbox) error {
			if c, ok := cur.Condition(CondDeleteFailed); ok && c.Status == ConditionTrue && c.Message == err.Error() {
				return errUnchanged
			}
			return cur.SetCondition(Condition{Type: CondDeleteFailed, Status: ConditionTrue, Reason: driver.Code(err), Message: err.Error()}, now)
		})
		if werr != nil {
			return werr
		}
		return err
	}
	if err := s.cfg.Store.Delete(ctx, sb.ID, sb.ResourceVersion); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if errors.Is(err, ErrVersionMismatch) {
			s.Kick(sb.ID)
			return nil
		}
		return err
	}
	_ = s.record(ctx, AuditEvent{Principal: SystemPrincipal, Action: "reconcile", SandboxID: sb.ID,
		Generation: sb.Generation, Outcome: OutcomeOK, Detail: map[string]string{"from": string(StateDeleting), "to": "removed"}})
	return nil
}

// resourceConfig is the size in force as the driver keys it, nil when the
// sandbox leaves its size to the driver.
func resourceConfig(vals Values) map[string]string {
	var out map[string]string
	for key, as := range map[string]string{SetCPUs: "cpus", SetMemoryMB: "memory_mb"} {
		if v := vals[key]; v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[as] = v
		}
	}
	return out
}
