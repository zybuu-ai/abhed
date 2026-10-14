// Package sessionsandbox gives each session `abhed serve` runs a
// control-plane sandbox of its own (control_plane.per_session): one per
// session or one per user, made from a template, claimed from a warm pool
// when one is Ready, reattached when the session is continued, and stopped
// or deleted as the retention setting says. The session's bash tool,
// terminals and file tools then work in that sandbox and its folder.
package sessionsandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Scopes and retentions the configuration names.
const (
	ScopeSession = "session"
	ScopeUser    = "user"

	RetainStop   = "stop"
	RetainDelete = "delete"
	RetainKeep   = "keep"

	// DefaultTemplate is the template a session's sandbox is made from when
	// none is configured.
	DefaultTemplate = "session"
)

// defaultReady bounds the wait for a session's sandbox.
const defaultReady = 2 * time.Minute

// stopGrace is what a session's sandbox gets to end its commands when the
// server stops it.
const stopGrace = 10 * time.Second

// ErrNoSandbox is a session that has no sandbox now.
var ErrNoSandbox = errors.New("the session has no sandbox")

// Config is how sessions get their sandboxes.
type Config struct {
	Service *controlplane.Service
	// Scope is ScopeSession (the default) or ScopeUser.
	Scope string
	// Template and Driver are what each sandbox is made from; Driver empty
	// is the service's default.
	Template, Driver string
	// Retention is RetainStop (the default), RetainDelete or RetainKeep: what
	// happens to the sandboxes when the server stops.
	Retention string
	// Pool keeps this many Ready sandboxes to claim; zero keeps none.
	Pool int
	// Ready bounds how long a session waits for its sandbox.
	Ready time.Duration
	// Tenant is the tenant the control plane's store holds; a session in
	// another is refused, so no sandbox is ever shared across tenants.
	Tenant string
	// Workspace is the folder of a sandbox, as the service gives it.
	Workspace func(id string) (string, error)
	// Isolation describes a sandbox's tier to the bash tool.
	Isolation func(sb controlplane.Sandbox) tools.Isolation
	Log       *slog.Logger
}

// Binding is one session asking for its sandbox.
type Binding struct {
	SessionID, User, Tenant string
	// Admin is whether the user is a server administrator, for the edition's scope.
	Admin bool
	// Shared is the server's bash; the session's keeps its secrets and settings.
	Shared tools.Bash
	// Recorder is the session's record; sandbox.bound goes to it before the
	// first command this process runs in the sandbox.
	Recorder *agent.Recorder
}

// Bound is a session's sandbox, Ready.
type Bound struct {
	Sandbox   controlplane.Sandbox
	Workspace string
	// Bash runs the session's commands and terminals in the sandbox.
	Bash tools.Bash
}

// ScopeFunc is an edition's scope for a user's sandboxes, such as the
// reserved label of the Team workspace the user acts in; nil or empty for none.
// An error refuses the session.
type ScopeFunc func(ctx context.Context, req ScopeRequest) (map[string]string, error)

// ScopeRequest is whose sandbox a scope is asked for.
type ScopeRequest struct {
	Tenant, User string
	// Admin is whether the user is a server administrator.
	Admin bool
}

// Manager gives sessions their sandboxes.
type Manager struct {
	cfg  Config
	pool *controlplane.Pool

	// scope is the edition's, set by SetScope before Run.
	scope ScopeFunc
	// pools are the warm pools of each scope, made when a session first
	// needs one; the unscoped one is pool.
	pools map[string]*controlplane.Pool
	// started holds the pools a session asked for, true once running.
	started map[string]bool
	runCtx  context.Context
	runs    sync.WaitGroup

	mu sync.Mutex
	// bound are the sandboxes this process bound, with the sessions on each
	// now, so the server's stop applies retention to them.
	bound    map[string]map[string]bool
	sessions map[string]string
	locks    map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	n int
}

// New checks cfg and returns its manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Service == nil || cfg.Workspace == nil || cfg.Isolation == nil {
		return nil, errors.New("per-session sandboxes: no control plane")
	}
	switch cfg.Scope {
	case "":
		cfg.Scope = ScopeSession
	case ScopeSession, ScopeUser:
	default:
		return nil, fmt.Errorf("control_plane.sessions.scope %q: use %q or %q", cfg.Scope, ScopeSession, ScopeUser)
	}
	switch cfg.Retention {
	case "":
		cfg.Retention = RetainStop
	case RetainStop, RetainDelete, RetainKeep:
	default:
		return nil, fmt.Errorf("control_plane.sessions.retention %q: use %q, %q or %q", cfg.Retention, RetainStop, RetainDelete, RetainKeep)
	}
	if cfg.Template == "" {
		cfg.Template = DefaultTemplate
	}
	if cfg.Ready <= 0 {
		cfg.Ready = defaultReady
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	m := &Manager{cfg: cfg, bound: map[string]map[string]bool{}, sessions: map[string]string{}, locks: map[string]*keyLock{},
		pools: map[string]*controlplane.Pool{}, started: map[string]bool{}}
	if cfg.Pool > 0 {
		p, err := m.newPool(nil)
		if err != nil {
			return nil, err
		}
		m.pool = p
		m.pools[""] = p
	}
	return m, nil
}

func (m *Manager) newPool(scope map[string]string) (*controlplane.Pool, error) {
	p, err := m.cfg.Service.NewPool(controlplane.PoolConfig{Key: m.cfg.Template, Size: m.cfg.Pool, Scope: scope,
		Create: controlplane.CreateRequest{Template: m.cfg.Template, Driver: m.cfg.Driver}})
	if err != nil {
		return nil, fmt.Errorf("control_plane.sessions.warm_pool: %w", err)
	}
	return p, nil
}

// Pool is the unscoped warm pool, nil when there is none.
func (m *Manager) Pool() *controlplane.Pool { return m.pool }

// SetScope installs the edition's scope, before Run. Each session's sandbox
// is then made, claimed and counted in its user's scope, and warm pools are
// kept per scope, from the first session that needs one.
func (m *Manager) SetScope(f ScopeFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scope = f
}

// Run keeps the warm pools full until ctx ends. Close drains them after.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	// Under an edition's scope an unscoped fill would be refused by its
	// guard, so the unscoped pool waits for a session that is unscoped.
	if m.pool != nil && m.scope == nil {
		m.started[""] = false
	}
	for key, run := range m.started {
		if !run {
			m.started[key] = m.startLocked(m.pools[key])
		}
	}
	m.mu.Unlock()
	<-ctx.Done()
	m.mu.Lock()
	m.runCtx = nil
	m.mu.Unlock()
	m.runs.Wait()
}

// startLocked runs p under Run's context, reporting whether it did; the
// caller holds m.mu.
func (m *Manager) startLocked(p *controlplane.Pool) bool {
	if m.runCtx == nil {
		return false
	}
	m.runs.Add(1)
	go func(ctx context.Context) {
		defer m.runs.Done()
		p.Run(ctx)
	}(m.runCtx)
	return true
}

// poolFor is the warm pool of scope, made and started on first use; nil
// when there is no pool.
func (m *Manager) poolFor(scope map[string]string) (*controlplane.Pool, error) {
	if m.cfg.Pool <= 0 {
		return nil, nil
	}
	key := scopeKey(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[key]
	if !ok {
		var err error
		if p, err = m.newPool(scope); err != nil {
			return nil, err
		}
		m.pools[key] = p
	}
	if !m.started[key] {
		m.started[key] = m.startLocked(p)
	}
	return p, nil
}

func scopeKey(scope map[string]string) string {
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + scope[k] + "\x00")
	}
	return b.String()
}

// scopeOf is the edition's scope for the session's user, checked: only
// reserved labels, and never one naming a session or a pool.
func (m *Manager) scopeOf(ctx context.Context, b Binding) (map[string]string, error) {
	m.mu.Lock()
	f := m.scope
	m.mu.Unlock()
	if f == nil {
		return nil, nil
	}
	scope, err := f(ctx, ScopeRequest{Tenant: b.Tenant, User: b.User, Admin: b.Admin})
	if err != nil {
		return nil, err
	}
	for k, v := range scope {
		switch {
		case k == controlplane.LabelSession || k == controlplane.LabelSessionUser || k == controlplane.LabelPool:
			return nil, fmt.Errorf("%w: the edition's scope sets the session label %s", controlplane.ErrForbidden, k)
		case !strings.HasPrefix(k, controlplane.ReservedPrefix) || v == "":
			return nil, fmt.Errorf("%w: the edition's scope %s=%q is not a reserved label with a value", controlplane.ErrForbidden, k, v)
		}
	}
	return maps.Clone(scope), nil
}

// with is labels and scope together.
func with(labels, scope map[string]string) map[string]string {
	out := maps.Clone(labels)
	maps.Copy(out, scope)
	return out
}

// labels are the reserved labels naming the sandbox of a session.
func (m *Manager) labels(sessionID, user, tenant string) map[string]string {
	if m.cfg.Scope == ScopeUser {
		return map[string]string{controlplane.LabelSessionUser: controlplane.UserLabel(tenant, user)}
	}
	return map[string]string{controlplane.LabelSession: sessionID}
}

// caller is the session's user, confined to the sandbox its labels name.
func caller(user string, labels map[string]string) controlplane.Caller {
	return controlplane.Caller{Principal: user, Admin: true, Scope: labels}
}

// maker is who makes and removes the sandboxes: never a user, so a claimed
// pool member and a new sandbox have the same owner.
func maker(labels map[string]string) controlplane.Caller {
	return controlplane.Caller{Principal: controlplane.SessionsPrincipal, Scope: labels}
}

// lock serialises binding one session's, or one user's, sandbox.
func (m *Manager) lock(key string) func() {
	m.mu.Lock()
	l := m.locks[key]
	if l == nil {
		l = &keyLock{}
		m.locks[key] = l
	}
	l.n++
	m.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		m.mu.Lock()
		if l.n--; l.n == 0 {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}
}

func lockKey(labels map[string]string) string {
	if v, ok := labels[controlplane.LabelSession]; ok {
		return "s\x00" + v
	}
	return "u\x00" + labels[controlplane.LabelSessionUser]
}

func (m *Manager) checkTenant(tenant string) error {
	if tenant != m.cfg.Tenant {
		return fmt.Errorf("%w: this server's sandboxes belong to tenant %q, not the session's", controlplane.ErrForbidden, m.cfg.Tenant)
	}
	return nil
}

// find is the sandbox labels name, if any; the oldest when a race made two.
func (m *Manager) find(ctx context.Context, labels map[string]string) (controlplane.Sandbox, bool, error) {
	items, _, err := m.cfg.Service.Store().List(ctx, controlplane.ListOptions{Owner: controlplane.SessionsPrincipal,
		Labels: labels, Limit: controlplane.MaxLimit})
	if err != nil {
		return controlplane.Sandbox{}, false, fmt.Errorf("%w: listing the session's sandbox: %w", controlplane.ErrUnavailable, err)
	}
	live := items[:0]
	for _, sb := range items {
		if sb.State != controlplane.StateDeleting {
			live = append(live, sb)
		}
	}
	if len(live) == 0 {
		return controlplane.Sandbox{}, false, nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i].CreatedAt.Before(live[j].CreatedAt) })
	return live[0], true, nil
}

// Bind gives the session its sandbox, Ready: the one its labels name when
// it has one, started again if it was stopped; else one claimed from the
// warm pool, else a new one. A sandbox that cannot be made Ready, or would
// run with no isolation, is refused; nothing falls back to the shared one.
func (m *Manager) Bind(ctx context.Context, b Binding) (Bound, error) {
	ctx = controlplane.WithOrigin(ctx, controlplane.OriginSession)
	if err := m.checkTenant(b.Tenant); err != nil {
		return Bound{}, err
	}
	labels := m.labels(b.SessionID, b.User, b.Tenant)
	defer m.lock(lockKey(labels))()
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Ready)
	defer cancel()
	scope, err := m.scopeOf(ctx, b)
	if err != nil {
		return Bound{}, err
	}
	// The edition's scope is stamped on what is made and confines the user to it.
	svc, who := m.cfg.Service, caller(b.User, with(labels, scope))

	sb, found, err := m.find(ctx, labels)
	if err != nil {
		return Bound{}, err
	}
	fresh := false
	if found {
		for k, v := range scope {
			if sb.Labels[k] != v {
				return Bound{}, fmt.Errorf("%w: the session's sandbox %s is outside the scope its user acts in now (%s)",
					controlplane.ErrForbidden, sb.ID, k)
			}
		}
		if sb, err = m.wake(ctx, who, sb); err != nil {
			return Bound{}, err
		}
	} else {
		pool, err := m.poolFor(scope)
		if err != nil {
			return Bound{}, err
		}
		claimed := false
		if pool != nil {
			if sb, claimed, err = pool.Claim(ctx, who, labels); err != nil {
				return Bound{}, err
			}
		}
		if !claimed {
			if sb, err = svc.Create(ctx, maker(with(labels, scope)), controlplane.CreateRequest{Template: m.cfg.Template, Driver: m.cfg.Driver}); err != nil {
				return Bound{}, err
			}
		}
		fresh = true
	}
	drop := func(err error) (Bound, error) {
		if fresh {
			// A sandbox made for this bind and refused is not left behind.
			dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.Ready)
			if derr := svc.DeleteNow(dctx, maker(labels), sb.ID); derr != nil {
				m.cfg.Log.Warn("removing a refused session sandbox", "sandbox", sb.ID, "err", derr)
			}
			dcancel()
		}
		return Bound{}, err
	}
	if sb, err = svc.WaitReady(ctx, who, sb.ID); err != nil {
		return drop(err)
	}
	if sb.Tier == "" || sb.Tier == "none" {
		return drop(fmt.Errorf("%w: sandbox %s has no isolation (tier %q), so sessions would not be kept apart", controlplane.ErrUnavailable, sb.ID, sb.Tier))
	}
	ws, err := m.cfg.Workspace(sb.ID)
	if err != nil {
		return drop(fmt.Errorf("%w: the sandbox's folder: %w", controlplane.ErrUnavailable, err))
	}
	m.mu.Lock()
	if m.bound[sb.ID] == nil {
		m.bound[sb.ID] = map[string]bool{}
	}
	m.bound[sb.ID][b.SessionID] = true
	m.sessions[b.SessionID] = sb.ID
	m.mu.Unlock()
	return Bound{Sandbox: sb, Workspace: ws, Bash: m.bash(b, who, sb, ws)}, nil
}

// wake starts a stopped or failed sandbox again, once one stopping has.
func (m *Manager) wake(ctx context.Context, who controlplane.Caller, sb controlplane.Sandbox) (controlplane.Sandbox, error) {
	svc := m.cfg.Service
	if sb.State == controlplane.StateReady {
		// Ready in the store need not be running: a crash loses a process
		// driver's instances. A pass now finds that out before it is used.
		if err := svc.Reconcile(ctx, sb.ID); err != nil {
			return sb, err
		}
		var err error
		if sb, err = svc.Get(ctx, who, sb.ID); err != nil {
			return sb, err
		}
	}
	if sb.State == controlplane.StateStopping {
		if err := svc.Settle(ctx, sb.ID); err != nil {
			return sb, err
		}
		var err error
		if sb, err = svc.Get(ctx, who, sb.ID); err != nil {
			return sb, err
		}
	}
	switch sb.State {
	case controlplane.StateStopped, controlplane.StateError, controlplane.StateCompleted:
		return svc.Start(ctx, who, sb.ID, "")
	}
	return sb, nil
}

// Release is told a session left this process. Its sandbox stays as it is;
// Close and SessionDeleted decide what becomes of it.
func (m *Manager) Release(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.sessions[sessionID]
	if !ok {
		return
	}
	delete(m.sessions, sessionID)
	delete(m.bound[id], sessionID)
}

// SessionDeleted deletes the sandbox of a deleted session, when it has one
// of its own; a user's sandbox stays for the user's other sessions.
func (m *Manager) SessionDeleted(ctx context.Context, sessionID, user, tenant string) error {
	ctx = controlplane.WithOrigin(ctx, controlplane.OriginSession)
	m.Release(sessionID)
	if m.cfg.Scope != ScopeSession || m.checkTenant(tenant) != nil {
		return nil
	}
	labels := m.labels(sessionID, user, tenant)
	defer m.lock(lockKey(labels))()
	sb, found, err := m.find(ctx, labels)
	if err != nil || !found {
		return err
	}
	m.mu.Lock()
	delete(m.bound, sb.ID)
	m.mu.Unlock()
	return m.cfg.Service.DeleteNow(ctx, maker(labels), sb.ID)
}

// Workspace is the folder of a session's sandbox, for a session this
// process is not running. ErrNoSandbox when it has none.
func (m *Manager) Workspace(ctx context.Context, sessionID, user, tenant string) (string, error) {
	if err := m.checkTenant(tenant); err != nil {
		return "", ErrNoSandbox
	}
	sb, found, err := m.find(ctx, m.labels(sessionID, user, tenant))
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNoSandbox
	}
	return m.cfg.Workspace(sb.ID)
}

// Sweep deletes the sandboxes of sessions that no longer exist, as a
// restart finds them: only ones made before since and not bound here, so a
// session being started is never swept. alive answers for a session id; an
// error leaves the sandbox alone.
func (m *Manager) Sweep(ctx context.Context, since time.Time, alive func(ctx context.Context, sessionID string) (bool, error)) error {
	ctx = controlplane.WithOrigin(ctx, controlplane.OriginSession)
	sel, err := controlplane.ParseSelector(controlplane.LabelSession)
	if err != nil {
		return err
	}
	var errs []error
	after := ""
	for {
		items, next, err := m.cfg.Service.Store().List(ctx, controlplane.ListOptions{Owner: controlplane.SessionsPrincipal,
			Selector: sel, Limit: controlplane.MaxLimit, After: after})
		if err != nil {
			return err
		}
		for _, sb := range items {
			m.mu.Lock()
			_, here := m.bound[sb.ID]
			m.mu.Unlock()
			if here || !sb.CreatedAt.Before(since) {
				continue
			}
			id := sb.Labels[controlplane.LabelSession]
			ok, err := alive(ctx, id)
			if err != nil || ok {
				continue
			}
			labels := map[string]string{controlplane.LabelSession: id}
			if err := m.cfg.Service.DeleteNow(ctx, maker(labels), sb.ID); err != nil {
				errs = append(errs, err)
			}
		}
		if next == "" {
			return errors.Join(errs...)
		}
		after = next
	}
}

// Close applies the retention to every sandbox this process bound, as the
// server stops, and drains the warm pools. Run must have returned.
func (m *Manager) Close(ctx context.Context) error {
	ctx = controlplane.WithOrigin(ctx, controlplane.OriginSession)
	m.mu.Lock()
	ids := make([]string, 0, len(m.bound))
	for id := range m.bound {
		ids = append(ids, id)
	}
	m.bound, m.sessions = map[string]map[string]bool{}, map[string]string{}
	m.mu.Unlock()
	var errs []error
	for _, id := range ids {
		sb, err := m.cfg.Service.Store().Get(ctx, id)
		if errors.Is(err, controlplane.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		who := maker(ownLabels(sb))
		switch m.cfg.Retention {
		case RetainStop:
			if sb.State == controlplane.StateReady {
				err = m.cfg.Service.StopNow(ctx, who, id, stopGrace)
			}
		case RetainDelete:
			err = m.cfg.Service.DeleteNow(ctx, who, id)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("sandbox %s: %w", id, err))
		}
	}
	m.mu.Lock()
	pools := make([]*controlplane.Pool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	m.mu.Unlock()
	for _, p := range pools {
		if err := p.Drain(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ownLabels are the session labels a sandbox carries, as a caller's scope.
func ownLabels(sb controlplane.Sandbox) map[string]string {
	out := map[string]string{}
	for _, k := range []string{controlplane.LabelSession, controlplane.LabelSessionUser} {
		if v, ok := sb.Labels[k]; ok {
			out[k] = v
		}
	}
	return out
}

// session is one live session's way into its sandbox.
type session struct {
	m   *Manager
	who controlplane.Caller
	sb  controlplane.Sandbox
	ws  string
	rec *agent.Recorder

	mu        sync.Mutex
	announced bool
}

func (m *Manager) bash(b Binding, who controlplane.Caller, sb controlplane.Sandbox, ws string) tools.Bash {
	s := &session{m: m, who: who, sb: sb, ws: ws, rec: b.Recorder}
	bash := b.Shared
	bash.Sandbox = func(ctx context.Context, cwd, command string) *exec.Cmd {
		return s.command(ctx, driver.CommandRequest{Command: command, Cwd: cwd})
	}
	bash.Shell = func(ctx context.Context, cwd string) *exec.Cmd {
		return s.command(ctx, driver.CommandRequest{Shell: true, Cwd: cwd})
	}
	bash.Forward = func(cmd *exec.Cmd, names []string) { m.cfg.Service.ForwardEnv(sb, cmd, names) }
	// The driver's HOME is inside its sandbox, which the host cannot read.
	bash.Home = func() (string, bool) { return "", false }
	bash.Isolation = m.cfg.Isolation(sb)
	bash.RanUnder = nil
	return bash
}

// announce records sandbox.bound once, before the first command; a record
// that cannot be written runs nothing.
func (s *session) announce() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.announced || s.rec == nil {
		return nil
	}
	if _, err := s.rec.Record(agent.EvSandboxBound, agent.ActorSystem, agent.Trusted, map[string]any{
		"sandbox_id": s.sb.ID, "generation": s.sb.Generation, "driver": s.sb.Driver, "tier": s.sb.Tier,
		"template": s.sb.Template, "workspace": s.ws, "scope": s.m.cfg.Scope,
	}); err != nil {
		return fmt.Errorf("recording the session's sandbox: %w", err)
	}
	s.announced = true
	return nil
}

func (s *session) command(ctx context.Context, req driver.CommandRequest) *exec.Cmd {
	ctx = controlplane.WithOrigin(ctx, controlplane.OriginSession)
	if err := s.announce(); err != nil {
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: %w", err)}
	}
	cmd, _, err := s.m.cfg.Service.Command(ctx, s.who, s.sb.ID, req)
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("sandbox %s: the command was not run: %w", s.sb.ID, err)}
	}
	return cmd
}
