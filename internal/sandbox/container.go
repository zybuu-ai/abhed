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
	"sync/atomic"
	"time"
)

// Image is the default execution image. In an air-gapped install this is
// digest-pinned and ships inside the signed bundle (docs/ops/air-gap.md);
// a tag is mutable and therefore unreproducible.
var Image = envOr("ABHED_SANDBOX_IMAGE", "docker.io/library/debian:bookworm-slim")

// Container runs commands in an OCI container.
//
// Namespace isolation with a SHARED KERNEL. Adequate for confining accidental
// damage and scoping the filesystem; NOT adequate against code actively trying
// to escape, because a kernel exploit crosses this boundary. That is why
// TierContainer sits below TierVM and why untrusted work should require the
// latter.
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
	// gitNoted is set once the walk for git folders has been recorded
	// stopping at its bound.
	gitNoted atomic.Bool
}

func NewContainer(p Policy) *Container {
	return &Container{policy: p}
}

// NewGVisor is a Container pinned to the runsc runtime.
//
// gVisor intercepts syscalls in userspace, which is a materially stronger
// boundary than namespaces: the guest kernel surface the workload can reach is
// a reimplementation, not the host kernel. Reported overhead is roughly
// 10-20% on syscall-heavy work — worth it for untrusted code.
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
	c.once.Do(func() {
		for _, rt := range []string{"docker", "podman", "nerdctl"} {
			path, err := exec.LookPath(rt)
			if err != nil {
				continue
			}
			// A binary on PATH is not a working daemon; check for real.
			probe := exec.Command(path, "info", "--format", "{{.ServerVersion}}")
			if err := probe.Run(); err != nil {
				c.reason = rt + " found but its daemon is not responding"
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
	out, err := exec.Command(c.runtime, "info", "--format", "{{json .Runtimes}}").Output()
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

// engine is the runtime's program name, such as docker or podman: podman
// for Podman under any name (podman-remote, or the podman-docker wrapper
// named docker), which reads PID and UTS from its own configuration.
func (c *Container) engine() string {
	base := filepath.Base(c.runtime)
	if c.podman || strings.HasPrefix(base, "podman") {
		return "podman"
	}
	return base
}

// saysPodman reports whether the runtime at path calls itself Podman.
func saysPodman(path string) bool {
	out, err := exec.Command(path, "--version").Output() // #nosec G204 -- the container runtime found on PATH
	return err == nil && strings.Contains(strings.ToLower(string(out)), "podman")
}

// runArgs is everything up to the image: the confinement both a command and
// a shell run under. It begins with "run --rm -i". An error is why the
// command is not run.
func (c *Container) runArgs(ctx context.Context, cwd string) ([]string, error) {
	args := []string{"run", "--rm", "-i"}
	args = append(args, c.extra...)

	// Drop every capability, then add nothing back: an agent's shell has no
	// legitimate need for CAP_NET_ADMIN or CAP_SYS_ADMIN.
	args = append(args,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
	)

	// The image itself is immutable. A writable rootfs lets a session drop a
	// binary somewhere on PATH, or edit a shell profile, and lets one command
	// leave something behind for the next — the container is per-command, so
	// without this the filesystem is the one place state could persist.
	args = append(args, "--read-only")

	// Somewhere to write, sized and mounted noexec so it cannot become the
	// place a downloaded binary is run from. Compilers and package managers
	// need a scratch directory; an attacker needs an executable one, and
	// these are not the same requirement.
	args = append(args,
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=256m",
		"--tmpfs", "/run:rw,noexec,nosuid,nodev,size=16m",
	)

	// Fork bombs and disk-fill are denial of service against the host, which
	// the memory and pid caps below do not cover on their own. Processes are
	// bounded by --pids-limit when there is one: an nproc limit counts every
	// process of the same uid on the host under rootful Docker.
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
	args = append(args, "--cpus", "2")

	// No IPC sharing, and a hostname of its own: the engine refuses a hostname
	// with the host's UTS namespace, so the flag also fails closed.
	args = append(args, "--ipc", "private", "--hostname", "abhed")
	// Docker's PID and UTS are always private and it refuses "private"; Podman
	// takes both from containers.conf, which could say host, so pin them there.
	if c.engine() == "podman" {
		args = append(args, "--pid", "private", "--uts", "private")
	}

	if !c.policy.AllowNetwork {
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

	// The workspace is the only writable host path. Mounted at the same path
	// inside so the model's absolute paths stay valid across the boundary.
	args = append(args, "-v", c.policy.Workspace+":"+c.policy.Workspace)
	for _, p := range c.policy.ReadOnlyPaths {
		args = append(args, "-v", p+":"+p+":ro")
	}
	// Mounted over the workspace's own, so only the paths that exist can be.
	// A folder holding one is mounted onto itself, so it cannot be renamed.
	ws := PathForms(c.policy.Workspace)
	var protected []string
	for _, p := range formsOf(c.policy.WriteProtected) {
		if insideAny(p, ws) {
			protected = append(protected, p)
		}
	}
	// The git folders found now, as on bubblewrap, after a planted
	// commondir is taken out.
	if c.policy.ProtectGit {
		found, err := gitGuard(ctx, c.policy.Workspace, string(c.Tier()), &c.gitNoted)
		if err != nil {
			return nil, err
		}
		protected = append(protected, found...)
	}
	for _, p := range holders(ws, protected) {
		if info, err := os.Lstat(p); err == nil && info.IsDir() {
			args = append(args, "-v", p+":"+p)
		} else if err == nil && info.Mode().IsRegular() {
			args = append(args, "-v", p+":"+p+":ro")
		}
	}
	for _, p := range protected {
		if _, err := os.Stat(p); err == nil {
			args = append(args, "-v", p+":"+p+":ro")
		}
	}
	args = append(args, c.stateMounts()...)

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

func (c *Container) Command(ctx context.Context, cwd, command string) *exec.Cmd {
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
	runtime := c.runtime
	return func() error {
		rmCtx, cancel := context.WithTimeout(context.Background(), engineWait)
		defer cancel()
		_ = exec.CommandContext(rmCtx, runtime, "rm", "-f", name).Run() // #nosec G204 -- the configured engine and a name this process made
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

// Shell starts an interactive shell in a container of its own, with a
// terminal (-t). The container is named so that ending the shell removes it,
// even when the engine's CLI is killed before it can.
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
		_ = exec.CommandContext(ctx, c.runtime, append([]string{"rm", "-f"}, ids...)...).Run() // #nosec G204 -- the configured engine and ids it listed
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
// container command. `-e NAME` makes the engine read the value from its own
// environment, so no value appears in its arguments. Other commands are left
// alone: their cmd.Env already reaches the shell.
func ForwardEnv(cmd *exec.Cmd, names []string) {
	at := -1
	for i, a := range cmd.Args {
		if a == Image {
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

// stateMounts hide Abhed's own state that the mounts above would show: an
// empty throwaway folder over the workspace's .abhed, as the process tier
// mounts, and the same over a state folder, /dev/null over a state file.
func (c *Container) stateMounts() []string {
	ws := c.policy.Workspace
	dir := filepath.Join(ws, stateDir)
	// Made here as the person, or the engine would make it as root.
	_ = os.Mkdir(dir, 0o700)
	var args []string
	tmpfs := func(p string) {
		args = append(args, "--tmpfs", p+":rw,noexec,nosuid,nodev,size=16m,mode=0700")
	}
	// On a disk that ignores case, .ABHED reaches the same folder by another
	// name the engine's own kernel keeps apart, so each spelling is covered.
	for _, name := range caseSpellings(ws, stateDir) {
		tmpfs(filepath.Join(ws, name))
	}
	visible := append(PathForms(ws), formsOf(c.policy.ReadOnlyPaths)...)
	for _, p := range formsOf(c.policy.StatePaths) {
		if !insideAny(p, visible) {
			continue
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			tmpfs(p)
		} else if err == nil {
			args = append(args, "-v", "/dev/null:"+p+":ro")
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
