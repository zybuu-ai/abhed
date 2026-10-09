package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// The labels and owner of the sandboxes `abhed serve` makes for its sessions
// (control-plane-contract.md §7). The keys are reserved, so no client can set
// or move them.
const (
	// LabelSession names the session a sandbox belongs to.
	LabelSession = ReservedPrefix + "session"
	// LabelSessionUser names, by digest, the user whose sessions share a
	// sandbox when sandboxes are made per user.
	LabelSessionUser = ReservedPrefix + "session-user"
	// LabelPool marks a warm pool's member that no session has claimed.
	LabelPool = ReservedPrefix + "pool"
	// SessionsPrincipal owns every sandbox made for a session, pooled or
	// not, so ownership never has to move when one is claimed.
	SessionsPrincipal = "system:sessions"
)

// UserLabel is the LabelSessionUser value for a user in a tenant: a digest,
// since a principal need not fit in a label.
func UserLabel(tenant, user string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + user))
	return "u-" + hex.EncodeToString(sum[:16])
}

// Command hands out the process running req in a Ready sandbox, for the
// caller to start and own: a session's bash tool and terminal run their
// commands this way. It is recorded, put to the guard and the interceptor
// as an exec, and refused for a driver that cannot hand one out. Exec's
// timeout and output bound are the caller's to apply.
func (s *Service) Command(ctx context.Context, c Caller, id string, req driver.CommandRequest) (*exec.Cmd, Sandbox, error) {
	if req.Shell != (req.Command == "") {
		return nil, Sandbox{}, fmt.Errorf("%w: exactly one of a command and a shell", ErrInvalid)
	}
	sb, err := s.Get(ctx, c, id)
	if err != nil {
		return nil, Sandbox{}, err
	}
	if !AcceptsWork(sb.State) {
		return nil, sb, fmt.Errorf("%w: sandbox %s is %s; only Ready runs commands", ErrInvalidTransition, id, sb.State)
	}
	d, err := s.cfg.Drivers.Get(sb.Driver)
	if err != nil {
		return nil, sb, fmt.Errorf("%w: driver %s is not registered", ErrUnavailable, sb.Driver)
	}
	if !d.Info().Has(driver.CapCommand) {
		return nil, sb, driver.Errorf(driver.ErrUnsupported, "driver %s cannot run a session's commands", sb.Driver)
	}
	if s.cfg.Egress != nil && s.cfg.Egress.For(sb.Driver) {
		// Its per-call credentials travel in Exec's request, which a handed-out process has none of.
		return nil, sb, driver.Errorf(driver.ErrUnsupported, "driver %s takes its egress from the control plane, which a session's commands cannot carry", sb.Driver)
	}
	line := req.Command
	if req.Shell {
		line = ":shell"
	}
	ev := AuditEvent{Principal: c.Principal, Action: "exec", SandboxID: id, Generation: sb.Generation,
		Detail: map[string]string{"command": s.redact(line), "via": "session"}}
	if err := s.owned(ctx, id); err != nil {
		return nil, sb, err
	}
	if err := s.record(ctx, requested(ev)); err != nil {
		return nil, sb, err
	}
	release, err := s.guardAdmit(ctx, &Admission{Action: ActionExec, Caller: c, Sandbox: sb, Exec: &ExecRequest{Command: line, Cwd: req.Cwd}})
	if err != nil {
		s.outcome(ctx, ev, err)
		return nil, sb, err
	}
	defer release()
	old := sb.Clone()
	adm := AdmissionRequest{Operation: OpExec, Caller: c, SandboxID: id, Old: &old, Object: sb, Command: ev.Detail["command"]}
	if _, err := s.admit(ctx, adm); err != nil {
		s.outcome(ctx, ev, err)
		return nil, sb, err
	}
	cmd, err := driver.Command(ctx, d, id, req)
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, sb, err)
	if err != nil {
		return nil, sb, err
	}
	return cmd, sb, nil
}

// ForwardEnv makes named variables already in cmd.Env reach a command the
// sandbox's driver built.
func (s *Service) ForwardEnv(sb Sandbox, cmd *exec.Cmd, names []string) {
	d, err := s.cfg.Drivers.Get(sb.Driver)
	if err != nil {
		return
	}
	if c, ok := d.(driver.Commander); ok {
		c.ForwardEnv(cmd, names)
	}
}

// awaitPoll bounds how long await sleeps between passes.
const awaitPoll = 250 * time.Millisecond

// await reconciles id now and again until done says it is settled.
func (s *Service) await(ctx context.Context, id string, done func(Sandbox, error) (bool, error)) error {
	delay := 10 * time.Millisecond
	for {
		sb, err := s.cfg.Store.Get(ctx, id)
		if ok, derr := done(sb, err); ok || derr != nil {
			return derr
		}
		if rerr := s.Reconcile(ctx, id); rerr != nil && ctx.Err() == nil {
			s.cfg.Log.Debug("control plane: a pass while waiting failed", "sandbox", id, "err", rerr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: sandbox %s did not settle: %w", ErrUnavailable, id, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, awaitPoll)
	}
}

// WaitReady waits until the sandbox is Ready, making the reconciler's
// passes itself so it does not wait for the next one. A sandbox that ends up
// anywhere else is an error naming why.
func (s *Service) WaitReady(ctx context.Context, c Caller, id string) (Sandbox, error) {
	var out Sandbox
	err := s.await(ctx, id, func(sb Sandbox, err error) (bool, error) {
		if err != nil {
			return false, storeErr(err)
		}
		if !c.may(sb) {
			return false, fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
		}
		out = sb
		switch sb.State {
		case StateReady:
			return true, nil
		case StateProvisioning, StateStarting:
			return false, nil
		}
		why := ""
		if cond, ok := sb.Condition(CondDriverReady); ok && cond.Message != "" {
			why = ": " + cond.Message
		} else if cond, ok := sb.Condition(CondIsolationVerified); ok && cond.Status == ConditionFalse {
			why = ": " + cond.Message
		}
		return false, fmt.Errorf("%w: sandbox %s is %s%s", ErrUnavailable, id, sb.State, why)
	})
	return out, err
}

// Settle waits until no driver call is due for the sandbox's state.
func (s *Service) Settle(ctx context.Context, id string) error {
	return s.await(ctx, id, func(sb Sandbox, err error) (bool, error) {
		if err != nil {
			return false, storeErr(err)
		}
		switch sb.State {
		case StateProvisioning, StateStarting, StateStopping:
			return false, nil
		}
		return true, nil
	})
}

// StopNow stops a Ready sandbox and waits until its driver has stopped it.
func (s *Service) StopNow(ctx context.Context, c Caller, id string, grace time.Duration) error {
	if _, err := s.Stop(ctx, c, id, "", grace); err != nil {
		return err
	}
	return s.await(ctx, id, func(sb Sandbox, err error) (bool, error) {
		if err != nil {
			return false, storeErr(err)
		}
		return sb.State == StateStopped || sb.State == StateError, nil
	})
}

// DeleteNow deletes a sandbox and waits until it has left the store.
func (s *Service) DeleteNow(ctx context.Context, c Caller, id string) error {
	if _, err := s.Delete(ctx, c, id, ""); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return s.await(ctx, id, func(sb Sandbox, err error) (bool, error) {
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, storeErr(err)
		}
		if c, ok := sb.Condition(CondDeleteFailed); ok && c.Status == ConditionTrue {
			return false, fmt.Errorf("%w: deleting sandbox %s: %s", ErrUnavailable, id, c.Message)
		}
		return false, nil
	})
}

// MaxPoolSize bounds a warm pool.
const MaxPoolSize = 16

// PoolConfig is one warm pool: Size Ready sandboxes made from Create and
// kept for sessions to claim.
type PoolConfig struct {
	// Key names the pool in its members' LabelPool; a label value.
	Key  string
	Size int
	// Create is what each member is made from; it carries no labels.
	Create CreateRequest
	// Scope, when set, is reserved labels every member carries and is made
	// under, such as an edition's workspace, so its guard admits and counts
	// each fill as that scope's.
	Scope map[string]string
	// Interval is how often the pool is checked besides after each claim.
	Interval time.Duration
}

// Pool keeps a warm pool full. Members belong to SessionsPrincipal and carry
// LabelPool until one is claimed; a claimed member never goes back.
type Pool struct {
	svc  *Service
	cfg  PoolConfig
	kick chan struct{}
	// mu serialises claims within this process; the guarded update is what
	// keeps two processes from claiming one member.
	mu sync.Mutex
}

// NewPool checks cfg and returns its pool; Run keeps it full.
func (s *Service) NewPool(cfg PoolConfig) (*Pool, error) {
	if !validValue(cfg.Key) || cfg.Key == "" {
		return nil, fmt.Errorf("%w: pool key %q is not a label value", ErrInvalid, cfg.Key)
	}
	if cfg.Size < 1 || cfg.Size > MaxPoolSize {
		return nil, fmt.Errorf("%w: a warm pool holds 1 to %d sandboxes, not %d", ErrInvalid, MaxPoolSize, cfg.Size)
	}
	if len(cfg.Create.Labels) > 0 {
		return nil, fmt.Errorf("%w: pool members carry no labels of their own", ErrInvalid)
	}
	if err := checkScope(cfg.Scope); err != nil {
		return nil, err
	}
	if _, ok := cfg.Scope[LabelPool]; ok {
		return nil, fmt.Errorf("%w: a pool's scope cannot set %s", ErrInvalid, LabelPool)
	}
	cfg.Scope = maps.Clone(cfg.Scope)
	if err := s.cfg.Settings.Check(cfg.Create.Settings); err != nil {
		return nil, err
	}
	if cfg.Interval <= 0 {
		cfg.Interval = s.cfg.Settings.For(nil).Seconds(SetReconcileInterval)
	}
	return &Pool{svc: s, cfg: cfg, kick: make(chan struct{}, 1)}, nil
}

func (p *Pool) caller() Caller {
	return Caller{Principal: SessionsPrincipal, Scope: p.labels()}
}

// labels are what every member carries until it is claimed.
func (p *Pool) labels() map[string]string {
	l := maps.Clone(p.cfg.Scope)
	if l == nil {
		l = map[string]string{}
	}
	l[LabelPool] = p.cfg.Key
	return l
}

// Members lists the pool's unclaimed sandboxes.
func (p *Pool) Members(ctx context.Context) ([]Sandbox, error) {
	var out []Sandbox
	after := ""
	for {
		items, next, err := p.svc.cfg.Store.List(ctx, ListOptions{Owner: SessionsPrincipal,
			Labels: p.labels(), Limit: MaxLimit, After: after})
		if err != nil {
			return nil, storeErr(err)
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// Fill makes members until Size are Ready or on their way, and deletes
// members that failed or stopped; it never holds more than Size.
func (p *Pool) Fill(ctx context.Context) error {
	ms, err := p.Members(ctx)
	if err != nil {
		return err
	}
	live := 0
	var errs []error
	for _, m := range ms {
		switch m.State {
		case StateReady, StateProvisioning, StateStarting:
			live++
		case StateDeleting:
		default:
			if _, err := p.svc.Delete(ctx, p.caller(), m.ID, ""); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	for ; live < p.cfg.Size && ctx.Err() == nil; live++ {
		req := p.cfg.Create
		req.Labels = nil
		req.Settings = maps.Clone(req.Settings)
		if _, err := p.svc.Create(ctx, p.caller(), req); err != nil {
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(errs...)
}

// Kick asks Run to fill the pool soon.
func (p *Pool) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Run fills the pool at once, after each claim and every interval, until
// ctx ends.
func (p *Pool) Run(ctx context.Context) {
	tick := time.NewTicker(p.cfg.Interval)
	defer tick.Stop()
	for {
		if err := p.Fill(ctx); err != nil && ctx.Err() == nil {
			p.svc.cfg.Log.Warn("control plane: filling the warm pool failed", "pool", p.cfg.Key, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		case <-tick.C:
		}
	}
}

// Drain deletes every unclaimed member and waits for each to go.
func (p *Pool) Drain(ctx context.Context) error {
	ms, err := p.Members(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range ms {
		if err := p.svc.DeleteNow(ctx, p.caller(), m.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// errClaimed is a member another claim took first.
var errClaimed = fmt.Errorf("%w: claimed by another session", ErrVersionMismatch)

// Claim takes one Ready member for c: in one update guarded on its
// resource version the member leaves the pool, takes labels, and its
// generation goes up, so nothing bound to it in the pool carries over. It
// is recorded as a claim and put to the guard and the interceptor as an
// update. ok is false when no member is Ready.
func (p *Pool) Claim(ctx context.Context, c Caller, labels map[string]string) (sb Sandbox, ok bool, err error) {
	if err := ValidateLabels(labels, true); err != nil {
		return Sandbox{}, false, err
	}
	for k := range labels {
		if !strings.HasPrefix(k, ReservedPrefix) || k == LabelPool {
			return Sandbox{}, false, fmt.Errorf("%w: a claim sets only the server's own labels, not %q", ErrInvalid, k)
		}
		if _, ok := p.cfg.Scope[k]; ok {
			// A claim never moves a member out of the scope it was made and counted in.
			return Sandbox{}, false, fmt.Errorf("%w: a claim cannot change the pool's scope label %q", ErrInvalid, k)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ms, err := p.Members(ctx)
	if err != nil {
		return Sandbox{}, false, err
	}
	for _, m := range ms {
		if m.State != StateReady {
			continue
		}
		out, err := p.claim(ctx, c, m, labels)
		if errors.Is(err, errClaimed) {
			continue
		}
		if err != nil {
			return Sandbox{}, false, err
		}
		p.Kick()
		return out, true, nil
	}
	return Sandbox{}, false, nil
}

func (p *Pool) claim(ctx context.Context, c Caller, m Sandbox, labels map[string]string) (Sandbox, error) {
	s := p.svc
	if err := s.owned(ctx, m.ID); err != nil {
		return Sandbox{}, err
	}
	claimed := func(sb *Sandbox) error {
		if sb.State != StateReady || sb.Owner != SessionsPrincipal || !scoped(p.labels(), sb.Labels) {
			return errClaimed
		}
		delete(sb.Labels, LabelPool)
		if sb.Labels == nil {
			sb.Labels = map[string]string{}
		}
		maps.Copy(sb.Labels, labels)
		sb.Generation++
		sb.UpdatedAt = s.now()
		return nil
	}
	ev := AuditEvent{Principal: c.Principal, Action: "claim", SandboxID: m.ID, Generation: m.Generation,
		Detail: map[string]string{"pool": p.cfg.Key}}
	for k, v := range labels {
		ev.Detail[strings.TrimPrefix(k, ReservedPrefix)] = v
	}
	if err := s.record(ctx, requested(ev)); err != nil {
		return Sandbox{}, err
	}
	next := m.Clone()
	if err := claimed(&next); err != nil {
		s.outcome(ctx, ev, err)
		return Sandbox{}, err
	}
	release, err := s.guardAdmit(ctx, &Admission{Action: ActionUpdate, Caller: c, Sandbox: m, Next: &next})
	if err != nil {
		s.outcome(ctx, ev, err)
		return Sandbox{}, err
	}
	defer release()
	iadm := AdmissionRequest{Operation: OpUpdate, Caller: c, SandboxID: m.ID}
	admitted := false
	out, err := Mutate(ctx, s.cfg.Store, m.ID, "", func(sb *Sandbox) error {
		before := sb.Clone()
		if err := claimed(sb); err != nil {
			return err
		}
		iadm.Old, iadm.Object = &before, *sb
		adm, err := s.admit(ctx, iadm)
		if err != nil {
			return err
		}
		*sb = adm
		iadm.Object, admitted = adm, true
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		err = errClaimed
	}
	if err == nil {
		ev.Generation = out.Generation
	}
	s.outcome(ctx, ev, err)
	if admitted {
		s.committed(ctx, iadm, out, err)
	}
	if err != nil {
		if errors.Is(err, errClaimed) || errors.Is(err, ErrVersionMismatch) {
			return Sandbox{}, errClaimed
		}
		return Sandbox{}, storeErr(err)
	}
	return out, nil
}
