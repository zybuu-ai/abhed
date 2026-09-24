package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

// runArgs is everything up to the image: the confinement both a command and
// a shell run under. It begins with "run --rm -i".
func (c *Container) runArgs(cwd string) []string {
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
	// the memory and pid caps below do not cover on their own.
	args = append(args,
		"--ulimit", "nproc=256:256",
		"--ulimit", "nofile=1024:1024",
		"--ulimit", "fsize=536870912:536870912", // 512 MB per file
		"--ulimit", "core=0:0",
	)

	// A CPU cap so one session cannot starve the host. Not security, but a
	// session that pins every core is indistinguishable from an outage.
	args = append(args, "--cpus", "2")

	// No IPC or UTS sharing with anything else, and a private PID namespace,
	// so one session cannot see or signal another's processes.
	args = append(args, "--ipc", "private", "--uts", "private")

	if !c.policy.AllowNetwork {
		args = append(args, "--network", "none")
	}
	if c.policy.MaxMemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(c.policy.MaxMemoryMB)+"m")
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
	return args
}

func (c *Container) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	args := append(c.runArgs(cwd), Image, "/bin/sh", "-c", command)
	cmd := exec.CommandContext(ctx, c.runtime, args...)
	// The engine's CLI needs the host's PATH, HOME and DOCKER_HOST; only the
	// -e flags above reach the container.
	cmd.Env = os.Environ()
	return cmd
}

// containerShell prefers bash where the image has it.
const containerShell = `if command -v bash >/dev/null 2>&1; then exec bash --noprofile --norc -i; fi; PS1='(sandbox: container) $ ' exec sh -i`

// Shell starts an interactive shell in a container of its own, with a
// terminal (-t). The container is named so that ending the shell removes it,
// even when the engine's CLI is killed before it can.
func (c *Container) Shell(ctx context.Context, cwd string) *exec.Cmd {
	name := "abhed-term-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	args := []string{"run", "--rm", "-i", "-t", "--name", name}
	args = append(args, c.runArgs(cwd)[3:]...)
	for _, kv := range shellEnv(c.Tier()) {
		args = append(args, "-e", kv)
	}
	args = append(args, "-e", "HOME=/tmp", Image, "/bin/sh", "-c", containerShell)
	cmd := exec.CommandContext(ctx, c.runtime, args...) // #nosec G204 -- the configured engine; the shell is fixed
	cmd.Env = os.Environ()
	runtime := c.runtime
	cmd.Cancel = func() error {
		rm := exec.Command(runtime, "rm", "-f", name) // #nosec G204 -- the configured engine and a name this process made
		_ = rm.Run()
		return cmd.Process.Kill()
	}
	return cmd
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
