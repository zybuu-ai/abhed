package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Image is the default execution image. Air-gapped installs pin it by digest: a
// tag is mutable.
var Image = envOr("ABHED_SANDBOX_IMAGE", "docker.io/library/debian:bookworm-slim")

// Container runs commands in an OCI container. The kernel is shared, so an
// exploit crosses it: untrusted work needs TierVM.
type Container struct {
	policy  Policy
	runtime string // docker | podman | nerdctl
	extra   []string

	once      sync.Once
	available bool
	reason    string
	// podman is set when the runtime is Podman by what it says it is, as the
	// podman-docker wrapper named docker is; see engine.
	podman bool
	// git is what the look for git folders keeps between commands.
	git gitMemory

	// The egress proxy under sandbox.network allowlist, started once.
	egOnce  sync.Once
	eg      *egressState
	egProxy *egress.Proxy
	egErr   error
}

func NewContainer(p Policy) *Container {
	return &Container{policy: p}
}

// NewGVisor is a Container pinned to the runsc runtime. gVisor serves syscalls
// in userspace, keeping the host kernel out of reach.
func NewGVisor(p Policy) *Container {
	return &Container{policy: p, extra: []string{"--runtime", "runsc"}}
}

func (c *Container) isGVisor() bool {
	for i, a := range c.extra {
		if a == "--runtime" && i+1 < len(c.extra) && c.extra[i+1] == "runsc" {
			return true
		}
	}
	return false
}

func (c *Container) Tier() Tier {
	if c.isGVisor() {
		return TierVM
	}
	return TierContainer
}

func (c *Container) Available() (bool, string) {
	// The proxy listens on the host's loopback, which a container with no
	// network reaches only through a relay inside it.
	if c.policy.Egress != nil {
		if _, why := c.containerRelay(); why != "" {
			return false, why
		}
	}
	c.once.Do(func() {
		for _, rt := range []string{"docker", "podman", "nerdctl"} {
			path, err := exec.LookPath(rt)
			if err != nil {
				continue
			}
			// A binary on PATH is not a working daemon; check for real.
			// Plain info: Podman's has no ServerVersion, so a format naming it failed there.
			if _, err := engineProbe(path, "info"); err != nil {
				c.reason = rt + " found but its daemon is not responding: " + err.Error()
				continue
			}
			c.runtime = path
			c.podman = saysPodman(path)
			c.available = true
			c.reason = ""
			return
		}
		if c.reason == "" {
			c.reason = "no container runtime found (docker, podman, nerdctl)"
		}
	})

	if !c.available {
		return false, c.reason
	}
	if c.isGVisor() && !c.runscInstalled() {
		return false, "runsc (gVisor) runtime not registered with the container engine"
	}
	return true, ""
}

func (c *Container) runscInstalled() bool {
	if _, err := exec.LookPath("runsc"); err == nil {
		return true
	}
	out, err := engineProbe(c.runtime, "info", "--format", "{{json .Runtimes}}")
	return err == nil && strings.Contains(string(out), "runsc")
}

func (c *Container) Describe() string {
	net := "network disabled"
	if c.policy.AllowNetwork {
		net = "network enabled"
	}
	engine := c.runtime
	if i := strings.LastIndex(engine, "/"); i >= 0 {
		engine = engine[i+1:]
	}
	if c.isGVisor() {
		return fmt.Sprintf("gVisor (runsc) via %s · syscall interception · %s · image %s", engine, net, Image)
	}
	return fmt.Sprintf("OCI container via %s · shared kernel · %s · image %s", engine, net, Image)
}

// engine is the runtime's program name: podman for Podman under any name, which
// reads PID and UTS from its own configuration.
func (c *Container) engine() string {
	base := filepath.Base(c.runtime)
	if c.podman || strings.HasPrefix(base, "podman") {
		return "podman"
	}
	return base
}

// saysPodman reports whether the runtime at path calls itself Podman.
func saysPodman(path string) bool {
	out, err := engineProbe(path, "--version")
	return err == nil && strings.Contains(strings.ToLower(string(out)), "podman")
}

// engineProbeWait bounds each engine query while the tier is chosen, so a hung
// engine cannot hold a session's start.
var engineProbeWait = 5 * time.Second

// engineProbe runs the engine's CLI with args and returns its output, ended
// with everything it started after engineProbeWait.
func engineProbe(path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), engineProbeWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- the container runtime found on PATH, fixed arguments
	ownGroup(cmd)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return out, fmt.Errorf("%s %s did not answer within %s", filepath.Base(path), strings.Join(args, " "), engineProbeWait)
	}
	return out, err
}

// runArgs is the confinement a command and a shell both run under, up to the
// image; an error is why the command is not run.
func (c *Container) runArgs(ctx context.Context, cwd string) ([]string, error) {
	args := []string{"run", "--rm", "-i"}
	args = append(args, c.extra...)

	// Drop every capability, then add nothing back: an agent's shell has no
	// legitimate need for CAP_NET_ADMIN or CAP_SYS_ADMIN.
	args = append(args,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
	)

	// A read-only rootfs stops a command leaving a binary on PATH or an edited
	// profile behind for the next one.
	args = append(args, "--read-only")

	// Scratch space is sized and noexec, so it cannot run a downloaded binary.
	args = append(args,
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=256m",
		"--tmpfs", "/run:rw,noexec,nosuid,nodev,size=16m",
	)

	// Fork bombs and disk-fill are host DoS. --pids-limit is preferred: under
	// rootful Docker nproc counts the uid's every host process.
	if c.policy.MaxProcs <= 0 {
		args = append(args, "--ulimit", "nproc=256:256")
	}
	args = append(args,
		"--ulimit", "nofile=1024:1024",
		"--ulimit", "fsize=536870912:536870912", // 512 MB per file
		"--ulimit", "core=0:0",
	)

	// A CPU cap so one session cannot starve the host. Not security, but a
	// session that pins every core is indistinguishable from an outage.
	args = append(args, "--cpus", c.cpus())

	// No IPC sharing, and a hostname of its own: the engine refuses a hostname
	// with the host's UTS namespace, so the flag also fails closed.
	args = append(args, "--ipc", "private", "--hostname", "abhed")
	// Docker's PID and UTS are always private and it refuses "private"; Podman
	// takes both from containers.conf, which could say host, so pin them there.
	if c.engine() == "podman" {
		args = append(args, "--pid", "private", "--uts", "private")
	}

	// Under the allowlist the proxy is the only way out, whatever AllowNetwork says.
	if !c.policy.AllowNetwork || c.policy.Egress != nil {
		args = append(args, "--network", "none")
	}
	if c.policy.MaxMemoryMB > 0 {
		// Swap set to the same: by default the engine allows as much swap
		// again, so the bound was twice what it said.
		m := strconv.Itoa(c.policy.MaxMemoryMB) + "m"
		args = append(args, "--memory", m, "--memory-swap", m)
	}
	if c.policy.MaxProcs > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(c.policy.MaxProcs))
	}

	mounts, err := mountArgs(ctx, c.policy, string(c.Tier()), &c.git)
	if err != nil {
		return nil, err
	}
	args = append(args, mounts...)

	workdir := cwd
	if workdir == "" {
		workdir = c.policy.Workspace
	}
	args = append(args, "-w", workdir)

	// Run as the invoking user so files created in the workspace are owned by
	// them rather than root — otherwise the host is left with unwritable files.
	if uid, gid := os.Getuid(), os.Getgid(); uid > 0 {
		args = append(args, "--user", fmt.Sprintf("%d:%d", uid, gid))
	}

	args = append(args, "-e", "ABHED_SANDBOX="+string(c.Tier()))
	return args, nil
}

// cpus is the --cpus value: MaxCPUPercent of one CPU, two when unset.
func (c *Container) cpus() string {
	pct := c.policy.MaxCPUPercent
	if pct <= 0 {
		pct = 200
	}
	return strconv.FormatFloat(float64(pct)/100, 'f', -1, 64)
}

// mountArgs are a container's host mounts for p, Abhed's state hidden; backend
// names the tier in records, mem is what the git look keeps.
func mountArgs(ctx context.Context, p Policy, backend string, mem *gitMemory) ([]string, error) {
	var args []string
	// The workspace is the only writable host path. Mounted at the same path
	// inside so the model's absolute paths stay valid across the boundary.
	args = append(args, "-v", p.Workspace+":"+p.Workspace)
	for _, ro := range p.ReadOnlyPaths {
		args = append(args, "-v", ro+":"+ro+":ro")
	}
	// Mounted over the workspace's own, so only the paths that exist can be.
	// A folder holding one is mounted onto itself, so it cannot be renamed.
	ws := PathForms(p.Workspace)
	var protected []string
	for _, wp := range formsOf(p.WriteProtected) {
		if insideAny(wp, ws) {
			protected = append(protected, wp)
		}
	}
	// The git folders found now, as on bubblewrap, after a planted
	// commondir is taken out.
	if p.ProtectGit {
		found, err := gitGuardFor(ctx, p.Workspace, backend, mem, p.GitRequired)
		if err != nil {
			return nil, err
		}
		protected = append(protected, found...)
	}
	for _, h := range holders(ws, protected) {
		if info, err := os.Lstat(h); err == nil && info.IsDir() {
			args = append(args, "-v", h+":"+h)
		} else if err == nil && info.Mode().IsRegular() {
			args = append(args, "-v", h+":"+h+":ro")
		}
	}
	for _, pp := range protected {
		if _, err := os.Stat(pp); err == nil {
			args = append(args, "-v", pp+":"+pp+":ro")
		}
	}
	return append(args, stateMounts(p)...), nil
}

// ContainerMounts gives a long-lived container the mounts the container
// tier gives each command, with its own memory of the git folders found.
type ContainerMounts struct{ git gitMemory }

// Args are the mounts for p now; backend names the sandbox in records. An
// error is why no command may start.
func (m *ContainerMounts) Args(ctx context.Context, p Policy, backend string) ([]string, error) {
	return mountArgs(ctx, p, backend, &m.git)
}

func (c *Container) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	if c.policy.Egress != nil {
		return c.egressCommand(ctx, cwd, command)
	}
	// Named, so a cancel can remove the container: killing the engine's CLI
	// leaves what runs inside it running.
	args, err := c.runArgs(ctx, cwd)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	name := containerName("abhed-cmd-")
	args = append(args, "--name", name, Image, "/bin/sh", "-c", command)
	cmd := exec.CommandContext(ctx, c.runtime, args...) // #nosec G204 -- the configured engine; the command runs inside the container
	// The engine's CLI needs the host's PATH, HOME and DOCKER_HOST; only the
	// -e flags above reach the container.
	cmd.Env = os.Environ()
	cmd.Cancel = c.remove(cmd, name)
	return cmd
}

// containerName is a fresh name for a container this process starts.
func containerName(prefix string) string {
	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 36) + hex.EncodeToString(nonce[:])
}

// remove is a cancel that removes the named container, then kills the CLI.
func (c *Container) remove(cmd *exec.Cmd, name string) func() error {
	runtime, rm := c.runtime, c.rmArgs(name)
	return func() error {
		rmCtx, cancel := context.WithTimeout(context.Background(), engineWait)
		defer cancel()
		_ = exec.CommandContext(rmCtx, runtime, rm...).Run() // #nosec G204 -- the configured engine and a name this process made
		return cmd.Process.Kill()
	}
}

// containerShell prefers bash where the image has it.
const containerShell = `if command -v bash >/dev/null 2>&1; then exec bash --noprofile --norc -l -O huponexit -i; fi; PS1='(sandbox: container) $ ' exec sh -i`

// engineWait bounds a call to the engine made while ending or sweeping
// shells, so an engine that hangs cannot hold a terminal open.
const engineWait = 20 * time.Second

// shellLabel marks this server's terminal containers: the host and the
// workspace, so a restarted server finds the ones it left behind.
func (c *Container) shellLabel() string {
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte(host + "\x00" + c.policy.Workspace))
	return "abhed.term=" + hex.EncodeToString(sum[:6])
}

// Shell starts an interactive shell in a container of its own, with a terminal
// (-t).
func (c *Container) Shell(ctx context.Context, cwd string) *exec.Cmd {
	run, err := c.runArgs(ctx, cwd)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	name := containerName("abhed-term-")
	args := []string{"run", "--rm", "-i", "-t", "--name", name, "--label", c.shellLabel()}
	args = append(args, run[3:]...)
	for _, kv := range shellEnv(c.Tier()) {
		args = append(args, "-e", kv)
	}
	args = append(args, "-e", "HOME=/tmp", Image, "/bin/sh", "-c", containerShell)
	cmd := exec.CommandContext(ctx, c.runtime, args...) // #nosec G204 -- the configured engine; the shell is fixed
	cmd.Env = os.Environ()
	cmd.Cancel = c.remove(cmd, name)
	cmd.WaitDelay = hangUpDelay
	return cmd
}

// SweepShells removes terminal containers this server left running, as a
// crash does. Only containers with this server's label are touched.
func (c *Container) SweepShells() {
	if ok, _ := c.Available(); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), engineWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.runtime, "ps", "-aq", "--filter", "label="+c.shellLabel()).Output() // #nosec G204 -- the configured engine
	if err != nil {
		return
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		_ = exec.CommandContext(ctx, c.runtime, c.rmArgs(ids...)...).Run() // #nosec G204 -- the configured engine and ids it listed
	}
}

// Backend names the engine, and gVisor's runtime when it is in use.
func (c *Container) Backend() string {
	engine := c.runtime
	if i := strings.LastIndex(engine, "/"); i >= 0 {
		engine = engine[i+1:]
	}
	if c.isGVisor() {
		return "runsc via " + engine
	}
	return engine
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ForwardEnv passes the named variables, already set in cmd.Env, into a
// container command. `-e NAME` keeps each value out of its arguments.
func ForwardEnv(cmd *exec.Cmd, names []string) {
	at := -1
	for i, a := range cmd.Args {
		// An exec names its container in the image's place.
		if a == Image || strings.HasPrefix(a, egressPrefix) {
			at = i
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
	args := append(append(append([]string{}, cmd.Args[:at]...), extra...), cmd.Args[at:]...)
	cmd.Args = args
}

// stateMounts cover Abhed's state the mounts above would show: an empty folder
// over each state folder, /dev/null over a state file.
func stateMounts(p Policy) []string {
	ws := p.Workspace
	dir := filepath.Join(ws, stateDir)
	// Made here as the person, or the engine would make it as root.
	_ = os.Mkdir(dir, 0o700)
	var args []string
	tmpfs := func(path string) {
		args = append(args, "--tmpfs", path+":rw,noexec,nosuid,nodev,size=16m,mode=0700")
	}
	// On a disk that ignores case, .ABHED reaches the same folder by another
	// name the engine's own kernel keeps apart, so each spelling is covered.
	for _, name := range caseSpellings(ws, stateDir) {
		tmpfs(filepath.Join(ws, name))
	}
	visible := append(PathForms(ws), formsOf(p.ReadOnlyPaths)...)
	for _, sp := range formsOf(p.StatePaths) {
		if !insideAny(sp, visible) {
			continue
		}
		if info, err := os.Stat(sp); err == nil && info.IsDir() {
			tmpfs(sp)
		} else if err == nil {
			args = append(args, "-v", "/dev/null:"+sp+":ro")
		}
	}
	return args
}

// caseSpellings is name, and on a disk where dir folds case, every spelling
// of it in upper and lower case.
func caseSpellings(dir, name string) []string {
	upper := filepath.Join(dir, strings.ToUpper(name))
	if _, err := os.Lstat(upper); err != nil || strings.ToUpper(name) == name {
		return []string{name}
	}
	var letters []int
	for i, r := range name {
		if strings.ToUpper(string(r)) != strings.ToLower(string(r)) {
			letters = append(letters, i)
		}
	}
	out := make([]string, 0, 1<<len(letters))
	for mask := 0; mask < 1<<len(letters); mask++ {
		b := []byte(strings.ToLower(name))
		for bit, i := range letters {
			if mask&(1<<bit) != 0 {
				b[i] = strings.ToUpper(string(b[i]))[0]
			}
		}
		out = append(out, string(b))
	}
	return out
}

// Close stops the egress proxy commands under the allowlist reach.
func (c *Container) Close() error { return c.closeEgress() }

// rmArgs removes the named containers; --time 0 stops Podman waiting ten
// seconds on a process that ignores SIGTERM.
func (c *Container) rmArgs(names ...string) []string {
	args := []string{"rm", "-f"}
	if c.engine() == "podman" {
		args = append(args, "--time", "0")
	}
	return append(args, names...)
}
