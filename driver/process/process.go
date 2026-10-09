// Package process is the built-in driver over Abhed's existing sandbox tiers.
// It builds each instance exactly as a session's sandbox is built today, from
// the configuration (Seatbelt or bubblewrap, the fence when sandbox.tier is
// fence, or the strongest tier available), and runs commands through it
// unchanged. It adds lifecycle around the sandbox, not confinement.
package process

import (
	"context"
	"errors"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"mvdan.cc/sh/v3/syntax"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
)

// Name is the driver's registered name.
const Name = "process"

// Driver runs instances under the configured sandbox tier.
type Driver struct {
	build func(spec driver.Spec) (sandbox.Sandbox, error)

	mu    sync.Mutex
	insts map[string]*instance
}

type instance struct {
	spec    driver.Spec
	sb      sandbox.Sandbox
	stopped bool
	// runCtx ends every command the instance is running when it stops.
	runCtx  context.Context
	stopRun context.CancelFunc
	running sync.WaitGroup
}

// New returns the driver building sandboxes from cfg, as a session does.
func New(cfg config.Config) *Driver {
	return newDriver(func(spec driver.Spec) (sandbox.Sandbox, error) {
		return sandboxconfig.Build(cfg, spec.Workspace)
	})
}

func newDriver(build func(driver.Spec) (sandbox.Sandbox, error)) *Driver {
	return &Driver{build: build, insts: map[string]*instance{}}
}

func (d *Driver) Info() driver.Info {
	return driver.Info{Name: Name, Version: "1", Capabilities: []driver.Capability{driver.CapAttach}}
}

// tiers are the names Spec.MinTier may use; anything else is refused, since
// an unknown name would otherwise compare as the weakest.
var tiers = map[string]sandbox.Tier{
	string(sandbox.TierNone): sandbox.TierNone, string(sandbox.TierProcess): sandbox.TierProcess,
	string(sandbox.TierFence): sandbox.TierFence, string(sandbox.TierContainer): sandbox.TierContainer,
	string(sandbox.TierVM): sandbox.TierVM,
}

func status(id string, in *instance) driver.Status {
	st := driver.Status{ID: id, Phase: driver.PhaseRunning}
	if in.stopped {
		st.Phase = driver.PhaseStopped
	}
	if in.sb != nil {
		st.Tier, st.Message = string(in.sb.Tier()), in.sb.Describe()
		if i, ok := in.sb.(sandbox.Interactive); ok {
			st.Backend = i.Backend()
		}
	}
	return st
}

// open builds the sandbox for spec and checks it meets spec.MinTier.
func (d *Driver) open(spec driver.Spec) (sandbox.Sandbox, error) {
	sb, err := d.build(spec)
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "%v", err)
	}
	if spec.MinTier != "" && sb.Tier().Strength() < tiers[spec.MinTier].Strength() {
		_ = sandbox.Close(sb)
		return nil, driver.Errorf(driver.ErrUnavailable, "the sandbox here is %s, weaker than the required %s", sb.Tier(), spec.MinTier)
	}
	return sb, nil
}

func (d *Driver) Create(_ context.Context, spec driver.Spec) (driver.Status, error) {
	if spec.ID == "" {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "spec has no id")
	}
	if spec.Workspace == "" || !filepath.IsAbs(spec.Workspace) {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "the process driver needs an absolute workspace")
	}
	if _, ok := tiers[spec.MinTier]; spec.MinTier != "" && !ok {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "unknown tier %q", spec.MinTier)
	}
	// Nothing here can hold one sandbox to a size or any other setting, so
	// a Spec asking for one is refused rather than run without it.
	if len(spec.Config) > 0 {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid,
			"the process driver cannot apply %s; use a driver that bounds each sandbox",
			strings.Join(slices.Sorted(maps.Keys(spec.Config)), ", "))
	}
	d.mu.Lock()
	if _, ok := d.insts[spec.ID]; ok {
		d.mu.Unlock()
		return driver.Status{}, driver.Errorf(driver.ErrExists, "instance %s exists", spec.ID)
	}
	// Reserve the id while building, so a second create of it fails.
	in := &instance{spec: spec, stopped: true}
	d.insts[spec.ID] = in
	d.mu.Unlock()

	sb, err := d.open(spec)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.insts[spec.ID] != in {
		if err == nil {
			_ = sandbox.Close(sb)
		}
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s was deleted while it was created", spec.ID)
	}
	if err != nil {
		delete(d.insts, spec.ID)
		return driver.Status{}, err
	}
	in.sb, in.stopped = sb, false
	in.runCtx, in.stopRun = context.WithCancel(context.Background())
	return status(spec.ID, in), nil
}

func (d *Driver) get(id string) (*instance, error) {
	in, ok := d.insts[id]
	if !ok {
		return nil, driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	return in, nil
}

func (d *Driver) Status(_ context.Context, id string) (driver.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.get(id)
	if err != nil {
		return driver.Status{}, err
	}
	return status(id, in), nil
}

// Start builds the sandbox again for a stopped instance.
func (d *Driver) Start(_ context.Context, id string) (driver.Status, error) {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.Status{}, err
	}
	if !in.stopped {
		st := status(id, in)
		d.mu.Unlock()
		return st, nil
	}
	spec := in.spec
	d.mu.Unlock()

	sb, err := d.open(spec)
	if err != nil {
		return driver.Status{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur, ok := d.insts[id]; !ok || cur != in || !in.stopped || in.sb != nil {
		_ = sandbox.Close(sb)
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s changed while starting", id)
	}
	in.sb, in.stopped = sb, false
	in.runCtx, in.stopRun = context.WithCancel(context.Background())
	return status(id, in), nil
}

// Stop refuses new commands, gives running ones grace to finish, ends the
// rest, and closes the sandbox, which releases the fence's cgroup.
func (d *Driver) Stop(_ context.Context, id string, grace time.Duration) (driver.Status, error) {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.Status{}, err
	}
	if in.stopped {
		st := status(id, in)
		d.mu.Unlock()
		return st, nil
	}
	in.stopped = true
	sb, stopRun := in.sb, in.stopRun
	d.mu.Unlock()

	drained := make(chan struct{})
	go func() { in.running.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(grace):
		stopRun()
		<-drained
	}
	stopRun()
	closeErr := sandbox.Close(sb)

	d.mu.Lock()
	defer d.mu.Unlock()
	in.sb = nil
	if closeErr != nil {
		return driver.Status{}, driver.Errorf(driver.ErrInternal, "closing the sandbox: %v", closeErr)
	}
	return status(id, in), nil
}

func (d *Driver) Delete(ctx context.Context, id string) error {
	if _, err := d.Stop(ctx, id, 0); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.get(id); err != nil {
		return err
	}
	delete(d.insts, id)
	return nil
}

// begin admits one command to a running instance; done must be called after.
func (d *Driver) begin(ctx context.Context, id string) (*instance, context.Context, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.get(id)
	if err != nil {
		return nil, nil, nil, err
	}
	if in.stopped || in.sb == nil {
		return nil, nil, nil, driver.Errorf(driver.ErrConflict, "instance %s is stopped", id)
	}
	in.running.Add(1)
	cctx, cancel := context.WithCancel(ctx)
	unhook := context.AfterFunc(in.runCtx, cancel)
	return in, cctx, func() { unhook(); cancel(); in.running.Done() }, nil
}

// cwd resolves dir against the workspace and refuses one outside it.
func cwd(workspace, dir string) (string, error) {
	if dir == "" {
		return workspace, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(workspace, dir)
	}
	dir = filepath.Clean(dir)
	if dir != workspace && !strings.HasPrefix(dir, workspace+string(filepath.Separator)) {
		return "", driver.Errorf(driver.ErrInvalid, "cwd %s is outside the workspace", dir)
	}
	return dir, nil
}

func commandLine(req driver.ExecRequest) (string, error) {
	if req.Command != "" {
		return req.Command, nil
	}
	parts := make([]string, len(req.Argv))
	for i, a := range req.Argv {
		q, err := syntax.Quote(a, syntax.LangBash)
		if err != nil {
			return "", driver.Errorf(driver.ErrInvalid, "argv[%d] cannot be quoted: %v", i, err)
		}
		parts[i] = q
	}
	return strings.Join(parts, " "), nil
}

// addEnv appends env to cmd, refusing a variable the sandbox already set:
// a caller may add to the environment, never replace what confines it.
func addEnv(cmd *exec.Cmd, env []string) error {
	if len(env) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, kv := range cmd.Env {
		k, _, _ := strings.Cut(kv, "=")
		set[k] = true
	}
	names := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return driver.Errorf(driver.ErrInvalid, "env entry %q is not NAME=value", kv)
		}
		if set[k] {
			return driver.Errorf(driver.ErrInvalid, "env %s is set by the sandbox and cannot be replaced", k)
		}
		names = append(names, k)
	}
	cmd.Env = append(cmd.Env, env...)
	// A container sees only what is forwarded to it by name.
	sandbox.ForwardEnv(cmd, names)
	return nil
}

func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.ExecResult, error) {
	if err := req.Validate(); err != nil {
		return driver.ExecResult{}, err
	}
	line, err := commandLine(req)
	if err != nil {
		return driver.ExecResult{}, err
	}
	in, cctx, done, err := d.begin(ctx, id)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer done()
	dir, err := cwd(in.spec.Workspace, req.Cwd)
	if err != nil {
		return driver.ExecResult{}, err
	}
	cmd := in.sb.Command(cctx, dir, line)
	// A cancelled call ends what the command started, not only its shell.
	sandbox.EndWithCommand(cmd)
	if err := addEnv(cmd, req.Env); err != nil {
		sandbox.Release(cmd)
		return driver.ExecResult{}, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = req.Stdin, req.Stdout, req.Stderr
	return result(ctx, cmd, cmd.Run())
}

// result is how cmd ended, or an error when it did not run to an end or
// the caller gave up on it. A command the instance's Stop ended has an exit
// code like any other.
func result(ctx context.Context, cmd *exec.Cmd, err error) (driver.ExecResult, error) {
	if cmd.ProcessState == nil {
		sandbox.Release(cmd)
		if ctx.Err() != nil {
			return driver.ExecResult{}, ctx.Err()
		}
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the command was not run: %v", err)
	}
	if ctx.Err() != nil {
		return driver.ExecResult{}, ctx.Err()
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		return driver.ExecResult{}, driver.Errorf(driver.ErrInternal, "%v", err)
	}
	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = 128 + int(ws.Signal())
	}
	return driver.ExecResult{ExitCode: code}, nil
}

// Attach runs the sandbox's interactive shell on a terminal.
func (d *Driver) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.ExecResult, error) {
	in, cctx, done, err := d.begin(ctx, id)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer done()
	sh, ok := in.sb.(sandbox.Interactive)
	if !ok {
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnsupported, "the %s tier here has no interactive shell", in.sb.Tier())
	}
	dir, err := cwd(in.spec.Workspace, req.Cwd)
	if err != nil {
		return driver.ExecResult{}, err
	}
	cmd := sh.Shell(cctx, dir)
	if cmd.Err != nil {
		sandbox.Release(cmd)
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the shell was not started: %v", cmd.Err)
	}
	if cmd.Env == nil {
		cmd.Env = sandbox.HostCommandEnv()
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	size := &pty.Winsize{Rows: req.Size.Rows, Cols: req.Size.Cols}
	if size.Rows == 0 || size.Cols == 0 {
		size = &pty.Winsize{Rows: 30, Cols: 100}
	}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		sandbox.Release(cmd)
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the shell was not started: %v", err)
	}
	defer tty.Close()
	if req.Stdin != nil {
		go func() { _, _ = io.Copy(tty, req.Stdin) }()
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		if req.Stdout != nil {
			_, _ = io.Copy(req.Stdout, tty)
		} else {
			_, _ = io.Copy(io.Discard, tty)
		}
	}()
	stopResize, resized := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(resized)
		for req.Resize != nil {
			select {
			case ws, ok := <-req.Resize:
				if !ok {
					return
				}
				_ = pty.Setsize(tty, &pty.Winsize{Rows: ws.Rows, Cols: ws.Cols})
			case <-stopResize:
				return
			}
		}
	}()
	// Resizing stops before the terminal closes, so it never touches a closed one.
	defer func() { close(stopResize); <-resized }()
	werr := cmd.Wait()
	// The output still buffered on the terminal is read before it closes.
	select {
	case <-copied:
	case <-time.After(time.Second):
	}
	return result(ctx, cmd, werr)
}
