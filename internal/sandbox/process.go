package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Process confines a command with OS process-level primitives: macOS
// sandbox-exec (Seatbelt) or Linux bubblewrap.
//
// This is a REAL boundary for filesystem and network scoping, and it is
// verified by the tests in this package. It is NOT a boundary against kernel
// exploitation — the kernel is shared. Use TierVM for untrusted code.
type Process struct {
	policy  Policy
	backend string // "sandbox-exec" | "bwrap"

	// Whether bwrap may mount a fresh /proc and /dev here. Inside a hardened
	// container (capabilities dropped, masked paths) the kernel refuses those
	// mounts and every command died with "Can't mount proc" — a sandbox
	// failure that looked exactly like an agent failure. Probed once.
	freshOnce sync.Once
	freshOK   bool
}

func NewProcess(p Policy) *Process {
	s := &Process{policy: p}
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err == nil {
			s.backend = "sandbox-exec"
		}
	case "linux":
		if _, err := exec.LookPath("bwrap"); err == nil {
			s.backend = "bwrap"
		}
	}
	return s
}

func (s *Process) Tier() Tier { return TierProcess }

func (s *Process) Available() (bool, string) {
	if s.backend == "" {
		switch runtime.GOOS {
		case "darwin":
			return false, "sandbox-exec not found"
		case "linux":
			return false, "bubblewrap (bwrap) not installed"
		default:
			return false, runtime.GOOS + " has no supported process sandbox"
		}
	}
	return true, ""
}

func (s *Process) Describe() string {
	net := "no network"
	if s.policy.AllowNetwork {
		net = "network allowed"
	}
	return fmt.Sprintf("process isolation via %s · workspace-scoped writes · %s", s.backend, net)
}

// seatbeltProfile renders a macOS Seatbelt profile.
//
// Deliberately built on (allow default) with targeted denials rather than
// (deny default) with targeted allows. A deny-default profile breaks DNS and
// mach service lookup in ways that make ordinary toolchains fail confusingly,
// and a sandbox people disable is worth nothing. The denials below are the ones
// that actually matter for an agent: writes outside the workspace, and network.
// stateDir mirrors tools.StateDir; the package is kept free of tool imports.
const stateDir = ".abhed"

// tempAreas are the system temp folders commands may write.
var tempAreas = []string{"/private/tmp", "/private/var/tmp"}

// userTemp is the per-user TMPDIR, resolved.
func userTemp() string {
	tmp := strings.TrimSuffix(os.Getenv("TMPDIR"), "/")
	if tmp == "" {
		return ""
	}
	// Trim the trailing slash BEFORE resolving: EvalSymlinks on a path with
	// one can resolve somewhere other than intended.
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	return tmp
}

// cacheAreas are the toolchain caches under home that commands may write.
func cacheAreas() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range []string{".cache", "Library/Caches", ".npm", ".cargo/registry", "go/pkg/mod"} {
		out = append(out, filepath.Join(home, c))
	}
	return out
}

// WritableAreas are the folders beyond the workspace that a sandboxed
// command may write on some backend: temp folders and toolchain caches. A
// file there cannot be protected by the sandbox's rules, since a command can
// move the folder around it.
func WritableAreas() []string {
	out := append([]string{"/tmp", "/var/tmp", os.TempDir()}, tempAreas...)
	if tmp := userTemp(); tmp != "" {
		out = append(out, tmp)
	}
	return append(out, cacheAreas()...)
}

func (s *Process) seatbeltProfile() string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n\n")

	b.WriteString(";; Writes are confined to the workspace and standard temp dirs.\n")
	b.WriteString("(deny file-write*)\n")
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", s.policy.Workspace)
	for _, p := range append(tempAreas[:len(tempAreas):len(tempAreas)], "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/urandom", "/dev/dtracehelper") {
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", p)
	}
	// A command on a terminal reopens its tty; the workbench runs one that way.
	b.WriteString("(allow file-write* (literal \"/dev/tty\") (regex #\"^/dev/ttys[0-9]+$\"))\n")
	// macOS gives each user a private TMPDIR under /var/folders, and compilers
	// put their work directories there. Without this, every `go build`, `cc` and
	// `cargo build` inside the sandbox fails with "operation not permitted" —
	// which reads as an agent error rather than a sandbox one. Found by running
	// the USAGE.md quickstart end to end.
	if tmp := userTemp(); tmp != "" {
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", tmp)
	}

	// Toolchains need writable caches or builds fail in ways that look like
	// agent errors rather than sandbox errors.
	for _, c := range cacheAreas() {
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", c)
	}

	// The harness's own state is out of reach for commands, as it is for the
	// file tools: the later rule wins, so this holds inside the workspace allow.
	b.WriteString("\n;; Abhed's own configuration, users and keys.\n")
	state := filepath.Join(s.policy.Workspace, stateDir)
	fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", state)
	fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", state)
	// A stat of it succeeds, so pytest, ls -R and git pass it by; listing
	// it and reading what it holds do not.
	fmt.Fprintf(&b, "(allow file-read-metadata (subpath %q))\n", state)
	if home, err := os.UserHomeDir(); err == nil {
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", filepath.Join(home, stateDir))
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", filepath.Join(home, stateDir))
		// Skills are the one part of it a command may need: a skill can ship a script.
		fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", filepath.Join(home, stateDir, "skills"))
	}

	for _, p := range s.statePaths() {
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", p)
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", p)
	}

	if !s.policy.AllowNetwork {
		b.WriteString("\n;; Egress denied: a successful injection has no channel out.\n")
		b.WriteString("(deny network*)\n")
		// Nor a view of the host's network: its interfaces, addresses and
		// routes, as bwrap's --unshare-net gives on Linux.
		b.WriteString("(deny sysctl-read (sysctl-name-prefix \"net.route\"))\n")
		b.WriteString("(deny system-socket (socket-domain AF_ROUTE))\n")
		b.WriteString("(deny mach-lookup (global-name-prefix \"com.apple.SystemConfiguration\") (global-name-prefix \"com.apple.network\"))\n")
	}

	b.WriteString("\n;; Never writable, regardless of workspace location.\n")
	for _, p := range []string{"/etc", "/System", "/usr", "/bin", "/sbin", "/Library/LaunchDaemons"} {
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", p)
	}
	// Credentials are readable by many tools legitimately, but an agent has no
	// reason to read SSH or cloud keys.
	b.WriteString("\n;; Credential paths are unreadable.\n")
	if home, err := os.UserHomeDir(); err == nil {
		for _, c := range []string{".ssh", ".aws", ".kube", ".gnupg", ".docker/config.json"} {
			fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", filepath.Join(home, c))
		}
	}
	return b.String()
}

// bwrapFreshOK reports whether bwrap can mount a fresh /proc and /dev in
// this environment, probing once with a trivial command.
func (s *Process) bwrapFreshOK() bool {
	s.freshOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "bwrap", "--unshare-pid", "--proc", "/proc", "--dev", "/dev",
			"--ro-bind", "/usr", "/usr", "--ro-bind", "/bin", "/bin", "--ro-bind", "/lib", "/lib",
			"--ro-bind-try", "/lib64", "/lib64", "/bin/true").CombinedOutput()
		s.freshOK = err == nil
		if err != nil && !strings.Contains(string(out), "Can't mount") {
			// Some other failure: keep the private mounts and let the real
			// command report what is wrong, rather than hiding it.
			s.freshOK = true
		}
	})
	return s.freshOK
}

func (s *Process) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	return s.wrap(ctx, cwd, s.env(), "/bin/bash", "-c", command)
}

// Shell starts a long-lived interactive bash under the same confinement as
// Command, for a person at a terminal.
func (s *Process) Shell(ctx context.Context, cwd string) *exec.Cmd {
	return hangUp(s.wrap(ctx, cwd, append(s.env(), shellEnv(s.Tier())...), shellArgv...))
}

// Backend names the mechanism: sandbox-exec or bwrap.
func (s *Process) Backend() string { return s.backend }

// wrap runs argv inside the backend's confinement.
func (s *Process) wrap(ctx context.Context, cwd string, env []string, argv ...string) *exec.Cmd {
	switch s.backend {
	case "sandbox-exec":
		profile := s.seatbeltProfile()
		// -p takes the profile inline, avoiding a temp file the command could
		// itself tamper with.
		cmd := exec.CommandContext(ctx, "sandbox-exec", append([]string{"-p", profile}, argv...)...)
		cmd.Dir = cwd
		cmd.Env = env
		return cmd

	case "bwrap":
		args := []string{
			"--die-with-parent",
			"--unshare-pid", "--unshare-ipc", "--unshare-uts",
		}
		// A private /proc and /dev when the kernel allows them. Where it does
		// not (a container that has dropped the capabilities), the host's
		// /dev is bound instead and /proc is left out: the PID namespace still
		// holds, and tools that read /proc see nothing rather than the host.
		// That is the lesser loss — the alternative was no sandboxed command
		// running at all.
		if s.bwrapFreshOK() {
			args = append(args, "--proc", "/proc", "--dev", "/dev")
		} else {
			args = append(args, "--dev-bind", "/dev", "/dev")
		}
		args = append(args,
			// Read-only system, writable workspace.
			"--ro-bind", "/usr", "/usr",
			"--ro-bind", "/bin", "/bin",
			"--ro-bind", "/lib", "/lib",
			"--ro-bind-try", "/lib64", "/lib64",
			"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
			"--ro-bind-try", "/etc/ssl", "/etc/ssl",
			// Debian links vi, awk, editor and others through /etc/alternatives.
			"--ro-bind-try", "/etc/alternatives", "/etc/alternatives",
			// /tmp first: a workspace under it is bound on top afterwards, or
			// the tmpfs would hide it and every command would fail to start.
			"--tmpfs", "/tmp",
			"--bind", s.policy.Workspace, s.policy.Workspace,
			// An empty, throwaway directory over Abhed's own state: nothing in
			// it can be read, and anything written there is gone at exit.
			"--tmpfs", filepath.Join(s.policy.Workspace, stateDir),
			"--chdir", cwd,
		)
		if !s.policy.AllowNetwork {
			args = append(args, "--unshare-net")
		}
		for _, p := range s.policy.ReadOnlyPaths {
			args = append(args, "--ro-bind-try", p, p)
		}
		// State kept outside .abhed is hidden where a bind above would show
		// it: a folder by an empty one, a file by /dev/null.
		for _, p := range s.statePaths() {
			if !s.visibleInside(p) {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				args = append(args, "--tmpfs", p)
			} else if err == nil {
				args = append(args, "--ro-bind", "/dev/null", p)
			}
		}
		args = append(args, argv...)

		cmd := exec.CommandContext(ctx, "bwrap", args...)
		cmd.Dir = cwd
		cmd.Env = env
		return cmd
	}

	// Unreachable when Available() gated correctly, but fail closed rather than
	// silently running unsandboxed.
	return exec.CommandContext(ctx, "false")
}

// statePaths returns the policy's state paths, each also as its links
// resolve, since a profile rule names the path the kernel sees.
func (s *Process) statePaths() []string {
	var out []string
	for _, p := range s.policy.StatePaths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		out = append(out, abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
			out = append(out, real)
		}
	}
	return out
}

// visibleInside reports whether p is under a path bwrap binds into the
// sandbox; anything else is not there to hide.
func (s *Process) visibleInside(p string) bool {
	for _, root := range append([]string{s.policy.Workspace}, s.policy.ReadOnlyPaths...) {
		if rel, err := filepath.Rel(root, p); err == nil && filepath.IsLocal(rel) {
			return true
		}
	}
	return false
}

// env builds a minimal environment. The agent should not inherit the operator's
// credentials by accident, so only an explicit allowlist passes through.
func (s *Process) env() []string {
	keep := []string{"PATH", "HOME", "LANG", "LC_ALL", "TERM", "TMPDIR",
		"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE",
		"NODE_PATH", "npm_config_cache", "CARGO_HOME", "RUSTUP_HOME",
		"JAVA_HOME", "PYTHONPATH", "VIRTUAL_ENV"}
	out := []string{"ABHED_SANDBOX=" + string(s.Tier()), "VIMINIT=" + vimInit}
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// vimInit reads the person's vim config, then turns off the history file: the sandbox
// refuses that write in home, and vim then waits at "Press ENTER" after :wq.
const vimInit = `if 1 | if has('nvim') | let g:abhed_rc = stdpath('config') . (filereadable(stdpath('config') . '/init.lua') ? '/init.lua' : '/init.vim')` +
	` | if filereadable(g:abhed_rc) | let $MYVIMRC = g:abhed_rc | exe 'source' fnameescape(g:abhed_rc) | endif | unlet g:abhed_rc | set shada=` +
	` | else | let g:abhed_rc = filter(['~/.vimrc', '~/.vim/vimrc', '~/.config/vim/vimrc', '~/.exrc'], 'filereadable(expand(v:val))')` +
	` | if !empty(g:abhed_rc) | let $MYVIMRC = expand(g:abhed_rc[0]) | exe 'source' fnameescape($MYVIMRC)` +
	` | elseif filereadable($VIMRUNTIME . '/defaults.vim') | exe 'source' fnameescape($VIMRUNTIME . '/defaults.vim') | endif` +
	` | unlet g:abhed_rc | endif | endif | silent! set viminfo=`

// None runs commands directly on the host.
//
// It exists so the tier is always explicit and auditable rather than implied by
// the absence of a sandbox. It refuses to be selected when the policy requires
// any real isolation.
type None struct{ policy Policy }

func NewNone(p Policy) *None { return &None{policy: p} }

func (n *None) Tier() Tier { return TierNone }

func (n *None) Available() (bool, string) { return true, "" }

func (n *None) Describe() string {
	return "NO ISOLATION — commands run directly on the host. Trusted repositories only."
}

func (n *None) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = cwd
	cmd.Env = append(HostCommandEnv(), "ABHED_SANDBOX=none")
	return cmd
}

// HostCommandEnv is the server's environment for a command run on the host,
// without what would make bash run a file first (BASH_ENV) or send a plain cd
// somewhere other than the folder named (CDPATH), which the tracker follows.
// Exported functions and shell options go too: BASH_FUNC_cd%% redefines cd,
// and SHELLOPTS or BASHOPTS change how every line is run.
func HostCommandEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !hostDropped(kv) {
			out = append(out, kv)
		}
	}
	return out
}

func hostDropped(kv string) bool {
	for _, p := range []string{"BASH_ENV=", "CDPATH=", "SHELLOPTS=", "BASHOPTS=", "BASH_FUNC_"} {
		if strings.HasPrefix(kv, p) {
			return true
		}
	}
	return false
}

// Shell starts an interactive bash directly on the host, with nothing
// between it and the machine.
func (n *None) Shell(ctx context.Context, cwd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, shellArgv[0], shellArgv[1:]...) // #nosec G204 -- a fixed argv
	cmd.Dir = cwd
	cmd.Env = append(append(hostEnv(), "ABHED_SANDBOX=none"), shellEnv(TierNone)...)
	return hangUp(cmd)
}

// Backend says there is none.
func (n *None) Backend() string { return "host" }
