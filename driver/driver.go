// Package driver is the interface between Abhed's control plane and the code
// that runs a sandbox: confined host processes, a container, a microVM, a pod.
//
// A driver owns instances, addressed by sandbox id; the control plane owns
// the sandbox objects and their lifecycle. The required methods are what
// every driver can do; attach, copy, forward and snapshot are optional
// interfaces a driver also declares in its capabilities, so it can say what
// it does not support. docs/architecture/control-plane-contract.md is the
// contract; this package is an experimental API until 2.0.
package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Capability names one optional thing a driver can do.
type Capability string

const (
	CapAttach   Capability = "attach"
	CapCopy     Capability = "copy"
	CapForward  Capability = "forward"
	CapSnapshot Capability = "snapshot"
)

// Info describes a driver.
type Info struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities,omitempty"`
}

// Has reports whether the driver declares c.
func (i Info) Has(c Capability) bool {
	for _, x := range i.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// Spec is what the control plane asks a driver to build.
type Spec struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	Template   string `json:"template,omitempty"`
	PolicyRev  string `json:"policy_rev,omitempty"`
	// Workspace is the host folder the sandbox works on, when it has one.
	Workspace string `json:"workspace,omitempty"`
	// MinTier is the weakest isolation acceptable; a driver that cannot meet
	// it fails Create with ErrUnavailable rather than build something weaker.
	MinTier string            `json:"min_tier,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	// Config is driver-specific settings, passed through unread.
	Config map[string]string `json:"config,omitempty"`
}

// Phase is an instance's runtime phase as the driver sees it.
type Phase string

const (
	PhaseRunning Phase = "running"
	PhaseStopped Phase = "stopped"
	PhaseExited  Phase = "exited"
	PhaseFailed  Phase = "failed"
)

// Status is an instance's state as the driver reports it.
type Status struct {
	ID    string `json:"id"`
	Phase Phase  `json:"phase"`
	// Tier is the isolation actually in force, never the one requested.
	Tier    string `json:"tier"`
	Backend string `json:"backend,omitempty"`
	Message string `json:"message,omitempty"`
}

// ExecRequest runs one command in an instance. Exactly one of Argv and
// Command is set; Command is a shell command line.
type ExecRequest struct {
	Argv    []string  `json:"argv,omitempty"`
	Command string    `json:"command,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Env     []string  `json:"env,omitempty"`
	Stdin   io.Reader `json:"-"`
	Stdout  io.Writer `json:"-"`
	Stderr  io.Writer `json:"-"`
}

// Validate refuses a request that names no command or two.
func (r ExecRequest) Validate() error {
	if (len(r.Argv) == 0) == (r.Command == "") {
		return Errorf(ErrInvalid, "exactly one of argv and command must be set")
	}
	return nil
}

// ExecResult is how a command ended. It is returned only when the command
// ran; a driver that cannot tell returns an error instead.
type ExecResult struct {
	ExitCode int `json:"exit_code"`
}

// WinSize is a terminal size.
type WinSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// AttachRequest runs an interactive shell on a terminal in an instance.
type AttachRequest struct {
	Cwd    string         `json:"cwd,omitempty"`
	Size   WinSize        `json:"size"`
	Stdin  io.Reader      `json:"-"`
	Stdout io.Writer      `json:"-"`
	Resize <-chan WinSize `json:"-"`
}

// SnapshotRef names a snapshot a driver took.
type SnapshotRef struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Generation int64  `json:"generation"`
}

// Driver is what every driver implements.
type Driver interface {
	Info() Info
	Create(ctx context.Context, spec Spec) (Status, error)
	Start(ctx context.Context, id string) (Status, error)
	Stop(ctx context.Context, id string, grace time.Duration) (Status, error)
	Delete(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (Status, error)
	Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error)
}

// Attacher is implemented by drivers declaring CapAttach.
type Attacher interface {
	Attach(ctx context.Context, id string, req AttachRequest) (ExecResult, error)
}

// Copier is implemented by drivers declaring CapCopy. Both directions carry
// a tar stream.
type Copier interface {
	CopyIn(ctx context.Context, id, dst string, tar io.Reader) error
	CopyOut(ctx context.Context, id, src string, tar io.Writer) error
}

// Forwarder is implemented by drivers declaring CapForward.
type Forwarder interface {
	Forward(ctx context.Context, id string, port int) (io.ReadWriteCloser, error)
}

// Snapshotter is implemented by drivers declaring CapSnapshot.
type Snapshotter interface {
	Snapshot(ctx context.Context, id, name string) (SnapshotRef, error)
	Restore(ctx context.Context, id string, ref SnapshotRef) error
}

// Check refuses a driver whose declared capabilities do not match what it
// implements, or which declares one this package does not know.
func Check(d Driver) error {
	info := d.Info()
	if !ValidName(info.Name) {
		return Errorf(ErrInvalid, "driver name %q is not valid", info.Name)
	}
	seen := map[Capability]bool{}
	for _, c := range info.Capabilities {
		if seen[c] {
			return Errorf(ErrInvalid, "driver %s declares %s twice", info.Name, c)
		}
		seen[c] = true
		var ok bool
		switch c {
		case CapAttach:
			_, ok = d.(Attacher)
		case CapCopy:
			_, ok = d.(Copier)
		case CapForward:
			_, ok = d.(Forwarder)
		case CapSnapshot:
			_, ok = d.(Snapshotter)
		default:
			return Errorf(ErrInvalid, "driver %s declares unknown capability %q", info.Name, c)
		}
		if !ok {
			return Errorf(ErrInvalid, "driver %s declares %s but does not implement it", info.Name, c)
		}
	}
	return nil
}

func unsupported(d Driver, c Capability) error {
	return Errorf(ErrUnsupported, "driver %s does not support %s", d.Info().Name, c)
}

// Attach runs req through d when d declares CapAttach, else ErrUnsupported.
func Attach(ctx context.Context, d Driver, id string, req AttachRequest) (ExecResult, error) {
	a, ok := d.(Attacher)
	if !ok || !d.Info().Has(CapAttach) {
		return ExecResult{}, unsupported(d, CapAttach)
	}
	return a.Attach(ctx, id, req)
}

// CopyIn copies a tar stream into an instance when d declares CapCopy.
func CopyIn(ctx context.Context, d Driver, id, dst string, tar io.Reader) error {
	c, ok := d.(Copier)
	if !ok || !d.Info().Has(CapCopy) {
		return unsupported(d, CapCopy)
	}
	return c.CopyIn(ctx, id, dst, tar)
}

// CopyOut copies src out of an instance as a tar stream when d declares CapCopy.
func CopyOut(ctx context.Context, d Driver, id, src string, tar io.Writer) error {
	c, ok := d.(Copier)
	if !ok || !d.Info().Has(CapCopy) {
		return unsupported(d, CapCopy)
	}
	return c.CopyOut(ctx, id, src, tar)
}

// Forward opens a stream to port inside an instance when d declares CapForward.
func Forward(ctx context.Context, d Driver, id string, port int) (io.ReadWriteCloser, error) {
	f, ok := d.(Forwarder)
	if !ok || !d.Info().Has(CapForward) {
		return nil, unsupported(d, CapForward)
	}
	if port < 1 || port > 65535 {
		return nil, Errorf(ErrInvalid, "port %d is out of range", port)
	}
	return f.Forward(ctx, id, port)
}

// Snapshot takes a snapshot when d declares CapSnapshot.
func Snapshot(ctx context.Context, d Driver, id, name string) (SnapshotRef, error) {
	s, ok := d.(Snapshotter)
	if !ok || !d.Info().Has(CapSnapshot) {
		return SnapshotRef{}, unsupported(d, CapSnapshot)
	}
	return s.Snapshot(ctx, id, name)
}

// Restore restores a snapshot when d declares CapSnapshot.
func Restore(ctx context.Context, d Driver, id string, ref SnapshotRef) error {
	s, ok := d.(Snapshotter)
	if !ok || !d.Info().Has(CapSnapshot) {
		return unsupported(d, CapSnapshot)
	}
	return s.Restore(ctx, id, ref)
}

// Sentinel errors. A driver wraps one with Errorf so callers match it with
// errors.Is and the out-of-process protocol carries its code.
var (
	ErrUnsupported = errors.New("unsupported")
	ErrNotFound    = errors.New("not_found")
	ErrExists      = errors.New("exists")
	ErrConflict    = errors.New("conflict")
	ErrInvalid     = errors.New("invalid")
	ErrUnavailable = errors.New("unavailable")
	ErrVersion     = errors.New("version")
	ErrInternal    = errors.New("internal")
)

var codes = []error{ErrUnsupported, ErrNotFound, ErrExists, ErrConflict, ErrInvalid, ErrUnavailable, ErrVersion, ErrInternal}

// Error is a driver error with a code.
type Error struct {
	Kind    error
	Message string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Kind }

// Errorf returns an error of kind with a formatted message.
func Errorf(kind error, format string, args ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Code is err's wire code; anything not one of the sentinels is "internal".
func Code(err error) string {
	for _, c := range codes {
		if errors.Is(err, c) {
			return c.Error()
		}
	}
	return ErrInternal.Error()
}

// KindOf maps a wire code back to its sentinel; an unknown code is internal.
func KindOf(code string) error {
	for _, c := range codes {
		if c.Error() == code {
			return c
		}
	}
	return ErrInternal
}

// ValidName reports whether s can name a driver: [a-z][a-z0-9-]{0,31}.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
