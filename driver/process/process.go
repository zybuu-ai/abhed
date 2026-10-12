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
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/forward"
	"github.com/zybuu-ai/abhed/driver/snapshot"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
)

// Name is the driver's registered name.
const Name = "process"

// Options are the operator's settings for the process driver.
type Options struct {
	// SnapshotDir holds workspace snapshots, outside every workspace; the
	// driver declares snapshot only when it is set.
	SnapshotDir string
	// Record receives snapshot, restore and forward events.
	Record driver.Recorder
	// Hide are folders no instance's commands may read or write, apart from
	// the instance's own workspace inside one: where sibling sandboxes live.
	Hide []string
	// StateWorkspace is serve's workspace: state and secret paths are
	// resolved against it, and its .abhed is denied to every instance.
	// Empty is each instance's own folder.
	StateWorkspace string
	// sizer holds sized instances to their size; nil is the host's.
	sizer sizing
}

// Driver runs instances under the configured sandbox tier.
type Driver struct {
	build func(spec driver.Spec) (sandbox.Sandbox, error)
	opts  Options
	snaps snapshot.Store

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
	// forwards are the open forwards of this incarnation; done closes when
	// it stops.
	forwards *forward.Set
	done     chan struct{}
	// restoring holds off a start while the workspace is replaced.
	restoring bool
	// box holds every command to the size the spec asked for; nil when it
	// asked for none.
	box box
}

// begun starts a new incarnation's run context, forwards and done signal.
func (in *instance) begun() {
	in.runCtx, in.stopRun = context.WithCancel(context.Background())
	in.forwards, in.done = &forward.Set{}, make(chan struct{})
}

// New returns the driver building sandboxes from cfg, as a session does.
func New(cfg config.Config) *Driver {
	return NewWithOptions(cfg, Options{})
}

// NewWithOptions is New with the operator's options.
func NewWithOptions(cfg config.Config, opts Options) *Driver {
	return newDriverWith(func(spec driver.Spec) (sandbox.Sandbox, error) {
		p, err := sandboxconfig.InstancePolicy(cfg, spec.Workspace, opts.StateWorkspace)
		if err != nil {
			return nil, err
		}
		p.Hidden = append(p.Hidden, opts.Hide...)
		return sandbox.Select(p)
	}, opts)
}

func newDriverWith(build func(driver.Spec) (sandbox.Sandbox, error), opts Options) *Driver {
	return &Driver{build: build, opts: opts, snaps: snapshot.Store{Dir: opts.SnapshotDir}, insts: map[string]*instance{}}
}

func (d *Driver) Info() driver.Info {
	caps := []driver.Capability{driver.CapAttach, driver.CapForward, driver.CapCommand}
	if d.opts.SnapshotDir != "" {
		caps = append(caps, driver.CapSnapshot)
	}
	return driver.Info{Name: Name, Version: "1", Capabilities: caps}
}

// tiers are the names Spec.MinTier may use; anything else is refused, since
// an unknown name would otherwise compare as the weakest.
var tiers = map[string]sandbox.Tier{
	string(sandbox.TierNone): sandbox.TierNone, string(sandbox.TierProcess): sandbox.TierProcess,
	string(sandbox.TierFence): sandbox.TierFence, string(sandbox.TierContainer): sandbox.TierContainer,
	string(sandbox.TierVM): sandbox.TierVM,
}

// microVMTier is the tier only the microvm driver provides: known, so not
// invalid, but never met here.
const microVMTier = "microvm"

func status(id string, in *instance) driver.Status {
	st := driver.Status{ID: id, Phase: driver.PhaseRunning}
	if in.stopped {
		st.Phase = driver.PhaseStopped
	}
	if in.sb != nil {
		st.Tier, st.Message, st.Caveat = string(in.sb.Tier()), in.sb.Describe(), sandbox.Caveat(in.sb)
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
	if spec.MinTier == microVMTier {
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "the process driver cannot provide the %s tier; use the microvm driver", microVMTier)
	}
	if _, ok := tiers[spec.MinTier]; spec.MinTier != "" && !ok {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "unknown tier %q", spec.MinTier)
	}
	// A size is held by a cgroup per instance where there are cgroups; any
	// other setting, or a size where none can be made, is refused rather
	// than run without it.
	sz, err := parseSize(spec.Config)
	if err != nil {
		return driver.Status{}, err
	}
	if !sz.zero() {
		if err := d.sizer().Check(); err != nil {
			return driver.Status{}, err
		}
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
	var bx box
	if err == nil && !sz.zero() {
		if bx, err = d.sizedBox(spec.ID, sb, sz); err != nil {
			_ = sandbox.Close(sb)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.insts[spec.ID] != in {
		if err == nil {
			_ = sandbox.Close(sb)
			if bx != nil {
				_ = bx.Remove(context.Background())
			}
		}
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s was deleted while it was created", spec.ID)
	}
	if err != nil {
		delete(d.insts, spec.ID)
		return driver.Status{}, err
	}
	in.sb, in.stopped, in.box = sb, false, bx
	in.begun()
	return status(spec.ID, in), nil
}

// sizedBox makes the box holding instance id to sz, on a tier whose
// commands stay where it puts them.
func (d *Driver) sizedBox(id string, sb sandbox.Sandbox, sz size) (box, error) {
	if !sizedTier(sb.Tier()) {
		return nil, driver.Errorf(driver.ErrInvalid, "the process driver sizes only the process tier, and the sandbox here is %s; "+
			"use the container or microvm driver for a sized sandbox", sb.Tier())
	}
	bx, err := d.sizer().NewBox(id, sz)
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "making the sandbox's cgroup: %v", err)
	}
	if err := nestIn(bx, sb); err != nil {
		_ = bx.Remove(context.Background())
		return nil, err
	}
	return bx, nil
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
	if in.restoring {
		d.mu.Unlock()
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s is being restored", id)
	}
	spec := in.spec
	d.mu.Unlock()

	sb, err := d.open(spec)
	if err != nil {
		return driver.Status{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur, ok := d.insts[id]; !ok || cur != in || !in.stopped || in.sb != nil || in.restoring {
		_ = sandbox.Close(sb)
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s changed while starting", id)
	}
	if in.box != nil && !sizedTier(sb.Tier()) {
		_ = sandbox.Close(sb)
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "the sandbox here is now %s, which the instance's size cannot hold", sb.Tier())
	}
	if in.box != nil {
		if err := nestIn(in.box, sb); err != nil {
			_ = sandbox.Close(sb)
			return driver.Status{}, err
		}
	}
	// Starting again is a new incarnation, as the control plane counts it.
	in.spec.Generation++
	in.sb, in.stopped = sb, false
	in.begun()
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
	sb, stopRun, fw, done := in.sb, in.stopRun, in.forwards, in.done
	d.mu.Unlock()
	// What is bound to this incarnation ends first, and does not wait out the grace.
	close(done)
	fw.Close()

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
	in := d.insts[id]
	delete(d.insts, id)
	if in.box != nil {
		if err := in.box.Remove(ctx); err != nil {
			return driver.Errorf(driver.ErrInternal, "removing the sandbox's cgroup: %v", err)
		}
	}
	if d.opts.SnapshotDir != "" {
		// A sandbox's snapshots restore only into it, so they go with it.
		return d.snaps.RemoveAll(id)
	}
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
	for i, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			// By position only: a bare entry may be a secret's value.
			return driver.Errorf(driver.ErrInvalid, "env entry %d is not NAME=value", i+1)
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
	line, err := req.Line()
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
	release, err := placeOn(in.box, cmd)
	if err != nil {
		sandbox.Release(cmd)
		return driver.ExecResult{}, err
	}
	defer release()
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

// Command builds a command, or the interactive shell, under the instance's
// sandbox for the caller to run. It counts as running until ctx ends, and
// ends when the instance stops.
func (d *Driver) Command(ctx context.Context, id string, req driver.CommandRequest) (*exec.Cmd, error) {
	in, cctx, done, err := d.begin(ctx, id)
	if err != nil {
		return nil, err
	}
	dir, err := cwd(in.spec.Workspace, req.Cwd)
	if err != nil {
		done()
		return nil, err
	}
	var cmd *exec.Cmd
	if req.Shell {
		sh, ok := in.sb.(sandbox.Interactive)
		if !ok {
			done()
			return nil, driver.Errorf(driver.ErrUnsupported, "the %s tier here has no interactive shell", in.sb.Tier())
		}
		cmd = sh.Shell(cctx, dir)
	} else {
		cmd = in.sb.Command(cctx, dir, req.Command)
	}
	release, err := placeOn(in.box, cmd)
	if err != nil {
		sandbox.Release(cmd)
		done()
		return nil, err
	}
	context.AfterFunc(cctx, func() { release(); done() })
	return cmd, nil
}

// ForwardEnv passes named variables into a container-tier command.
func (d *Driver) ForwardEnv(cmd *exec.Cmd, names []string) { sandbox.ForwardEnv(cmd, names) }

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
	if err := addEnv(cmd, req.Env); err != nil {
		sandbox.Release(cmd)
		return driver.ExecResult{}, err
	}
	release, err := placeOn(in.box, cmd)
	if err != nil {
		sandbox.Release(cmd)
		return driver.ExecResult{}, err
	}
	defer release()
	winSize := &pty.Winsize{Rows: req.Size.Rows, Cols: req.Size.Cols}
	if winSize.Rows == 0 || winSize.Cols == 0 {
		winSize = &pty.Winsize{Rows: 30, Cols: 100}
	}
	tty, err := pty.StartWithSize(cmd, winSize)
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

// record appends an event, or warns when the driver has no record.
func (d *Driver) record(event string, payload map[string]any) error {
	if d.opts.Record == nil {
		slog.Warn("sandbox event not recorded: the process driver has no record", "event", event, "sandbox_id", payload["sandbox_id"])
		return nil
	}
	return d.opts.Record(event, payload)
}

// Done is closed when the instance's current incarnation stops.
func (d *Driver) Done(id string) (<-chan struct{}, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.get(id)
	if err != nil {
		return nil, err
	}
	if in.stopped {
		return nil, driver.Errorf(driver.ErrConflict, "instance %s is stopped", id)
	}
	return in.done, nil
}

// Forward runs a relay inside the sandbox that connects to port on the
// sandbox's own loopback, so it reaches only what the sandbox could. The
// stream lives until it is closed or the instance stops; ctx bounds only
// the connecting.
func (d *Driver) Forward(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if port < 1 || port > 65535 {
		return nil, driver.Errorf(driver.ErrInvalid, "port %d is out of range", port)
	}
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	if in.stopped || in.sb == nil {
		d.mu.Unlock()
		return nil, driver.Errorf(driver.ErrConflict, "instance %s is stopped", id)
	}
	sb, gen, fw, runCtx, bx := in.sb, in.spec.Generation, in.forwards, in.runCtx, in.box
	in.running.Add(1)
	d.mu.Unlock()

	fctx, cancel := context.WithCancel(runCtx)
	cmd := sb.Command(fctx, in.spec.Workspace, forward.RelayLine(port))
	if cmd.Err != nil {
		cancel()
		sandbox.Release(cmd)
		in.running.Done()
		return nil, driver.Errorf(driver.ErrUnavailable, "the relay was not started: %v", cmd.Err)
	}
	sandbox.EndWithCommand(cmd)
	release, err := placeOn(bx, cmd)
	if err != nil {
		cancel()
		sandbox.Release(cmd)
		in.running.Done()
		return nil, err
	}
	base := map[string]any{"sandbox_id": id, "generation": gen, "port": port}
	if err := d.record(driver.EventForward, with(base, "action", "open")); err != nil {
		cancel()
		release()
		sandbox.Release(cmd)
		in.running.Done()
		return nil, driver.Errorf(driver.ErrUnavailable, "recording the forward: %v", err)
	}
	var st *forward.Stream
	st, err = forward.Open(ctx, cmd, port, cancel, func(bin, bout int64) {
		cancel()
		release()
		if st != nil {
			fw.Remove(st)
		}
		in.running.Done()
		_ = d.record(driver.EventForward, with(base, "action", "close", "bytes_in", bin, "bytes_out", bout))
	})
	if err != nil {
		release()
		sandbox.Release(cmd)
		return nil, err
	}
	if err := fw.Add(st); err != nil {
		return nil, err
	}
	return st, nil
}

// with is base and the pairs kv added.
func with(base map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// Snapshot archives the instance's workspace, without Abhed's state, and
// records it. A snapshot of a running instance is taken while commands may
// write; stop it first for a consistent one.
func (d *Driver) Snapshot(ctx context.Context, id, name string) (driver.SnapshotRef, error) {
	if d.opts.SnapshotDir == "" {
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnsupported, "the process driver has no snapshot folder")
	}
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.SnapshotRef{}, err
	}
	if in.restoring {
		d.mu.Unlock()
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrConflict, "instance %s is being restored", id)
	}
	ws, gen := in.spec.Workspace, in.spec.Generation
	d.mu.Unlock()
	ref, st, err := d.snaps.Take(ctx, id, gen, name, ws)
	if err != nil {
		return driver.SnapshotRef{}, err
	}
	if err := d.record(driver.EventSnapshot, snapshot.Payload(id, gen, ref, st)); err != nil {
		// A snapshot the record does not hold is not kept.
		_ = d.snaps.Remove(id, ref)
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnavailable, "recording the snapshot: %v", err)
	}
	return ref, nil
}

// Restore replaces a stopped instance's workspace with the snapshot, once
// its digest matches; Abhed's state is left as it is. The next start is a
// new generation.
func (d *Driver) Restore(ctx context.Context, id string, ref driver.SnapshotRef) error {
	if d.opts.SnapshotDir == "" {
		return driver.Errorf(driver.ErrUnsupported, "the process driver has no snapshot folder")
	}
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	if !in.stopped || in.sb != nil || in.restoring {
		d.mu.Unlock()
		return driver.Errorf(driver.ErrConflict, "instance %s must be stopped to restore it", id)
	}
	in.restoring = true
	ws, gen := in.spec.Workspace, in.spec.Generation
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		in.restoring = false
		d.mu.Unlock()
	}()
	st, err := d.snaps.Restore(ctx, id, ref, ws)
	payload := snapshot.Payload(id, gen, ref, st)
	if err != nil {
		payload["outcome"], payload["error"] = "refused", err.Error()
		_ = d.record(driver.EventRestore, payload)
		return err
	}
	payload["outcome"] = "restored"
	if err := d.record(driver.EventRestore, payload); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "the workspace was restored but the record refused it: %v", err)
	}
	return nil
}
