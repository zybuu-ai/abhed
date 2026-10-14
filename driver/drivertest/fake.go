// Package drivertest provides an in-memory driver for tests of code built on
// package driver. It confines nothing and must never be registered in a
// server.
package drivertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// Fake is an in-memory driver. It implements every optional interface but
// declares only the capabilities it was built with.
type Fake struct {
	info driver.Info

	// ExecFunc, when set, runs each Exec. The default writes the command to
	// stdout, copies stdin to stdout after it, and exits 0.
	ExecFunc func(ctx context.Context, id string, req driver.ExecRequest) (driver.ExecResult, error)
	// AttachFunc, when set, runs each Attach once the instance is found
	// running. The default echoes stdin to stdout and notes each resize.
	AttachFunc func(ctx context.Context, id string, req driver.AttachRequest) (driver.ExecResult, error)
	// Tier is the tier every instance reports; "process" when empty.
	Tier string
	// Caveat is what every instance reports its tier lacks, such as "no /proc".
	Caveat string
	// Errs, when set for a method name ("create", "start", "stop",
	// "delete", "status"), is what that method returns instead of acting.
	Errs map[string]error

	mu        sync.Mutex
	instances map[string]driver.Phase
	files     map[string][]byte
	snapshots map[string]driver.SnapshotRef
	calls     map[string]int
	specs     map[string]driver.Spec
	Resizes   []driver.WinSize
}

// Spec is the Spec the instance id was last created from.
func (f *Fake) Spec(id string) (driver.Spec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.specs[id]
	return s, ok
}

// New returns a fake named name declaring caps.
func New(name string, caps ...driver.Capability) *Fake {
	return &Fake{
		info:      driver.Info{Name: name, Version: "0.0.0-test", Capabilities: caps},
		instances: map[string]driver.Phase{},
		files:     map[string][]byte{},
		snapshots: map[string]driver.SnapshotRef{},
	}
}

func (f *Fake) Info() driver.Info { return f.info }

func (f *Fake) tier() string {
	if f.Tier == "" {
		return "process"
	}
	return f.Tier
}

func (f *Fake) status(id string, p driver.Phase) driver.Status {
	return driver.Status{ID: id, Phase: p, Tier: f.tier(), Backend: "fake", Caveat: f.Caveat}
}

// SetPhase puts an instance in phase p, as if it had exited or failed.
func (f *Fake) SetPhase(id string, p driver.Phase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[id] = p
}

// SetTier changes the tier instances report from now on.
func (f *Fake) SetTier(tier string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Tier = tier
}

// SetErr makes method return err until it is set to nil.
func (f *Fake) SetErr(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Errs == nil {
		f.Errs = map[string]error{}
	}
	f.Errs[method] = err
}

// Calls counts the calls made of each method.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// enter counts a call and returns the error set for it, under f.mu.
func (f *Fake) enter(method string) error {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	return f.Errs[method]
}

func (f *Fake) Create(_ context.Context, spec driver.Spec) (driver.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create"); err != nil {
		return driver.Status{}, err
	}
	if spec.ID == "" {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "spec has no id")
	}
	if _, ok := f.instances[spec.ID]; ok {
		return driver.Status{}, driver.Errorf(driver.ErrExists, "instance %s exists", spec.ID)
	}
	f.instances[spec.ID] = driver.PhaseRunning
	if f.specs == nil {
		f.specs = map[string]driver.Spec{}
	}
	f.specs[spec.ID] = spec
	return f.status(spec.ID, driver.PhaseRunning), nil
}

func (f *Fake) set(method, id string, p driver.Phase) (driver.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(method); err != nil {
		return driver.Status{}, err
	}
	if _, ok := f.instances[id]; !ok {
		return driver.Status{}, driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	f.instances[id] = p
	return f.status(id, p), nil
}

func (f *Fake) Start(_ context.Context, id string) (driver.Status, error) {
	return f.set("start", id, driver.PhaseRunning)
}

func (f *Fake) Stop(_ context.Context, id string, _ time.Duration) (driver.Status, error) {
	return f.set("stop", id, driver.PhaseStopped)
}

func (f *Fake) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("delete"); err != nil {
		return err
	}
	if _, ok := f.instances[id]; !ok {
		return driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	delete(f.instances, id)
	return nil
}

func (f *Fake) Status(_ context.Context, id string) (driver.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("status"); err != nil {
		return driver.Status{}, err
	}
	p, ok := f.instances[id]
	if !ok {
		return driver.Status{}, driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	return f.status(id, p), nil
}

func (f *Fake) running(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.instances[id]
	if !ok {
		return driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	if p != driver.PhaseRunning {
		return driver.Errorf(driver.ErrConflict, "instance %s is %s", id, p)
	}
	return nil
}

func (f *Fake) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.ExecResult, error) {
	f.mu.Lock()
	err := f.enter("exec")
	f.mu.Unlock()
	if err != nil {
		return driver.ExecResult{}, err
	}
	if err := req.Validate(); err != nil {
		return driver.ExecResult{}, err
	}
	if err := f.running(id); err != nil {
		return driver.ExecResult{}, err
	}
	if f.ExecFunc != nil {
		return f.ExecFunc(ctx, id, req)
	}
	cmd := req.Command
	if cmd == "" {
		cmd = strings.Join(req.Argv, " ")
	}
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, cmd+"\n")
		if req.Stdin != nil {
			_, _ = io.Copy(req.Stdout, req.Stdin)
		}
	}
	return driver.ExecResult{}, nil
}

func (f *Fake) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.ExecResult, error) {
	if err := f.running(id); err != nil {
		return driver.ExecResult{}, err
	}
	if f.AttachFunc != nil {
		return f.AttachFunc(ctx, id, req)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if req.Stdin != nil && req.Stdout != nil {
			_, _ = io.Copy(req.Stdout, req.Stdin)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return driver.ExecResult{}, ctx.Err()
		case ws, ok := <-req.Resize:
			if !ok {
				req.Resize = nil
				continue
			}
			f.mu.Lock()
			f.Resizes = append(f.Resizes, ws)
			f.mu.Unlock()
		case <-done:
			// select picks at random, so take resizes already sent before returning.
			for req.Resize != nil {
				select {
				case ws, ok := <-req.Resize:
					if !ok {
						req.Resize = nil
						continue
					}
					f.mu.Lock()
					f.Resizes = append(f.Resizes, ws)
					f.mu.Unlock()
				default:
					req.Resize = nil
				}
			}
			return driver.ExecResult{}, nil
		}
	}
}

func (f *Fake) CopyIn(_ context.Context, id, dst string, tar io.Reader) error {
	if err := f.running(id); err != nil {
		return err
	}
	b, err := io.ReadAll(tar)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.files[id+":"+dst] = b
	f.mu.Unlock()
	return nil
}

func (f *Fake) CopyOut(_ context.Context, id, src string, tar io.Writer) error {
	if err := f.running(id); err != nil {
		return err
	}
	f.mu.Lock()
	b, ok := f.files[id+":"+src]
	f.mu.Unlock()
	if !ok {
		return driver.Errorf(driver.ErrNotFound, "no %s in %s", src, id)
	}
	_, err := io.Copy(tar, bytes.NewReader(b))
	return err
}

// Forward returns one end of a pipe whose other end echoes what it reads.
func (f *Fake) Forward(_ context.Context, id string, _ int) (io.ReadWriteCloser, error) {
	if err := f.running(id); err != nil {
		return nil, err
	}
	a, b := net.Pipe()
	go func() {
		defer func() { _ = b.Close() }()
		_, _ = io.Copy(b, b)
	}()
	return a, nil
}

func (f *Fake) Snapshot(_ context.Context, id, name string) (driver.SnapshotRef, error) {
	if _, err := f.Status(context.Background(), id); err != nil {
		return driver.SnapshotRef{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256([]byte(id + "@" + name))
	ref := driver.SnapshotRef{ID: id + "@" + name, Name: name, Digest: "sha256:" + hex.EncodeToString(sum[:])}
	f.snapshots[ref.ID] = ref
	return ref, nil
}

func (f *Fake) Restore(_ context.Context, id string, ref driver.SnapshotRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.instances[id]; !ok {
		return driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	if _, ok := f.snapshots[ref.ID]; !ok {
		return driver.Errorf(driver.ErrNotFound, "no snapshot %s", ref.ID)
	}
	return nil
}

// Command runs req with /bin/sh in the instance's workspace, unconfined; a
// shell is sh -i. It is for tests of code that runs commands it is handed.
func (f *Fake) Command(ctx context.Context, id string, req driver.CommandRequest) (*exec.Cmd, error) {
	f.mu.Lock()
	err := f.enter("command")
	spec := f.specs[id]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := f.running(id); err != nil {
		return nil, err
	}
	dir := req.Cwd
	if dir == "" {
		dir = spec.Workspace
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(spec.Workspace, dir)
	}
	if rel, err := filepath.Rel(spec.Workspace, dir); spec.Workspace == "" || err != nil || !filepath.IsLocal(rel) && rel != "." {
		return nil, driver.Errorf(driver.ErrInvalid, "cwd %s is outside the workspace", dir)
	}
	args := []string{"-c", req.Command}
	if req.Shell {
		args = []string{"-i"}
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + spec.Workspace, "ABHED_SANDBOX=" + id}
	return cmd, nil
}

// ForwardEnv leaves cmd alone: its environment already reaches the shell.
func (f *Fake) ForwardEnv(*exec.Cmd, []string) {}
