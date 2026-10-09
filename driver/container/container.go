// Package container is the docker and podman drivers: each sandbox is one
// long-lived container, started from an image the operator allows, with no
// capabilities, no new privileges, a read-only root filesystem, the engine's
// default seccomp profile, a non-root user, bounded memory, processes and
// CPU, and no network. Under sandbox.network allowlist the egress proxy stays
// on the host and the container reaches it only through a relay on its own
// loopback. The engines are driven through their CLIs, as the container tier
// is; that tier, one container per command, is unchanged.
package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"mvdan.cc/sh/v3/syntax"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/forward"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
)

// The registered names.
const (
	Docker = "docker"
	Podman = "podman"
)

// engineWait bounds a call to the engine that is not a command's own, such
// as a stop, a removal or a probe.
const engineWait = 60 * time.Second

// Driver runs each sandbox as one container on a docker or podman engine.
type Driver struct {
	name string
	opts Options
	r    runner
	// policy is the sandbox policy for a workspace, as a session's is built.
	policy func(workspace string) (sandbox.Policy, error)

	probeMu sync.Mutex
	eng     engineInfo
	probed  bool

	mu    sync.Mutex
	insts map[string]*instance
}

type instance struct {
	spec   driver.Spec
	name   string
	pol    sandbox.Policy
	req    request
	user   string
	uid    int
	gid    int
	eng    engineInfo
	git    *sandbox.ContainerMounts
	mounts []string // the sandbox's mount arguments the container was started with
	egress *egressState

	// life serialises start, stop and delete of the instance.
	life    sync.Mutex
	stopped bool
	// running counts commands in flight; a stop waits for them up to its grace.
	running sync.WaitGroup
	// forwards are this incarnation's open forwards; done closes when it stops.
	forwards *forward.Set
	done     chan struct{}
}

// NewDocker returns the docker driver building each sandbox's policy from cfg.
func NewDocker(cfg config.Config, opts Options) (*Driver, error) {
	return newFromConfig(Docker, cfg, opts)
}

// NewPodman returns the podman driver building each sandbox's policy from cfg.
func NewPodman(cfg config.Config, opts Options) (*Driver, error) {
	return newFromConfig(Podman, cfg, opts)
}

func newFromConfig(name string, cfg config.Config, opts Options) (*Driver, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	bin := opts.Binary
	if bin == "" {
		var err error
		if bin, err = exec.LookPath(name); err != nil {
			bin = name // probed, and refused, at the first Create
		}
	}
	return newDriver(name, opts, cli{path: bin}, func(ws string) (sandbox.Policy, error) {
		return sandboxconfig.Policy(cfg, ws)
	}), nil
}

func newDriver(name string, opts Options, r runner, policy func(string) (sandbox.Policy, error)) *Driver {
	return &Driver{name: name, opts: opts, r: r, policy: policy, insts: map[string]*instance{}}
}

func (d *Driver) Info() driver.Info {
	caps := []driver.Capability{driver.CapAttach, driver.CapCopy, driver.CapForward, driver.CapCommand}
	if d.opts.SnapshotDir != "" {
		caps = append(caps, driver.CapSnapshot)
	}
	return driver.Info{Name: d.name, Version: "1", Capabilities: caps}
}

// engine probes the engine once it answers; until then every call asks again.
func (d *Driver) engine(ctx context.Context) (engineInfo, error) {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if d.probed {
		return d.eng, nil
	}
	pctx, cancel := context.WithTimeout(ctx, engineWait)
	defer cancel()
	eng, err := probe(pctx, d.r)
	if err != nil {
		return engineInfo{}, driver.Errorf(driver.ErrUnavailable, "the %s engine is not answering: %v", d.name, err)
	}
	if d.name == Podman && !eng.podman {
		return engineInfo{}, driver.Errorf(driver.ErrUnavailable, "the podman driver's engine says it is not Podman")
	}
	d.eng, d.probed = eng, true
	return eng, nil
}

// Probe reports whether the engine answers as the engine this driver drives.
func (d *Driver) Probe(ctx context.Context) error {
	_, err := d.engine(ctx)
	return err
}

// tierNames are the tiers Spec.MinTier may name; anything else is refused,
// since an unknown name would otherwise compare as the weakest.
var tierNames = map[string]sandbox.Tier{
	"none": sandbox.TierNone, "process": sandbox.TierProcess, "fence": sandbox.TierFence,
	"container": sandbox.TierContainer, "vm": sandbox.TierVM,
}

// checkTier refuses a floor a container cannot meet.
func checkTier(min string) error {
	if min == "" {
		return nil
	}
	t, ok := tierNames[min]
	if !ok {
		return driver.Errorf(driver.ErrInvalid, "unknown tier %q", min)
	}
	if t.Strength() > sandbox.TierContainer.Strength() {
		return driver.Errorf(driver.ErrUnavailable, "a container is tier %s, weaker than the required %s", sandbox.TierContainer, min)
	}
	return nil
}

func (d *Driver) Create(ctx context.Context, spec driver.Spec) (driver.Status, error) {
	if !containerID.MatchString(spec.ID) {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "sandbox id %q cannot name a container", spec.ID)
	}
	if spec.Workspace == "" || !filepath.IsAbs(spec.Workspace) || filepath.Clean(spec.Workspace) != spec.Workspace {
		return driver.Status{}, driver.Errorf(driver.ErrInvalid, "the %s driver needs an absolute, clean workspace", d.name)
	}
	if err := safeArg(spec.Workspace); err != nil {
		return driver.Status{}, err
	}
	if err := checkTier(spec.MinTier); err != nil {
		return driver.Status{}, err
	}
	d.mu.Lock()
	if _, ok := d.insts[spec.ID]; ok {
		d.mu.Unlock()
		return driver.Status{}, driver.Errorf(driver.ErrExists, "instance %s exists", spec.ID)
	}
	// Reserved while it is built, so a second create of the id fails.
	in := &instance{spec: spec, name: d.opts.prefix() + spec.ID, stopped: true}
	in.life.Lock()
	defer in.life.Unlock()
	d.insts[spec.ID] = in
	d.mu.Unlock()

	st, err := d.build(ctx, in)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.insts[spec.ID] != in {
		if err == nil {
			_ = d.teardown(in)
		}
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s was deleted while it was created", spec.ID)
	}
	if err != nil {
		delete(d.insts, spec.ID)
		return driver.Status{}, err
	}
	in.stopped = false
	return st, nil
}

// build works out in's policy and admitted request, then starts it.
func (d *Driver) build(ctx context.Context, in *instance) (driver.Status, error) {
	eng, err := d.engine(ctx)
	if err != nil {
		return driver.Status{}, err
	}
	pol, err := d.policy(in.spec.Workspace)
	if err != nil {
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "%v", err)
	}
	if pol.Tier != "" {
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "sandbox.tier is %s, which the %s driver does not provide", pol.Tier, d.name)
	}
	if err := checkTier(string(pol.MinTier)); err != nil {
		return driver.Status{}, err
	}
	if pol.Egress != nil {
		if why := egressRefusal(eng); why != "" {
			return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "%s", why)
		}
	}
	req, err := d.opts.admit(in.spec, pol, denied(pol))
	if err != nil {
		return driver.Status{}, err
	}
	wsUID, wsGID := owner(pol.Workspace)
	in.pol, in.req = pol, req
	in.uid, in.gid = userFor(os.Getuid(), os.Getgid(), wsUID, wsGID, eng)
	in.user = strconv.Itoa(in.uid) + ":" + strconv.Itoa(in.gid)
	in.eng = eng
	in.git = &sandbox.ContainerMounts{}
	return d.start(ctx, in, eng)
}

// denied are the host paths of Abhed's state a mount may not reach.
func denied(pol sandbox.Policy) []string {
	return append([]string{filepath.Join(pol.Workspace, ".abhed")}, pol.StatePaths...)
}

// start runs in's container from fresh mounts and checks what is in force.
func (d *Driver) start(ctx context.Context, in *instance, eng engineInfo) (driver.Status, error) {
	mounts, err := in.git.Args(ctx, in.pol, d.name)
	if err != nil {
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "%v", err)
	}
	in.mounts = mounts
	if in.pol.Egress != nil {
		e, err := startEgress(in.spec.ID, in.pol.Egress, d.opts.Record, eng.selinux)
		if err != nil {
			return driver.Status{}, err
		}
		in.egress = e
	}
	// A stopped container this driver left for the id, as a server that
	// restarted finds one, would hold the name; it is removed first.
	if ours, running, err := d.leftover(ctx, in); err == nil && ours && !running {
		if err := d.remove(in.name); err != nil {
			_ = d.teardown(in)
			return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "removing the stopped container left for %s: %v", in.spec.ID, err)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := d.r.output(rctx, d.runArgs(in), nil); err != nil {
		if nameInUse(err) {
			// Someone else's container keeps its name and is never removed.
			if ours, _, lerr := d.leftover(ctx, in); lerr == nil && ours {
				_ = d.teardown(in)
			} else {
				_ = in.egress.close()
				in.egress = nil
			}
			return driver.Status{}, driver.Errorf(driver.ErrExists, "a container named %s already exists", in.name)
		}
		_ = d.teardown(in)
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "starting the container: %v", err)
	}
	if err := d.verify(ctx, in); err != nil {
		_ = d.teardown(in)
		return driver.Status{}, err
	}
	in.forwards, in.done = &forward.Set{}, make(chan struct{})
	return d.status(in, eng, driver.PhaseRunning), nil
}

// verify asks the kernel, from inside the container, what is in force, and
// refuses a container weaker than its arguments said.
func (d *Driver) verify(ctx context.Context, in *instance) error {
	vctx, cancel := context.WithTimeout(ctx, engineWait)
	defer cancel()
	out, err := d.r.output(vctx, []string{"exec", "--user", in.user, in.name, "/bin/sh", "-c", verifyScript}, nil)
	if err != nil {
		return driver.Errorf(driver.ErrUnavailable, "checking the container: %v", err)
	}
	got := map[string]string{}
	var netLines []string
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch k {
		case "net", "dev", "fib", "fib-read", "inet6":
			netLines = append(netLines, strings.TrimSpace(line))
		case "rootfs":
			return driver.Errorf(driver.ErrUnavailable, "the container's root filesystem is writable")
		default:
			got[k] = v
		}
	}
	var weak []string
	if got["Seccomp:"] != "2" {
		weak = append(weak, "no seccomp filter (Seccomp: "+got["Seccomp:"]+")")
	}
	if got["NoNewPrivs:"] != "1" {
		weak = append(weak, "no_new_privs is off")
	}
	for _, k := range []string{"CapEff:", "CapBnd:"} {
		if strings.Trim(got[k], "0") != "" || got[k] == "" {
			weak = append(weak, k+" "+got[k])
		}
	}
	if got["Uid:"] != strconv.Itoa(in.uid) {
		weak = append(weak, "runs as uid "+got["Uid:"]+", not "+strconv.Itoa(in.uid))
	}
	if !in.pol.AllowNetwork || in.pol.Egress != nil {
		weak = append(weak, networkWeakness(netLines)...)
	}
	if len(weak) > 0 {
		return driver.Errorf(driver.ErrUnavailable, "the container is weaker than asked, so it is removed: %s", strings.Join(weak, "; "))
	}
	return nil
}

// teardown removes in's container and stops its egress proxy; nothing of
// in may be running.
func (d *Driver) teardown(in *instance) error {
	err := d.remove(in.name)
	err = errors.Join(err, in.egress.close())
	in.egress = nil
	return err
}

// remove force-removes the named container; one already gone is not an error.
func (d *Driver) remove(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), engineWait)
	defer cancel()
	_, err := d.r.output(ctx, []string{"rm", "-f", name}, nil)
	if err != nil && notFound(err) {
		err = nil
	}
	return err
}

func (d *Driver) status(in *instance, eng engineInfo, phase driver.Phase) driver.Status {
	net := "no network"
	switch {
	case in.pol.Egress != nil:
		net = "network through the host's egress proxy only"
	case in.pol.AllowNetwork:
		net = "network open"
	}
	msg := fmt.Sprintf("container %s via %s · shared kernel · %s · image %s · user %s", in.name, d.name, net, in.req.image, in.user)
	if eng.rootless && !eng.podman {
		msg += " (root in a user namespace mapped to this user)"
	}
	return driver.Status{ID: in.spec.ID, Phase: phase, Tier: string(sandbox.TierContainer), Backend: eng.backend(d.name), Message: msg}
}

func (d *Driver) get(id string) (*instance, error) {
	in, ok := d.insts[id]
	if !ok {
		return nil, driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	return in, nil
}

// Status is what the engine says of the container; a container this driver
// did not create in this process is reported without a tier.
func (d *Driver) Status(ctx context.Context, id string) (driver.Status, error) {
	d.mu.Lock()
	in, known := d.insts[id]
	d.mu.Unlock()
	name := d.opts.prefix() + id
	if known {
		name = in.name
	}
	ictx, cancel := context.WithTimeout(ctx, engineWait)
	defer cancel()
	out, err := d.r.output(ictx, []string{"container", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}}", name}, nil)
	if err != nil {
		if notFound(err) {
			if known {
				return driver.Status{ID: id, Phase: driver.PhaseFailed, Message: "the container is gone"}, nil
			}
			return driver.Status{}, driver.Errorf(driver.ErrNotFound, "no instance %s", id)
		}
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "asking the engine: %v", err)
	}
	f := strings.Fields(string(out))
	if len(f) < 3 {
		return driver.Status{}, driver.Errorf(driver.ErrInternal, "the engine answered %q", strings.TrimSpace(string(out)))
	}
	phase := driver.PhaseStopped
	switch {
	case f[0] == "running":
		phase = driver.PhaseRunning
	case f[2] == "true" || (f[1] != "0" && f[1] != "143" && f[1] != "137"):
		phase = driver.PhaseFailed
	}
	if !known {
		return driver.Status{ID: id, Phase: phase, Message: "a container this driver did not start here: " + name}, nil
	}
	d.mu.Lock()
	stopped := in.stopped
	d.mu.Unlock()
	if stopped && phase == driver.PhaseRunning {
		phase = driver.PhaseStopped
	}
	eng, _ := d.engine(ctx)
	return d.status(in, eng, phase), nil
}

// Start runs a stopped instance's container again, from fresh mounts.
func (d *Driver) Start(ctx context.Context, id string) (driver.Status, error) {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.Status{}, err
	}
	d.mu.Unlock()
	in.life.Lock()
	defer in.life.Unlock()
	d.mu.Lock()
	stopped := in.stopped
	d.mu.Unlock()
	if !stopped {
		return d.Status(ctx, id)
	}
	eng, err := d.engine(ctx)
	if err != nil {
		return driver.Status{}, err
	}
	if err := d.teardown(in); err != nil {
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "removing the stopped container: %v", err)
	}
	// Starting again is a new incarnation, as the control plane counts it.
	in.spec.Generation++
	st, err := d.start(ctx, in, eng)
	if err != nil {
		return driver.Status{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.insts[id] != in {
		_ = d.teardown(in)
		return driver.Status{}, driver.Errorf(driver.ErrConflict, "instance %s was deleted while it started", id)
	}
	in.stopped = false
	return st, nil
}

// Stop refuses new commands, then stops the container: its processes get
// grace to end on SIGTERM before they are killed.
func (d *Driver) Stop(ctx context.Context, id string, grace time.Duration) (driver.Status, error) {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.Status{}, err
	}
	in.stopped = true
	d.mu.Unlock()
	in.life.Lock()
	defer in.life.Unlock()
	in.endIncarnation()
	secs := int((grace + time.Second - 1) / time.Second)
	sctx, cancel := context.WithTimeout(context.Background(), grace+engineWait)
	defer cancel()
	if _, err := d.r.output(sctx, []string{"stop", "-t", strconv.Itoa(secs), in.name}, nil); err != nil && !notFound(err) {
		return driver.Status{}, driver.Errorf(driver.ErrUnavailable, "stopping the container: %v", err)
	}
	in.running.Wait()
	err = in.egress.close()
	in.egress = nil
	if err != nil {
		return driver.Status{}, driver.Errorf(driver.ErrInternal, "stopping the egress proxy: %v", err)
	}
	eng, _ := d.engine(ctx)
	return d.status(in, eng, driver.PhaseStopped), nil
}

// Delete removes the instance's container. One this process did not
// start, such as after a restart, is removed when it carries this driver's
// label for the id.
func (d *Driver) Delete(ctx context.Context, id string) error {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return d.deleteLeftover(ctx, id)
	}
	in.stopped = true
	delete(d.insts, id)
	d.mu.Unlock()
	in.life.Lock()
	defer in.life.Unlock()
	in.endIncarnation()
	if err := d.remove(in.name); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "removing the container: %v", err)
	}
	if err := d.removeSnapshots(ctx, id); err != nil {
		return err
	}
	// The commands end with their container; their proxy goes after them.
	in.running.Wait()
	if err := in.egress.close(); err != nil {
		return driver.Errorf(driver.ErrInternal, "stopping the egress proxy: %v", err)
	}
	in.egress = nil
	return nil
}

// leftover reports whether a container holds in's name with this driver's
// labels for in's id, and whether it runs; err is the engine's, or not found.
func (d *Driver) leftover(ctx context.Context, in *instance) (ours, running bool, err error) {
	ictx, cancel := context.WithTimeout(ctx, engineWait)
	defer cancel()
	out, err := d.r.output(ictx, []string{"container", "inspect", "--format",
		`{{index .Config.Labels "abhed.sandbox"}} {{index .Config.Labels "abhed.driver"}} {{.State.Running}}`, in.name}, nil)
	if err != nil {
		return false, false, err
	}
	f := strings.Fields(string(out))
	if len(f) != 3 {
		return false, false, nil
	}
	return f[0] == in.spec.ID && f[1] == d.name, f[2] == "true", nil
}

func (d *Driver) deleteLeftover(ctx context.Context, id string) error {
	if !containerID.MatchString(id) {
		return driver.Errorf(driver.ErrNotFound, "no instance %s", id)
	}
	name := d.opts.prefix() + id
	ictx, cancel := context.WithTimeout(ctx, engineWait)
	defer cancel()
	out, err := d.r.output(ictx, []string{"container", "inspect", "--format", `{{index .Config.Labels "abhed.sandbox"}} {{index .Config.Labels "abhed.driver"}}`, name}, nil)
	if err != nil {
		if notFound(err) {
			return driver.Errorf(driver.ErrNotFound, "no instance %s", id)
		}
		return driver.Errorf(driver.ErrUnavailable, "asking the engine: %v", err)
	}
	if strings.TrimSpace(string(out)) != id+" "+d.name {
		return driver.Errorf(driver.ErrNotFound, "no instance %s: %s is not this driver's", id, name)
	}
	if err := d.remove(name); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "removing the container: %v", err)
	}
	return d.removeSnapshots(ctx, id)
}

// begin admits one command to a running instance; done must be called after.
// With git protection on, a git folder that appeared since the container
// started refuses the command: its mounts are fixed until a restart.
func (d *Driver) begin(ctx context.Context, id string) (*instance, func(), error) {
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return nil, nil, err
	}
	if in.stopped {
		d.mu.Unlock()
		return nil, nil, driver.Errorf(driver.ErrConflict, "instance %s is stopped", id)
	}
	in.running.Add(1)
	d.mu.Unlock()
	now, err := in.git.Args(ctx, in.pol, d.name)
	if err != nil {
		in.running.Done()
		return nil, nil, driver.Errorf(driver.ErrUnavailable, "%v", err)
	}
	if missing := newMounts(in.mounts, now); len(missing) > 0 {
		in.running.Done()
		return nil, nil, driver.Errorf(driver.ErrConflict,
			"%s must be protected, which this container's mounts cannot do once started; restart the sandbox", strings.Join(missing, ", "))
	}
	return in, in.running.Done, nil
}

// newMounts lists the mount sources in now that were not in at.
func newMounts(at, now []string) []string {
	had := map[string]bool{}
	for _, a := range at {
		had[a] = true
	}
	var out []string
	for i := 0; i+1 < len(now); i++ {
		if (now[i] == "-v" || now[i] == "--tmpfs") && !had[now[i+1]] {
			src, _, _ := strings.Cut(now[i+1], ":")
			out = append(out, src)
		}
	}
	return out
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
	if _, ok := sandbox.Within(dir, workspace); !ok {
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
		q, err := syntax.Quote(a, syntax.LangPOSIX)
		if err != nil {
			return "", driver.Errorf(driver.ErrInvalid, "argv[%d] cannot be quoted: %v", i, err)
		}
		parts[i] = q
	}
	return strings.Join(parts, " "), nil
}

// fixedEnv are the variables the sandbox sets, which a caller cannot replace.
var fixedEnv = []string{"PATH", "HOME", "ABHED_SANDBOX", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy", "LD_PRELOAD", "LD_LIBRARY_PATH"}

// callerEnv checks env and returns its names, for forwarding by name.
func callerEnv(env []string) ([]string, error) {
	var names []string
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t\n=") {
			return nil, driver.Errorf(driver.ErrInvalid, "env entry %q is not NAME=value", kv)
		}
		for _, f := range fixedEnv {
			if strings.EqualFold(k, f) {
				return nil, driver.Errorf(driver.ErrInvalid, "env %s is set by the sandbox and cannot be replaced", k)
			}
		}
		names = append(names, k)
	}
	return names, nil
}

func nonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// prepared is one command ready to run in an instance.
type prepared struct {
	cmd     *exec.Cmd
	pidFile string
	end     func()
}

// prepare builds the engine command running line in in, with env forwarded
// by name and, under the allowlist, a credential for this command alone.
func (d *Driver) prepare(ctx context.Context, in *instance, tty bool, dir, line string, env, fixed []string) (*prepared, error) {
	names, err := callerEnv(env)
	if err != nil {
		return nil, err
	}
	values := append([]string{}, env...)
	end := func() {}
	if in.egress != nil {
		callEnv, stop, err := in.egress.call(sandbox.LaunchOf(ctx).CallID)
		if err != nil {
			return nil, driver.Errorf(driver.ErrUnavailable, "%v", err)
		}
		end = stop
		for _, kv := range callEnv {
			k, _, _ := strings.Cut(kv, "=")
			names = append(names, k)
		}
		values = append(values, callEnv...)
	}
	pf := pidFile(nonce())
	cmd := d.r.command(ctx, d.execArgs(in, tty, dir, pf, line, names, fixed))
	cmd.Env = append(cmd.Env, values...)
	return &prepared{cmd: cmd, pidFile: pf, end: end}, nil
}

// kill ends what a cancelled command started inside the container.
func (d *Driver) kill(in *instance, pidFile string) {
	ctx, cancel := context.WithTimeout(context.Background(), engineWait)
	defer cancel()
	_, _ = d.r.output(ctx, []string{"exec", "--user", in.user, in.name, "/bin/sh", "-c", killTree, "abhed-kill", pidFile}, nil)
}

func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.ExecResult, error) {
	if err := req.Validate(); err != nil {
		return driver.ExecResult{}, err
	}
	line, err := commandLine(req)
	if err != nil {
		return driver.ExecResult{}, err
	}
	in, done, err := d.begin(ctx, id)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer done()
	dir, err := cwd(in.pol.Workspace, req.Cwd)
	if err != nil {
		return driver.ExecResult{}, err
	}
	p, err := d.prepare(ctx, in, false, dir, line, req.Env, nil)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer p.end()
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = req.Stdin, req.Stdout, req.Stderr
	p.cmd.Cancel = func() error { d.kill(in, p.pidFile); return p.cmd.Process.Kill() }
	p.cmd.WaitDelay = 2 * time.Second
	return d.result(ctx, in, p.cmd, p.cmd.Run())
}

// result is how a command ended, or an error when it did not run, the
// engine failed around it, or the caller gave up on it.
func (d *Driver) result(ctx context.Context, in *instance, cmd *exec.Cmd, err error) (driver.ExecResult, error) {
	if ctx.Err() != nil {
		return driver.ExecResult{}, ctx.Err()
	}
	if cmd.ProcessState == nil {
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the engine was not run: %v", err)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		return driver.ExecResult{}, driver.Errorf(driver.ErrInternal, "%v", err)
	}
	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the engine's CLI was killed by %v", ws.Signal())
	}
	// Podman's own failures are 125; any engine's, or a stopped container,
	// leave the container not running. Neither is the command's exit code.
	if code == 125 && in.eng.podman {
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the engine could not run the command")
	}
	if code != 0 {
		if st, serr := d.Status(context.Background(), in.spec.ID); serr != nil || st.Phase != driver.PhaseRunning {
			return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the container stopped around the command (exit %d)", code)
		}
	}
	return driver.ExecResult{ExitCode: code}, nil
}

// Command builds an engine exec running a command, or the interactive shell,
// in the container for the caller to run. Under the allowlist its proxy
// credential is revoked when ctx ends; a cancelled command is ended inside
// the container too.
func (d *Driver) Command(ctx context.Context, id string, req driver.CommandRequest) (*exec.Cmd, error) {
	in, done, err := d.begin(ctx, id)
	if err != nil {
		return nil, err
	}
	dir, err := cwd(in.pol.Workspace, req.Cwd)
	if err != nil {
		done()
		return nil, err
	}
	line, fixed := req.Command, []string(nil)
	if req.Shell {
		line, fixed = shellLine, []string{"TERM=xterm-256color", `PS1=(sandbox: container) \W \$ `, "HISTFILE=", "ABHED_TERMINAL=1"}
	}
	p, err := d.prepare(ctx, in, req.Shell, dir, line, nil, fixed)
	// Not counted as running: the caller owns it, and stopping the container ends it.
	done()
	if err != nil {
		return nil, err
	}
	cmd := p.cmd
	cmd.Cancel = func() error { d.kill(in, p.pidFile); return cmd.Process.Kill() }
	cmd.WaitDelay = 2 * time.Second
	context.AfterFunc(ctx, p.end)
	return cmd, nil
}

// ForwardEnv adds `-e NAME` for each name before the container's name, so
// the engine passes the value from its own environment.
func (d *Driver) ForwardEnv(cmd *exec.Cmd, names []string) {
	at := -1
	for i := len(cmd.Args) - 1; i >= 4; i-- {
		if cmd.Args[i] == execWrapper && cmd.Args[i-1] == "-c" && cmd.Args[i-2] == "/bin/sh" {
			at = i - 3
			break
		}
	}
	if at < 1 || len(names) == 0 {
		return
	}
	extra := make([]string, 0, 2*len(names))
	for _, n := range names {
		extra = append(extra, "-e", n)
	}
	cmd.Args = append(append(append([]string{}, cmd.Args[:at]...), extra...), cmd.Args[at:]...)
}

// Attach runs an interactive shell on a terminal in the container.
func (d *Driver) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.ExecResult, error) {
	in, done, err := d.begin(ctx, id)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer done()
	dir, err := cwd(in.pol.Workspace, req.Cwd)
	if err != nil {
		return driver.ExecResult{}, err
	}
	fixed := []string{"TERM=xterm-256color", `PS1=(sandbox: container) \W \$ `, "HISTFILE=", "ABHED_TERMINAL=1"}
	p, err := d.prepare(ctx, in, true, dir, shellLine, nil, fixed)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer p.end()
	cmd := p.cmd
	cmd.Cancel = func() error { d.kill(in, p.pidFile); return cmd.Process.Kill() }
	cmd.WaitDelay = 2 * time.Second
	size := &pty.Winsize{Rows: req.Size.Rows, Cols: req.Size.Cols}
	if size.Rows == 0 || size.Cols == 0 {
		size = &pty.Winsize{Rows: 30, Cols: 100}
	}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return driver.ExecResult{}, driver.Errorf(driver.ErrUnavailable, "the shell was not started: %v", err)
	}
	defer tty.Close()
	if req.Stdin != nil {
		go func() { _, _ = io.Copy(tty, req.Stdin) }()
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		w := req.Stdout
		if w == nil {
			w = io.Discard
		}
		_, _ = io.Copy(w, tty)
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
	select {
	case <-copied:
	case <-time.After(time.Second):
	}
	return d.result(ctx, in, cmd, werr)
}

// copyPath resolves p, which must exist, on the host and refuses it unless
// it is inside the workspace: the workspace is the same path inside, so a
// link out of it is caught here before the engine follows it.
func copyPath(workspace, p string) (string, error) {
	asked := p
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	p = filepath.Clean(p)
	if _, ok := sandbox.Within(p, workspace); !ok {
		return "", driver.Errorf(driver.ErrInvalid, "%s is outside the workspace", p)
	}
	real, err := filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) {
		// Named as asked: the host's path to the workspace is not the caller's.
		return "", driver.Errorf(driver.ErrInvalid, "%s does not exist in the sandbox's workspace", asked)
	} else if err != nil {
		return "", driver.Errorf(driver.ErrInvalid, "%s cannot be read in the sandbox's workspace", asked)
	}
	wsReal, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", driver.Errorf(driver.ErrUnavailable, "the workspace: %v", err)
	}
	rest, ok := sandbox.Within(real, wsReal)
	if !ok {
		return "", driver.Errorf(driver.ErrInvalid, "%s leads outside the workspace", p)
	}
	for _, part := range rest {
		if strings.EqualFold(part, ".abhed") {
			return "", driver.Errorf(driver.ErrInvalid, "%s is Abhed's state", p)
		}
	}
	// The path inside is the workspace as mounted, with the real remainder.
	return filepath.Join(append([]string{workspace}, rest...)...), nil
}

// CopyIn unpacks a tar stream into dst, a folder in the workspace, as the
// sandbox's user; the archive is checked entry by entry on the way.
func (d *Driver) CopyIn(ctx context.Context, id, dst string, archive io.Reader) error {
	in, done, err := d.begin(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	target, err := copyPath(in.pol.Workspace, dst)
	if err != nil {
		return err
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		return driver.Errorf(driver.ErrInvalid, "%s is not a folder", dst)
	}
	pr, pw := io.Pipe()
	checked := make(chan error, 1)
	go func() {
		err := recheck(archive, pw, in.uid, in.gid)
		pw.CloseWithError(err)
		checked <- err
	}()
	_, cerr := d.r.output(ctx, []string{"cp", "-a", "-", in.name + ":" + target}, pr)
	_ = pr.Close()
	if err := <-checked; err != nil {
		return err
	}
	if cerr != nil {
		return driver.Errorf(driver.ErrUnavailable, "copying in: %v", cerr)
	}
	return nil
}

// CopyOut writes src, in the workspace, as a tar stream, checked entry by
// entry, since what the sandbox wrote is untrusted.
func (d *Driver) CopyOut(ctx context.Context, id, src string, archive io.Writer) error {
	in, done, err := d.begin(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	source, err := copyPath(in.pol.Workspace, src)
	if err != nil {
		return err
	}
	cmd := d.r.command(ctx, []string{"cp", in.name + ":" + source, "-"})
	out, err := cmd.StdoutPipe()
	if err != nil {
		return driver.Errorf(driver.ErrInternal, "%v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "copying out: %v", err)
	}
	rerr := recheck(out, archive, in.uid, in.gid)
	if rerr != nil {
		_ = cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if rerr != nil {
		return rerr
	}
	if werr != nil {
		return driver.Errorf(driver.ErrUnavailable, "copying out: %v: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return nil
}
