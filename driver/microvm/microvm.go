// Package microvm is a driver that runs each sandbox in its own lightweight
// virtual machine with its own guest kernel: Firecracker under its jailer on
// Linux with KVM, or a Virtualization.framework helper on macOS. The VM has
// no network device; the host reaches the guest only over vsock, where
// cmd/abhed-guest runs commands and reports their exit codes.
//
// This is hardware virtualization. It is not the "vm" tier of the process
// driver, which is gVisor, a user-space kernel; instances here report the
// tier "microvm".
//
// Everything a backend needs that is missing makes the driver unavailable:
// it never falls back to a weaker boundary.
package microvm

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/microvm/guest"
)

// Name is the driver's registered name.
const Name = "microvm"

// Tier is the isolation every instance reports.
const Tier = "microvm"

// GuestInit is where the guest agent is expected in the root filesystem.
const GuestInit = "/sbin/abhed-guest"

// Options are the operator's settings. Clients choose none of them; a
// sandbox's Spec.Config may only ask for fewer resources than the maxima.
type Options struct {
	// Kernel is an uncompressed guest kernel (vmlinux on x86_64, Image on
	// arm64) and Rootfs an ext4 root filesystem holding GuestInit and
	// /bin/sh. Both are absolute paths, read only by the VM.
	Kernel string `json:"kernel"`
	Rootfs string `json:"rootfs"`
	// BootArgs replaces the default kernel command line.
	BootArgs string `json:"boot_args,omitempty"`
	// StateDir holds each instance's workspace image. Only the driver's
	// user may write to it.
	StateDir string `json:"state_dir"`

	VCPUs        int `json:"vcpus,omitempty"`
	MaxVCPUs     int `json:"max_vcpus,omitempty"`
	MemoryMiB    int `json:"memory_mib,omitempty"`
	MaxMemoryMiB int `json:"max_memory_mib,omitempty"`
	// WorkspaceMiB is the size of the ext4 image the workspace is copied into.
	WorkspaceMiB int `json:"workspace_mib,omitempty"`
	// MkfsExt4 builds that image; "mkfs.ext4" from PATH when empty.
	MkfsExt4 string `json:"mkfs_ext4,omitempty"`
	// ImageUser, when set, is who builds workspace images, so a driver
	// running as root copies only what that user can read. A root driver
	// refuses a workspace without it.
	ImageUser *Credential `json:"image_user,omitempty"`
	// BootTimeoutSeconds bounds how long the guest agent may take to answer.
	BootTimeoutSeconds int `json:"boot_timeout_seconds,omitempty"`

	// Exactly one backend is set.
	Firecracker *Firecracker `json:"firecracker,omitempty"`
	Helper      *Helper      `json:"helper,omitempty"`
}

func (o *Options) defaults() {
	if o.VCPUs == 0 {
		o.VCPUs = 1
	}
	if o.MaxVCPUs == 0 {
		o.MaxVCPUs = max(o.VCPUs, 4)
	}
	if o.MemoryMiB == 0 {
		o.MemoryMiB = 512
	}
	if o.MaxMemoryMiB == 0 {
		o.MaxMemoryMiB = max(o.MemoryMiB, 4096)
	}
	if o.WorkspaceMiB == 0 {
		o.WorkspaceMiB = 2048
	}
	if o.MkfsExt4 == "" {
		o.MkfsExt4 = "mkfs.ext4"
	}
	if o.BootTimeoutSeconds == 0 {
		o.BootTimeoutSeconds = 30
	}
}

// Credential is a user and group id.
type Credential struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// backend boots machines; firecracker and the macOS helper are the two.
type backend interface {
	name() string
	// check reports why the backend cannot run here, or nil.
	check() error
	boot(ctx context.Context, b bootSpec) (machine, error)
}

// bootSpec is one boot of one instance.
type bootSpec struct {
	id        string // the sandbox id
	vmID      string // the id as the VMM accepts it
	slot      int
	vcpus     int
	memMiB    int
	kernel    string
	rootfs    string
	workspace string // the ext4 image, or empty
	bootArgs  string
	stateDir  string
}

// machine is a booted VM.
type machine interface {
	// dial opens a connection to the guest agent's port.
	dial(ctx context.Context) (net.Conn, error)
	// done is closed when the VMM process has exited.
	done() <-chan struct{}
	// kill ends the VMM and everything in its cgroup, and releases what the
	// boot set up. It is safe to call more than once.
	kill() error
	// console is the end of the guest's console output, for diagnosis.
	console() string
}

// Driver runs instances as microVMs.
type Driver struct {
	opts Options
	be   backend

	mu    sync.Mutex
	insts map[string]*instance
	slots map[int]string
}

type instance struct {
	spec   driver.Spec
	slot   int
	vcpus  int
	memMiB int
	image  string
	m      machine
	// stopped is set by Stop; a machine that ends without it has failed.
	stopped bool
	// starting is set while a boot is in flight; one boot at a time.
	starting bool
	runCtx   context.Context
	stopRun  context.CancelFunc
	running  sync.WaitGroup
}

// New returns the driver for opts, with the backend opts names. It fails
// only on settings that can never work; a host that lacks KVM, the VMM or
// the helper is reported by Create, so the driver can still be listed.
func New(opts Options) (*Driver, error) {
	opts.defaults()
	be, err := newBackend(&opts)
	if err != nil {
		return nil, err
	}
	return newDriver(opts, be), nil
}

func newBackend(o *Options) (backend, error) {
	switch {
	case o.Firecracker != nil && o.Helper != nil:
		return nil, driver.Errorf(driver.ErrInvalid, "microvm: set one backend, not both")
	case o.Firecracker != nil:
		return newFirecracker(o)
	case o.Helper != nil:
		return newHelper(o)
	}
	return nil, driver.Errorf(driver.ErrInvalid, "microvm: no backend is configured (firecracker on Linux, helper on macOS)")
}

func newDriver(opts Options, be backend) *Driver {
	return &Driver{opts: opts, be: be, insts: map[string]*instance{}, slots: map[int]string{}}
}

func (d *Driver) Info() driver.Info {
	return driver.Info{Name: Name, Version: "1", Capabilities: []driver.Capability{driver.CapAttach}}
}

// knownTiers are the names Spec.MinTier may use. A microVM meets each of
// them; an unknown name is refused rather than read as the weakest.
var knownTiers = map[string]bool{"none": true, "process": true, "fence": true, "container": true, "vm": true, Tier: true}

// resources reads the sizes a spec asks for, within the operator's maxima.
func (d *Driver) resources(cfg map[string]string) (vcpus, mem int, err error) {
	vcpus, mem = d.opts.VCPUs, d.opts.MemoryMiB
	for k, v := range cfg {
		n, perr := strconv.Atoi(v)
		switch {
		case k != "vcpus" && k != "memory_mib":
			return 0, 0, driver.Errorf(driver.ErrInvalid, "microvm: unknown config key %q", k)
		case perr != nil || n < 1:
			return 0, 0, driver.Errorf(driver.ErrInvalid, "microvm: %s must be a positive integer", k)
		case k == "vcpus" && n > d.opts.MaxVCPUs:
			return 0, 0, driver.Errorf(driver.ErrInvalid, "microvm: vcpus %d is over the maximum %d", n, d.opts.MaxVCPUs)
		case k == "memory_mib" && (n > d.opts.MaxMemoryMiB || n < 128):
			return 0, 0, driver.Errorf(driver.ErrInvalid, "microvm: memory_mib must be 128 to %d", d.opts.MaxMemoryMiB)
		case k == "vcpus":
			vcpus = n
		default:
			mem = n
		}
	}
	return vcpus, mem, nil
}

func (d *Driver) Create(ctx context.Context, spec driver.Spec) (driver.Status, error) {
	if spec.ID == "" {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "spec has no id")
	}
	if _, err := vmID(spec.ID); err != nil {
		return driver.Status{}, err
	}
	if spec.MinTier != "" && !knownTiers[spec.MinTier] {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "unknown tier %q", spec.MinTier)
	}
	if spec.Workspace != "" && !filepath.IsAbs(spec.Workspace) {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "microvm: the workspace must be an absolute path")
	}
	vcpus, mem, err := d.resources(spec.Config)
	if err != nil {
		return driver.Status{}, err
	}
	if err := d.be.check(); err != nil {
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "microvm: %v", err)
	}

	d.mu.Lock()
	if _, ok := d.insts[spec.ID]; ok {
		d.mu.Unlock()
		return driver.Status{}, driver.Errorf(driver.ErrExists, "instance %s exists", spec.ID)
	}
	slot := d.freeSlot()
	if slot < 0 {
		d.mu.Unlock()
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "microvm: every instance slot is in use")
	}
	in := &instance{spec: spec, slot: slot, vcpus: vcpus, memMiB: mem, stopped: true}
	d.insts[spec.ID], d.slots[slot] = in, spec.ID
	d.mu.Unlock()

	err = d.prepare(ctx, in)
	if err == nil {
		_, err = d.Start(ctx, spec.ID)
	}
	if err != nil {
		d.forget(spec.ID, in)
		return driver.Status{}, err
	}
	return d.Status(ctx, spec.ID)
}

// freeSlot is the lowest slot not in use, or -1. Callers hold d.mu.
func (d *Driver) freeSlot() int {
	n := slotCount(d.opts)
	for i := 0; i < n; i++ {
		if _, used := d.slots[i]; !used {
			return i
		}
	}
	return -1
}

func slotCount(o Options) int {
	if o.Firecracker != nil {
		return o.Firecracker.Slots
	}
	return 64
}

// prepare builds the instance's workspace image.
func (d *Driver) prepare(ctx context.Context, in *instance) error {
	// The image user passes through these folders to build its image in a
	// folder of its own, but can list and read nothing in them.
	if err := os.MkdirAll(d.opts.StateDir, 0o711); err != nil {
		return driver.Errorf(driver.ErrInternal, "microvm: state folder: %v", err)
	}
	dir := filepath.Join(d.opts.StateDir, in.spec.ID)
	// Anything here is left from a driver that did not exit cleanly.
	_ = os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0o711); err != nil {
		return driver.Errorf(driver.ErrInternal, "microvm: state folder: %v", err)
	}
	if in.spec.Workspace == "" {
		return nil
	}
	if d.opts.ImageUser == nil && os.Geteuid() == 0 {
		return driver.Errorf(driver.ErrUnavailable, "microvm: a driver running as root copies a workspace only as the user asking for it; set image_user")
	}
	if fi, err := os.Stat(d.opts.StateDir); d.opts.ImageUser != nil && (err != nil || fi.Mode().Perm()&0o001 == 0) {
		return driver.Errorf(driver.ErrUnavailable, "microvm: state_dir %s must be mode 0711, so the image user can reach its build folder", d.opts.StateDir)
	}
	img, err := buildImage(ctx, d.opts.MkfsExt4, in.spec.Workspace, dir, d.opts.WorkspaceMiB, d.opts.ImageUser)
	if err != nil {
		return driver.Errorf(driver.ErrUnavailable, "microvm: %v", err)
	}
	in.image = img
	return nil
}

// forget removes an instance that failed to be created, and its state.
func (d *Driver) forget(id string, in *instance) {
	d.mu.Lock()
	if d.insts[id] == in {
		delete(d.insts, id)
		delete(d.slots, in.slot)
	}
	d.mu.Unlock()
	_ = os.RemoveAll(filepath.Join(d.opts.StateDir, id))
}

func (d *Driver) get(id string) (*instance, error) {
	in, ok := d.insts[id]
	if !ok {
		return nil, driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	return in, nil
}

func (d *Driver) status(id string, in *instance) driver.Status {
	st := driver.Status{ID: id, Tier: Tier, Backend: d.be.name(), Phase: driver.PhaseRunning}
	switch {
	case in.m == nil:
		st.Phase = driver.PhaseStopped
	case exited(in.m):
		// The VM ended without being asked to: a guest crash or poweroff.
		st.Phase = driver.PhaseFailed
		st.Message = "the VM exited; console: " + in.m.console()
	}
	return st
}

func exited(m machine) bool {
	select {
	case <-m.done():
		return true
	default:
		return false
	}
}

func (d *Driver) Status(_ context.Context, id string) (driver.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.get(id)
	if err != nil {
		return driver.Status{}, err
	}
	return d.status(id, in), nil
}

// Start boots a stopped instance and waits for its agent to answer.
func (d *Driver) Start(ctx context.Context, id string) (driver.Status, error) {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.Status{}, err
	}
	if in.m != nil && !exited(in.m) {
		st := d.status(id, in)
		d.mu.Unlock()
		return st, nil
	}
	if in.starting {
		d.mu.Unlock()
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s is already starting", id)
	}
	in.starting = true
	old := in.m
	in.m = nil
	b := bootSpec{
		id: id, slot: in.slot, vcpus: in.vcpus, memMiB: in.memMiB,
		kernel: d.opts.Kernel, rootfs: d.opts.Rootfs, workspace: in.image,
		bootArgs: d.opts.BootArgs, stateDir: filepath.Join(d.opts.StateDir, id),
	}
	b.vmID, _ = vmID(id)
	d.mu.Unlock()
	if old != nil {
		_ = old.kill()
	}

	m, err := d.be.boot(ctx, b)
	if err == nil {
		if err = d.waitReady(ctx, m); err != nil {
			_ = m.kill()
		}
	} else {
		err = driver.Errorf(driver.ErrUnavailable, "microvm: booting: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	in.starting = false
	if err != nil {
		return driver.Status{}, err
	}
	if cur, ok := d.insts[id]; !ok || cur != in || in.m != nil {
		_ = m.kill()
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s changed while starting", id)
	}
	in.m, in.stopped = m, false
	in.runCtx, in.stopRun = context.WithCancel(context.Background())
	return d.status(id, in), nil
}

// waitReady pings the guest agent until it answers, the VM exits, or the
// boot timeout passes.
func (d *Driver) waitReady(ctx context.Context, m machine) error {
	timeout := time.Duration(d.opts.BootTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		c, err := m.dial(ctx)
		if err == nil {
			pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
			err = guest.Ping(pctx, c)
			pcancel()
			if err == nil {
				return nil
			}
		}
		select {
		case <-m.done():
			return driver.Errorf(driver.ErrUnavailable, "microvm: the VM exited while booting; console: %s", m.console())
		case <-ctx.Done():
			return driver.Errorf(driver.ErrUnavailable, "microvm: the guest agent did not answer within %s: %v; console: %s", timeout, err, m.console())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop lets running commands finish within grace, asks the guest to power
// off, and kills the VM if it has not exited by then.
func (d *Driver) Stop(ctx context.Context, id string, grace time.Duration) (driver.Status, error) {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.Status{}, err
	}
	m := in.m
	if m == nil {
		st := d.status(id, in)
		d.mu.Unlock()
		return st, nil
	}
	in.stopped = true
	stopRun := in.stopRun
	d.mu.Unlock()

	deadline := time.Now().Add(grace)
	drained := make(chan struct{})
	go func() { in.running.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(grace):
	}
	stopRun()
	<-drained
	if left := time.Until(deadline); left > 0 && !exited(m) {
		sctx, cancel := context.WithTimeout(ctx, left)
		if c, err := m.dial(sctx); err == nil && guest.Shutdown(sctx, c) == nil {
			select {
			case <-m.done():
			case <-sctx.Done():
			}
		}
		cancel()
	}
	killErr := m.kill()

	d.mu.Lock()
	defer d.mu.Unlock()
	if in.m == m {
		in.m = nil
	}
	if killErr != nil {
		return driver.Status{}, driver.Errorf(driver.ErrInternal, "microvm: stopping the VM: %v", killErr)
	}
	return d.status(id, in), nil
}

// Delete stops the VM and removes the instance's workspace image.
func (d *Driver) Delete(ctx context.Context, id string) error {
	if _, err := d.Stop(ctx, id, 0); err != nil {
		return err
	}
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	if in.m != nil || in.starting {
		d.mu.Unlock()
		return driver.Errorf(driver.ErrConflict, "instance %s is being started", id)
	}
	delete(d.insts, id)
	delete(d.slots, in.slot)
	d.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(d.opts.StateDir, id)); err != nil {
		return driver.Errorf(driver.ErrInternal, "microvm: removing state: %v", err)
	}
	return nil
}

// Close deletes every instance, for a driver process that is exiting:
// nothing else would track the VMs it leaves.
func (d *Driver) Close(ctx context.Context) error {
	d.mu.Lock()
	ids := make([]string, 0, len(d.insts))
	for id := range d.insts {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := d.Delete(ctx, id); err != nil && !errors.Is(err, driver.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// begin admits one call to a running instance; done must be called after.
func (d *Driver) begin(ctx context.Context, id string) (machine, context.Context, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.get(id)
	if err != nil {
		return nil, nil, nil, err
	}
	if in.stopped || in.m == nil {
		return nil, nil, nil, driver.Errorf(driver.ErrConflict, "instance %s is stopped", id)
	}
	if exited(in.m) {
		return nil, nil, nil, driver.Errorf(driver.ErrConflict, "instance %s has failed: its VM exited", id)
	}
	in.running.Add(1)
	cctx, cancel := context.WithCancel(ctx)
	unhook := context.AfterFunc(in.runCtx, cancel)
	return in.m, cctx, func() { unhook(); cancel(); in.running.Done() }, nil
}

func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.ExecResult, error) {
	if err := req.Validate(); err != nil {
		return driver.ExecResult{}, err
	}
	return d.run(ctx, id, guest.ExecRequest{Argv: req.Argv, Command: req.Command, Cwd: req.Cwd, Env: req.Env},
		guest.Streams{Stdin: req.Stdin, Stdout: req.Stdout, Stderr: req.Stderr})
}

// Attach runs the guest's shell on a terminal.
func (d *Driver) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.ExecResult, error) {
	var resize chan guest.Size
	if req.Resize != nil {
		resize = make(chan guest.Size)
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case ws, ok := <-req.Resize:
					if !ok {
						return
					}
					select {
					case resize <- guest.Size{Rows: ws.Rows, Cols: ws.Cols}:
					case <-stop:
						return
					}
				case <-stop:
					return
				}
			}
		}()
	}
	return d.run(ctx, id, guest.ExecRequest{TTY: true, Cwd: req.Cwd, Rows: req.Size.Rows, Cols: req.Size.Cols},
		guest.Streams{Stdin: req.Stdin, Stdout: req.Stdout, Resize: resize})
}

func (d *Driver) run(ctx context.Context, id string, req guest.ExecRequest, s guest.Streams) (driver.ExecResult, error) {
	m, cctx, done, err := d.begin(ctx, id)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer done()
	c, err := m.dial(cctx)
	if err != nil {
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "microvm: reaching the guest: %v", err)
	}
	code, err := guest.Exec(cctx, c, req, s)
	if err != nil {
		if ctx.Err() != nil {
			return driver.ExecResult{}, ctx.Err()
		}
		if ge := (*guest.Error)(nil); errors.As(err, &ge) {
			return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "microvm: the guest did not run the command: %s", ge.Message)
		}
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "microvm: the command did not report an exit: %v", err)
	}
	return driver.ExecResult{ExitCode: code}, nil
}

// vmID maps a sandbox id to one the jailer accepts (letters, digits and
// hyphens, at most 64). Ids may not hold '-' themselves, so two ids never
// map to one VM.
func vmID(id string) (string, error) {
	if len(id) == 0 || len(id) > 64 {
		return "", driver.Errorf(driver.ErrInvalid, "microvm: id %q must be 1 to 64 characters", id)
	}
	b := []byte(id)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '_':
			b[i] = '-'
		default:
			return "", driver.Errorf(driver.ErrInvalid, "microvm: id %q may hold only lowercase letters, digits and '_'", id)
		}
	}
	return string(b), nil
}
