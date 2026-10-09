package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// Caller is who asks for an operation.
type Caller struct {
	Principal string
	// Admin sees and acts on every sandbox in its scope, not only its own.
	Admin bool
	// Scope, when set, confines the caller to sandboxes carrying each of
	// these labels, and stamps them on every sandbox it creates. The keys are
	// reserved, so no client can set or change them on a sandbox.
	Scope map[string]string
}

func (c Caller) may(s Sandbox) bool {
	return scoped(c.Scope, s.Labels) && (c.Admin || (c.Principal != "" && s.Owner == c.Principal))
}

// Config is what a Service is built from.
type Config struct {
	Store    Store
	Audit    AuditLog
	Drivers  *driver.Registry
	Settings *Settings
	// DefaultDriver is the driver a create that names none uses.
	DefaultDriver string
	// Workspace returns the folder a sandbox works on, creating it if need be.
	Workspace func(id string) (string, error)
	// ReleaseWorkspace, when set, removes what Workspace made, once the
	// sandbox is deleted.
	ReleaseWorkspace func(id string) error
	// WriteProtected are workspace paths no command may write, given to
	// every driver in Spec.WriteProtected.
	WriteProtected []string
	// Egress, when set, gives sandboxes of the drivers it names an egress
	// proxy (Spec.Egress) and each of their commands a credential for it.
	Egress Egress
	// Redact rewrites text before it reaches the record. Nil records a
	// digest of it instead, so nothing unredacted is ever kept.
	Redact func(string) string
	// Interceptor, when set, admits every mutating call before it is
	// committed (Enterprise). Nil admits every call.
	Interceptor Interceptor
	// Ownership, when set, limits this replica to the sandboxes it owns:
	// only they are reconciled, changed or run in here (Enterprise HA).
	Ownership Ownership
	// NodeID names this replica in every event it records; empty on one node.
	NodeID string
	Now    func() time.Time
	Log    *slog.Logger
}

// Egress is the egress proxy for drivers that take it from the control
// plane rather than building their own, as the microvm driver does.
type Egress interface {
	// For reports whether sandboxes of the driver get their proxy here.
	For(driverName string) bool
	// Open starts the sandbox's proxy, or returns the one it has.
	Open(sandboxID string) (driver.Egress, error)
	// Call issues one command's credential: the environment carrying it,
	// and end, which revokes it.
	Call(sandboxID string) (env []string, end func(), err error)
	// Close stops the sandbox's proxy.
	Close(sandboxID string) error
}

// Service is the control plane's operations: the REST API and in-process
// callers share it, so both take one path to the store, record and drivers.
type Service struct {
	cfg Config

	kicks    chan string
	mu       sync.Mutex
	inflight map[string]bool
	again    map[string]bool
	graces   map[string]time.Duration
	running  bool
	work     sync.WaitGroup
	slots    chan struct{}
	guard    atomic.Pointer[guardBox]
	// probe, when set by a test, runs before each driver call of a pass.
	probe func(id string, st State)
}

// New checks cfg and returns the service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("control plane: no store")
	case cfg.Audit == nil:
		return nil, errors.New("control plane: no audit log")
	case cfg.Drivers == nil:
		return nil, errors.New("control plane: no drivers")
	case cfg.Settings == nil:
		return nil, errors.New("control plane: no settings")
	case cfg.Workspace == nil:
		return nil, errors.New("control plane: no workspace")
	}
	if cfg.DefaultDriver == "" {
		cfg.DefaultDriver = "process"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Service{cfg: cfg, kicks: make(chan string, 256), inflight: map[string]bool{},
		again: map[string]bool{}, graces: map[string]time.Duration{}, slots: make(chan struct{}, 8)}, nil
}

// Settings is the resolved settings registry.
func (s *Service) Settings() *Settings { return s.cfg.Settings }

// Store is the store the service writes.
func (s *Service) Store() Store { return s.cfg.Store }

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// record writes ev. A record that cannot be written is ErrUnavailable: the
// caller must not act on a request it could not record.
func (s *Service) record(ctx context.Context, ev AuditEvent) error {
	if ev.At.IsZero() {
		ev.At = s.now()
	}
	ev.Detail = maps.Clone(ev.Detail)
	if s.cfg.NodeID != "" {
		if ev.Detail == nil {
			ev.Detail = map[string]string{}
		}
		ev.Detail["node"] = s.cfg.NodeID
	}
	if err := s.cfg.Audit.Append(context.WithoutCancel(ctx), ev); err != nil {
		s.cfg.Log.Error("control plane: the record could not be written", "action", ev.Action,
			"sandbox", ev.SandboxID, "outcome", ev.Outcome, "err", err)
		return fmt.Errorf("%w: the record cannot be written", ErrUnavailable)
	}
	return nil
}

// outcome records how a recorded request ended. The request has already
// happened, so a failure here is logged by record and not returned.
func (s *Service) outcome(ctx context.Context, ev AuditEvent, err error) {
	ev.Outcome, ev.At = OutcomeOK, time.Time{}
	if err != nil {
		ev.Outcome, ev.Code = OutcomeFailed, ErrorCode(err)
	}
	_ = s.record(ctx, ev)
}

// redact returns text fit for the record.
func (s *Service) redact(text string) string {
	if s.cfg.Redact != nil {
		return s.cfg.Redact(text)
	}
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ErrorCode is the contract's error code for err.
func ErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrDenied):
		return "forbidden"
	case errors.Is(err, ErrNotFound), errors.Is(err, driver.ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrApprovalRequired):
		return "approval_required"
	case errors.Is(err, ErrQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrPreconditionFailed):
		return "precondition_failed"
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrExists), errors.Is(err, ErrVersionMismatch),
		errors.Is(err, ErrIdempotencyInFlight), errors.Is(err, driver.ErrConflict), errors.Is(err, driver.ErrExists):
		return "conflict"
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrIdempotencyMismatch), errors.Is(err, driver.ErrInvalid),
		errors.Is(err, driver.ErrOutputLimit):
		return "invalid"
	case errors.Is(err, driver.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, ErrUnavailable), errors.Is(err, driver.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		return "unavailable"
	}
	return "internal"
}

// CreateRequest is the body of a create.
type CreateRequest struct {
	Template string            `json:"template"`
	Driver   string            `json:"driver,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Settings map[string]string `json:"settings,omitempty"`
}

// Create records a new sandbox in Provisioning; the reconciler builds it.
func (s *Service) Create(ctx context.Context, c Caller, req CreateRequest) (Sandbox, error) {
	name := req.Driver
	if name == "" {
		name = s.cfg.DefaultDriver
	}
	if _, err := s.cfg.Drivers.Get(name); err != nil {
		return Sandbox{}, fmt.Errorf("%w: no driver named %q", ErrInvalid, name)
	}
	if err := s.cfg.Settings.Check(req.Settings); err != nil {
		return Sandbox{}, err
	}
	if err := checkScope(c.Scope); err != nil {
		return Sandbox{}, err
	}
	sb, err := NewSandbox(c.Principal, req.Template, name, req.Labels, s.now())
	if err != nil {
		return Sandbox{}, err
	}
	sb.Settings = maps.Clone(req.Settings)
	if len(c.Scope) > 0 {
		if sb.Labels == nil {
			sb.Labels = map[string]string{}
		}
		maps.Copy(sb.Labels, c.Scope)
		if err := ValidateLabels(sb.Labels, true); err != nil {
			return Sandbox{}, err
		}
	}
	ev := AuditEvent{Principal: c.Principal, Action: "create", SandboxID: sb.ID, Generation: sb.Generation,
		Detail: map[string]string{"template": sb.Template, "driver": sb.Driver}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return Sandbox{}, err
	}
	next := sb.Clone()
	release, err := s.guardAdmit(ctx, &Admission{Action: ActionCreate, Caller: c, Next: &next})
	if err == nil {
		// The guard may only have tightened settings; anything it made
		// invalid or looser than a ceiling is refused here.
		sb.Settings = next.Settings
		if err = s.cfg.Settings.Check(sb.Settings); err != nil {
			release()
		}
	}
	adm := AdmissionRequest{Operation: OpCreate, Caller: c, SandboxID: sb.ID, Object: sb}
	if err == nil {
		// Then the interceptor, which may tighten what the guard admitted.
		if sb, err = s.admit(ctx, adm); err != nil {
			release()
		}
	}
	if err != nil {
		s.outcome(ctx, ev, err)
		return Sandbox{}, err
	}
	adm.Object = sb
	out, err := s.cfg.Store.Create(ctx, sb)
	release()
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, out, err)
	if err != nil {
		return Sandbox{}, storeErr(err)
	}
	s.Kick(out.ID)
	return out, nil
}

func requested(ev AuditEvent) AuditEvent {
	ev.Outcome = OutcomeRequested
	return ev
}

// storeErr keeps the contract's errors and turns anything else from the
// store into ErrUnavailable, so a store fault never reads as success.
func storeErr(err error) error {
	for _, known := range []error{ErrNotFound, ErrExists, ErrInvalid, ErrVersionMismatch, ErrPreconditionFailed,
		ErrInvalidTransition, ErrUnavailable, ErrDenied} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%w: the store failed: %w", ErrUnavailable, err)
}

// Get returns the sandbox, if the caller may see it.
func (s *Service) Get(ctx context.Context, c Caller, id string) (Sandbox, error) {
	sb, err := s.cfg.Store.Get(ctx, id)
	if err != nil {
		return Sandbox{}, storeErr(err)
	}
	if !c.may(sb) {
		return Sandbox{}, fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
	}
	return sb, nil
}

// List lists the caller's sandboxes, or every one for an admin.
func (s *Service) List(ctx context.Context, c Caller, o ListOptions) ([]Sandbox, string, error) {
	if !c.Admin {
		o.Owner = c.Principal
		if o.Owner == "" {
			return nil, "", nil
		}
	}
	if len(c.Scope) > 0 {
		if err := checkScope(c.Scope); err != nil {
			return nil, "", err
		}
		o.Labels = maps.Clone(c.Scope)
	}
	items, next, err := s.cfg.Store.List(ctx, o)
	if err != nil {
		return nil, "", storeErr(err)
	}
	return items, next, nil
}

// Patch is a JSON merge patch of the fields a client may change. A nil
// value in Labels or Settings removes that key.
type Patch struct {
	Labels      map[string]*string
	ClearLabels bool
	Template    *string
	Settings    map[string]*string
	// ClearSettings removes every per-sandbox setting before Settings applies.
	ClearSettings bool
}

// mutate runs fn under Mutate after checking the caller may act, and
// records the request and its outcome.
func (s *Service) mutate(ctx context.Context, c Caller, id, ifMatch, action string, detail map[string]string,
	adm Admission, fn func(*Sandbox) error) (Sandbox, error) {
	cur, err := s.Get(ctx, c, id)
	if err != nil {
		return Sandbox{}, err
	}
	if err := CheckIfMatch(ifMatch, cur.ResourceVersion); err != nil {
		return Sandbox{}, err
	}
	if err := s.owned(ctx, id); err != nil {
		return Sandbox{}, err
	}
	ev := AuditEvent{Principal: c.Principal, Action: action, SandboxID: id, Generation: cur.Generation, Detail: detail}
	if err := s.record(ctx, requested(ev)); err != nil {
		return Sandbox{}, err
	}
	adm.Action, adm.Caller, adm.Sandbox = action, c, cur
	if action == ActionUpdate {
		// What the guard judges is what the patch makes of the object now.
		next := cur.Clone()
		if err := fn(&next); err != nil {
			s.outcome(ctx, ev, err)
			return Sandbox{}, err
		}
		adm.Next = &next
	}
	release, err := s.guardAdmit(ctx, &adm)
	if err != nil {
		s.outcome(ctx, ev, err)
		return Sandbox{}, err
	}
	defer release()
	iadm := AdmissionRequest{Operation: action, Caller: c, SandboxID: id}
	admitted := false
	out, err := Mutate(ctx, s.cfg.Store, id, ifMatch, func(sb *Sandbox) error {
		if !c.may(*sb) {
			return fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
		}
		before := sb.Clone()
		if err := fn(sb); err != nil {
			return err
		}
		iadm.Old, iadm.Object = &before, *sb
		next, err := s.admit(ctx, iadm)
		if err != nil {
			return err
		}
		*sb = next
		iadm.Object, admitted = next, true
		return nil
	})
	if err == nil {
		ev.Generation = out.Generation
	}
	s.outcome(ctx, ev, err)
	// Post-commit hooks hear only of calls that were admitted.
	if admitted {
		s.committed(ctx, iadm, out, err)
	}
	if err != nil {
		return Sandbox{}, storeErr(err)
	}
	return out, nil
}

// Update applies a patch. A template change bumps the generation and takes
// effect at the next start.
func (s *Service) Update(ctx context.Context, c Caller, id, ifMatch string, p Patch) (Sandbox, error) {
	detail := map[string]string{}
	if p.Template != nil {
		detail["template"] = *p.Template
	}
	if p.Labels != nil || p.ClearLabels {
		detail["labels"] = "changed"
	}
	if p.Settings != nil || p.ClearSettings {
		detail["settings"] = "changed"
	}
	return s.mutate(ctx, c, id, ifMatch, ActionUpdate, detail, Admission{}, func(sb *Sandbox) error {
		if sb.State == StateDeleting {
			return fmt.Errorf("%w: sandbox %s is being deleted", ErrInvalidTransition, sb.ID)
		}
		labels := maps.Clone(sb.Labels)
		if p.ClearLabels {
			for k := range labels {
				if !strings.HasPrefix(k, ReservedPrefix) {
					delete(labels, k)
				}
			}
		}
		set := map[string]string{}
		for k, v := range p.Labels {
			if strings.HasPrefix(k, ReservedPrefix) {
				return fmt.Errorf("%w: label %q is reserved", ErrInvalid, k)
			}
			if v == nil {
				delete(labels, k)
				continue
			}
			set[k] = *v
		}
		if err := ValidateLabels(set, false); err != nil {
			return err
		}
		if labels == nil && len(set) > 0 {
			labels = map[string]string{}
		}
		maps.Copy(labels, set)
		if err := ValidateLabels(labels, true); err != nil {
			return err
		}
		settings := maps.Clone(sb.Settings)
		if p.ClearSettings {
			settings = nil
		}
		for k, v := range p.Settings {
			if v == nil {
				delete(settings, k)
				continue
			}
			if settings == nil {
				settings = map[string]string{}
			}
			settings[k] = *v
		}
		if err := s.cfg.Settings.Check(settings); err != nil {
			return err
		}
		if p.Template != nil {
			if err := sb.SetTemplate(*p.Template); err != nil {
				return err
			}
		}
		if len(labels) == 0 {
			labels = nil
		}
		if len(settings) == 0 {
			settings = nil
		}
		sb.Labels, sb.Settings, sb.UpdatedAt = labels, settings, s.now()
		return nil
	})
}

// Start starts a Stopped, Completed or Error sandbox again, as a new
// generation. A client's start resets the restart count.
func (s *Service) Start(ctx context.Context, c Caller, id, ifMatch string) (Sandbox, error) {
	out, err := s.mutate(ctx, c, id, ifMatch, ActionStart, nil, Admission{}, func(sb *Sandbox) error {
		if err := sb.Transition(StateStarting, s.now()); err != nil {
			return err
		}
		sb.Restarts, sb.NextRestart = 0, nil
		return nil
	})
	if err == nil {
		s.Kick(id)
	}
	return out, err
}

// Stop stops a Ready sandbox, giving its processes grace to end.
func (s *Service) Stop(ctx context.Context, c Caller, id, ifMatch string, grace time.Duration) (Sandbox, error) {
	if grace < 0 || grace > time.Hour {
		return Sandbox{}, fmt.Errorf("%w: grace_seconds must be from 0 to 3600", ErrInvalid)
	}
	out, err := s.mutate(ctx, c, id, ifMatch, ActionStop, map[string]string{"grace_seconds": strconv.Itoa(int(grace.Seconds()))},
		Admission{Grace: grace}, func(sb *Sandbox) error { return sb.Transition(StateStopping, s.now()) })
	if err == nil {
		s.mu.Lock()
		s.graces[id] = grace
		s.mu.Unlock()
		s.Kick(id)
	}
	return out, err
}

// Delete moves the sandbox to Deleting; it leaves the store once its driver
// confirms. Deleting one already being deleted returns it as it is.
func (s *Service) Delete(ctx context.Context, c Caller, id, ifMatch string) (Sandbox, error) {
	cur, err := s.Get(ctx, c, id)
	if err != nil {
		return Sandbox{}, err
	}
	if cur.State == StateDeleting {
		if err := CheckIfMatch(ifMatch, cur.ResourceVersion); err != nil {
			return Sandbox{}, err
		}
		s.Kick(id)
		return cur, nil
	}
	out, err := s.mutate(ctx, c, id, ifMatch, ActionDelete, nil, Admission{}, func(sb *Sandbox) error {
		return sb.Transition(StateDeleting, s.now())
	})
	if err == nil {
		s.Kick(id)
	}
	return out, err
}

// ExecRequest is the body of an :exec.
type ExecRequest struct {
	Argv    []string `json:"argv,omitempty"`
	Command string   `json:"command,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Env     []string `json:"env,omitempty"`
	Stdin   bool     `json:"stdin,omitempty"`
}

// ExecIO is where an exec's streams go. Started runs once every check has
// passed, just before the driver runs: an error before it changed nothing.
type ExecIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Started        func(Sandbox)
	// Screen, when set, judges the command line once the request is
	// recorded and before the guard; a refusal is recorded and returned.
	Screen func(command string) error
	// Timeout, when set, replaces exec.timeout_seconds: for a command a
	// caller bounds otherwise, as an SSH session's is by its own limits.
	Timeout time.Duration
}

// Exec runs one command in a Ready sandbox, stopped past exec.output_bytes
// or exec.timeout_seconds. A result comes back only with a real exit code.
func (s *Service) Exec(ctx context.Context, c Caller, id, ifMatch string, req ExecRequest, eio ExecIO) (driver.ExecResult, Sandbox, error) {
	dreq := driver.ExecRequest{Argv: req.Argv, Command: req.Command, Cwd: req.Cwd, Env: req.Env, Stdin: eio.Stdin}
	if err := dreq.Validate(); err != nil {
		return driver.ExecResult{}, Sandbox{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	sb, err := s.Get(ctx, c, id)
	if err != nil {
		return driver.ExecResult{}, Sandbox{}, err
	}
	if err := CheckIfMatch(ifMatch, sb.ResourceVersion); err != nil {
		return driver.ExecResult{}, sb, err
	}
	if !AcceptsWork(sb.State) {
		return driver.ExecResult{}, sb, fmt.Errorf("%w: sandbox %s is %s; only Ready takes exec", ErrInvalidTransition, id, sb.State)
	}
	d, err := s.cfg.Drivers.Get(sb.Driver)
	if err != nil {
		return driver.ExecResult{}, sb, fmt.Errorf("%w: driver %s is not registered", ErrUnavailable, sb.Driver)
	}
	if s.cfg.Egress != nil && s.cfg.Egress.For(sb.Driver) {
		if k, ok := proxyVariable(req.Env); ok {
			return driver.ExecResult{}, sb, fmt.Errorf("%w: %s is the sandbox's own under the egress allowlist", ErrInvalid, k)
		}
		env, end, err := s.cfg.Egress.Call(id)
		if err != nil {
			return driver.ExecResult{}, sb, fmt.Errorf("%w: an egress credential: %w", ErrUnavailable, err)
		}
		defer end()
		dreq.Env = append(slices.Clone(dreq.Env), env...)
	}
	command := req.Command
	if command == "" {
		command = strings.Join(req.Argv, " ")
	}
	ev := AuditEvent{Principal: c.Principal, Action: "exec", SandboxID: id, Generation: sb.Generation,
		Detail: map[string]string{"command": s.redact(command)}}
	if err := s.owned(ctx, id); err != nil {
		return driver.ExecResult{}, sb, err
	}
	if err := s.record(ctx, requested(ev)); err != nil {
		return driver.ExecResult{}, sb, err
	}
	if eio.Screen != nil {
		if err := eio.Screen(command); err != nil {
			s.outcome(ctx, ev, err)
			return driver.ExecResult{}, sb, err
		}
	}
	release, err := s.guardAdmit(ctx, &Admission{Action: ActionExec, Caller: c, Sandbox: sb, Exec: &req})
	if err != nil {
		s.outcome(ctx, ev, err)
		return driver.ExecResult{}, sb, err
	}
	defer release()
	old := sb.Clone()
	adm := AdmissionRequest{Operation: OpExec, Caller: c, SandboxID: id, Old: &old, Object: sb, Command: ev.Detail["command"]}
	if _, err := s.admit(ctx, adm); err != nil {
		s.outcome(ctx, ev, err)
		return driver.ExecResult{}, sb, err
	}
	vals := s.cfg.Settings.For(sb.Settings)
	timeout := vals.Seconds(SetExecTimeout)
	if eio.Timeout > 0 {
		timeout = eio.Timeout
		ev.Detail["timeout_seconds"] = strconv.Itoa(int(timeout.Seconds()))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limit := driver.NewOutputLimit(vals.Int(SetExecOutput))
	limit.OnExceed = cancel
	dreq.Stdout, dreq.Stderr = limit.Wrap(eio.Stdout), limit.Wrap(eio.Stderr)
	if eio.Started != nil {
		eio.Started(sb)
	}
	res, err := d.Exec(ctx, id, dreq)
	switch {
	case limit.Exceeded():
		err = fmt.Errorf("%w: the command wrote more than %d bytes and was stopped", driver.ErrOutputLimit, vals.Int(SetExecOutput))
	case err == nil && ctx.Err() != nil:
		err = ctx.Err()
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("%w: the command ran past %s and was stopped", context.DeadlineExceeded, timeout)
	}
	if err == nil {
		ev.Detail["exit_code"] = strconv.Itoa(res.ExitCode)
	}
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, sb, err)
	if err != nil {
		return driver.ExecResult{}, sb, err
	}
	return res, sb, nil
}

// proxyVariable finds a proxy variable a client tried to set.
func proxyVariable(env []string) (string, bool) {
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			return k, true
		}
	}
	return "", false
}
