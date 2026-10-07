package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

	// Whether bwrap can make the namespaces a command runs in: a binary
	// that is installed but cannot (no user namespaces, a seccomp profile
	// refusing unshare) failed every command instead of the start-up check.
	nsOnce sync.Once
	nsErr  string

	// Whether a sandboxed command is left no capabilities and no writable
	// host-root path. Probed once; empty means it is, so the tier fails closed.
	capsOnce sync.Once
	capsErr  string
}

// rootCaps reports whether Abhed runs as uid 0, where bwrap keeps the host's
// full capability set and the command stays uid 0, so both need neutralising.
func rootCaps() bool { return os.Getuid() == 0 || os.Geteuid() == 0 }

// rootWritableProc are the /proc paths a uid-0 command would otherwise write
// by owner match; core_pattern and modprobe run a program as host root.
var rootWritableProc = []string{
	"/proc/sys", "/proc/sysrq-trigger", "/proc/dynamic_debug",
	"/proc/latency_stats", "/proc/pressure", "/proc/scsi",
	"/proc/acpi", "/proc/fs", // known root-writable on x86 and with cifs; the probe backstops the rest
}

// capProbeCaps prints the five capability sets and NoNewPrivs, for any uid.
const capProbeCaps = `grep -E '^(CapInh|CapPrm|CapEff|CapAmb|CapBnd|NoNewPrivs):' /proc/self/status; echo PROBE_DONE`

// capProbeRoot also lists every writable file left under /proc (outside the
// command's own self, thread-self and pid dirs); any marks the tier unsafe, so
// a per-kernel file the covers miss refuses rather than slips through.
// Read-only: -writable is an access check.
const capProbeRoot = `grep -E '^(CapInh|CapPrm|CapEff|CapAmb|CapBnd|NoNewPrivs):' /proc/self/status; ` +
	`if command -v find >/dev/null 2>&1; then ` +
	`find /proc -xdev -writable -not -path '/proc/[0-9]*' -not -path '/proc/self/*' -not -path '/proc/thread-self/*' 2>/dev/null ` +
	`| sed 's/^/WRITABLE /'; echo PROC_SCANNED; fi; echo PROBE_DONE`

// bwrapRun runs bwrap with args, for the start-up probe; a test replaces it.
var bwrapRun = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "bwrap", args...).CombinedOutput()
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
	if s.backend == "bwrap" {
		if why := s.bwrapNamespaces(); why != "" {
			return false, why
		}
		// As root with the network on, the command shares the host's network
		// namespace and so its abstract unix sockets, where host services that
		// trust uid 0 (iscsid and the like) take commands. Refuse it.
		if rootCaps() && s.policy.AllowNetwork {
			return false, "running as root, the process tier cannot allow network access: the command would share the host's abstract sockets; use the container or vm tier"
		}
		// As root the command would otherwise read the host's block devices
		// through the bound /dev; refuse rather than take that fallback.
		if rootCaps() && !s.bwrapFreshOK() {
			return false, "running as root, bubblewrap needs a private /proc and /dev here, which the kernel refuses; use the container or vm tier"
		}
		if why := s.bwrapDropsCaps(); why != "" {
			return false, why
		}
	}
	return true, ""
}

// bwrapNamespaces is why bwrap cannot make a command's namespaces here, or
// "", probing once. The network namespace is left out: a runner that grants
// the others but not it (as GitHub's hosted one) still runs commands with the
// network on, and one that denies it fails that command with its reason.
func (s *Process) bwrapNamespaces() string {
	s.nsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := []string{"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--ro-bind", "/", "/", "/bin/true"}
		if out, err := bwrapRun(ctx, args...); err != nil {
			first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			if first == "" {
				first = err.Error()
			}
			s.nsErr = "bubblewrap (bwrap) is installed but cannot create a sandbox's namespaces here: " + first
		}
	})
	return s.nsErr
}

// rootCapArgs empty every capability set when Abhed runs as root: a user
// namespace plus --cap-drop ALL. Off root bwrap already runs unprivileged.
func rootCapArgs() []string {
	if !rootCaps() {
		return nil
	}
	return []string{"--unshare-user", "--cap-drop", "ALL"}
}

// rootProcCovers bind the writable root-owned /proc files read-only so the
// still-uid-0 command cannot use owner rights to run code as host root; they
// follow --proc, which they override. binfmt_misc is an autofs point, so a
// host mount made after start would propagate in read-write: an empty
// read-only tmpfs over it keeps register unreachable regardless.
func rootProcCovers() []string {
	if !rootCaps() {
		return nil
	}
	var args []string
	for _, p := range rootWritableProc {
		args = append(args, "--ro-bind-try", p, p)
	}
	return append(args, "--tmpfs", "/proc/sys/fs/binfmt_misc", "--remount-ro", "/proc/sys/fs/binfmt_misc")
}

// bwrapDropsCaps is why a sandboxed command would keep a capability or a
// writable host-root path here, or "", probing once. It sets up /proc exactly
// as wrap does, so a non-root user without a private /proc still passes.
func (s *Process) bwrapDropsCaps() string {
	s.capsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		probe, scanned := capProbeCaps, false
		if rootCaps() {
			probe, scanned = capProbeRoot, true
		}
		// A writable /dev (as wrap gives) when there is a fresh /proc, so the
		// probe's own redirects work; otherwise the host /dev through --ro-bind.
		args := append(rootCapArgs(), "--ro-bind", "/", "/", "--unshare-pid")
		if s.bwrapFreshOK() {
			args = append(args, "--proc", "/proc", "--dev", "/dev")
			args = append(args, rootProcCovers()...)
		}
		args = append(args, "/bin/sh", "-c", probe)
		out, err := bwrapRun(ctx, args...)
		if err != nil {
			s.capsErr = "bubblewrap (bwrap) cannot confine a command here: " + firstLine(out, err)
			return
		}
		s.capsErr = checkCapProbe(string(out), scanned)
	})
	return s.capsErr
}

// checkCapProbe fails closed unless all five capability sets and NoNewPrivs
// were reported with safe values and no /proc file was writable; requireScan
// also demands the writable-/proc enumeration ran.
func checkCapProbe(out string, requireScan bool) string {
	if !strings.Contains(out, "PROBE_DONE") {
		return "bubblewrap (bwrap) confinement probe did not finish"
	}
	if requireScan && !strings.Contains(out, "PROC_SCANNED") {
		return "bubblewrap (bwrap) could not enumerate writable /proc files"
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "WRITABLE "); ok {
			return "a sandboxed command can still write " + strings.TrimSpace(rest)
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "CapInh", "CapPrm", "CapEff", "CapAmb", "CapBnd":
			if n, perr := strconv.ParseUint(v, 16, 64); perr != nil || n != 0 {
				return "bubblewrap (bwrap) left a sandboxed command capabilities (" + k + " " + v + ")"
			}
			seen[k] = true
		case "NoNewPrivs":
			if v != "1" {
				return "a sandboxed command can gain privileges (NoNewPrivs " + v + ")"
			}
			seen[k] = true
		}
	}
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb", "CapBnd", "NoNewPrivs"} {
		if !seen[k] {
			return "bubblewrap (bwrap) confinement probe did not report " + k
		}
	}
	return ""
}

// refusedProcessCmd is a process-tier command that does not start, with the reason.
func refusedProcessCmd(format string, a ...any) *exec.Cmd {
	return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: "+format, a...)}
}

// firstLine is a command's first line of output, or its error when it printed none.
func firstLine(out []byte, err error) string {
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if first == "" && err != nil {
		return err.Error()
	}
	return first
}

func (s *Process) Describe() string {
	net := "no network"
	if s.policy.AllowNetwork {
		net = "network allowed"
	}
	// Said only when one is set: with no count of the user's processes there
	// is no limit, though max_procs asked for one.
	procs := "processes not bounded"
	if s.procLimit() > 0 {
		procs = fmt.Sprintf("at most %d more processes per command", s.policy.MaxProcs)
	}
	return fmt.Sprintf("process isolation via %s · workspace-scoped writes · %s · %s · memory, CPU and disk not bounded",
		s.backend, net, procs)
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
	for _, ws := range s.workspaces() {
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", ws)
	}
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
	for _, ws := range s.workspaces() {
		state := filepath.Join(ws, stateDir)
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", state)
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", state)
		// A stat of it succeeds, so pytest, ls -R and git pass it by; listing
		// it and reading what it holds do not.
		fmt.Fprintf(&b, "(allow file-read-metadata (subpath %q))\n", state)
	}
	// Seatbelt matches the path the kernel resolved, so a home reached
	// through a link is named both ways.
	if home, err := os.UserHomeDir(); err == nil {
		for _, st := range PathForms(filepath.Join(home, stateDir)) {
			fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", st)
			fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", st)
			// Skills are the one part of it a command may need: a skill can ship a script.
			fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", filepath.Join(st, "skills"))
		}
	}

	protected := formsOf(s.policy.WriteProtected)
	for _, p := range protected {
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", p)
	}
	// Their folders stay where they are, though what else they hold is writable.
	for _, p := range holders(s.workspaces(), protected) {
		fmt.Fprintf(&b, "(deny file-write* (literal %q))\n", p)
	}
	if s.policy.ProtectGit {
		// Every .git at any depth, and its config and hooks, in any case.
		for _, ws := range s.workspaces() {
			fmt.Fprintf(&b, "(deny file-write* (regex #\"^%s/(.+/)?%s/(%s|%s)(/|$)\"))\n", regexQuote(ws), anyCase(".git"), anyCase("config"), anyCase("hooks"))
			fmt.Fprintf(&b, "(deny file-write* (regex #\"^%s/(.+/)?%s$\"))\n", regexQuote(ws), anyCase(".git"))
		}
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

	// Last, so they win over the denies above; read, never written.
	if files := s.readableFiles(); len(files) > 0 {
		b.WriteString("\n;; Files named to be readable, such as a statusline script.\n")
		for _, f := range files {
			fmt.Fprintf(&b, "(allow file-read* (literal %q))\n", f)
		}
	}

	b.WriteString("\n;; Never writable, regardless of workspace location.\n")
	for _, p := range []string{"/etc", "/System", "/usr", "/bin", "/sbin", "/Library/LaunchDaemons"} {
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", p)
	}
	// Credentials are readable by many tools legitimately, but an agent has no
	// reason to read keys and tokens. Bubblewrap leaves home out altogether.
	b.WriteString("\n;; Credential paths are unreadable.\n")
	if home, err := os.UserHomeDir(); err == nil {
		for _, c := range HomeSecrets {
			for _, p := range PathForms(filepath.Join(home, c)) {
				fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", p)
			}
		}
	}
	return b.String()
}

// HomeSecrets are the files and folders under home that hold credentials,
// tokens or what was typed at a shell; a macOS command may read none of them.
var HomeSecrets = []string{
	// Keys and cloud credentials.
	".ssh", ".aws", ".kube", ".gnupg", ".docker/config.json", ".azure", ".oci", ".boto", ".s3cfg",
	".config/gcloud", ".config/doctl", ".config/rclone", ".mc", ".vault-token",
	".terraform.d/credentials.tfrc.json", ".terraformrc", ".config/sops", ".config/age",
	".password-store", ".config/op", "Library/Keychains",
	// Git hosts and package registries.
	".netrc", ".git-credentials", ".config/git/credentials", ".config/gh", ".config/hub",
	".config/glab-cli", ".npmrc", ".yarnrc", ".yarnrc.yml", ".config/configstore", ".pypirc",
	".gem/credentials", ".cargo/credentials", ".cargo/credentials.toml", ".m2/settings.xml",
	".gradle/gradle.properties", ".ivy2/.credentials", ".composer/auth.json",
	".config/composer/auth.json", ".nuget/NuGet/NuGet.Config",
	// Databases, model and agent tokens.
	".pgpass", ".my.cnf", ".huggingface/token", ".cache/huggingface/token",
	".config/github-copilot", ".claude", ".codex", ".config/anthropic", ".config/openai",
	// Shell and REPL history, where a pasted secret stays.
	".bash_history", ".zsh_history", ".zsh_sessions", ".python_history", ".psql_history",
	".mysql_history", ".node_repl_history", ".lesshst",
	// Browser profiles, cookies and mail.
	"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox",
	"Library/Application Support/BraveSoftware", "Library/Application Support/Microsoft Edge",
	"Library/Safari", "Library/Cookies", "Library/Mail", "Library/Messages",
}

// readableFiles are the policy's readable files that are still the files
// pinned; one swapped since is left out, so its allow goes with it.
func (s *Process) readableFiles() []string {
	var out []string
	for _, f := range s.policy.ReadableFiles {
		if f.Same() {
			out = append(out, f.Path)
		}
	}
	return out
}

// bwrapFreshOK reports whether bwrap can mount a fresh /proc and /dev in
// this environment, probing once. It uses the same capability context as the
// real command, so the probe and the command cannot disagree under root.
func (s *Process) bwrapFreshOK() bool {
	s.freshOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := append(rootCapArgs(), "--unshare-pid", "--proc", "/proc", "--dev", "/dev",
			"--ro-bind", "/usr", "/usr", "--ro-bind", "/bin", "/bin", "--ro-bind", "/lib", "/lib",
			"--ro-bind-try", "/lib64", "/lib64", "/bin/true")
		out, err := bwrapRun(ctx, args...)
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
		cmd := s.bounded(ctx, "sandbox-exec", append([]string{"-p", profile}, argv...))
		cmd.Dir = cwd
		cmd.Env = env
		return cmd

	case "bwrap":
		args := []string{
			"--die-with-parent",
			"--unshare-pid", "--unshare-ipc", "--unshare-uts",
		}
		// As root bwrap keeps the host's full capability set and the command
		// stays uid 0; empty the sets so it cannot escape the setup.
		args = append(args, rootCapArgs()...)
		// A private /proc and /dev with the writable /proc files covered after;
		// the host-/dev fallback is refused as root (it exposes block devices).
		switch {
		case s.bwrapFreshOK():
			args = append(args, "--proc", "/proc", "--dev", "/dev")
			args = append(args, rootProcCovers()...)
		case rootCaps():
			return refusedProcessCmd("running as root, bubblewrap cannot mount a private /proc and /dev here, and the host /dev would expose block devices; use the container or vm tier")
		default:
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
		// A folder holding a protected path is bound onto itself first: a
		// mount point cannot be renamed or removed, and stays writable. A
		// .git file, which names the git folder, is bound read-only.
		protected := s.protectedInside()
		// Bubblewrap can bind only what exists, so each git folder found now
		// has its config and hooks bound read-only; Seatbelt names them by pattern.
		if s.policy.ProtectGit {
			for _, ws := range s.workspaces() {
				protected = append(protected, GitProtected(ws)...)
			}
		}
		for _, p := range holders(s.workspaces(), protected) {
			if info, err := os.Lstat(p); err == nil && info.IsDir() {
				args = append(args, "--bind", p, p)
			} else if err == nil && info.Mode().IsRegular() {
				args = append(args, "--ro-bind", p, p)
			}
		}
		for _, p := range append(s.policy.ReadOnlyPaths[:len(s.policy.ReadOnlyPaths):len(s.policy.ReadOnlyPaths)], protected...) {
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
		// Bound last and read-only, so a file named readable shows even
		// where a mount above would hide its folder.
		for _, f := range s.readableFiles() {
			args = append(args, "--ro-bind", f, f)
		}
		args = append(args, argv...)

		cmd := s.bounded(ctx, "bwrap", args)
		cmd.Dir = cwd
		cmd.Env = env
		return cmd
	}

	// Unreachable when Available() gated correctly, but fail closed rather than
	// silently running unsandboxed.
	return exec.CommandContext(ctx, "false")
}

// bounded starts the backend under the process limit: a bash sets it and execs the
// backend, so it holds for all the command starts, bubblewrap's namespace included.
func (s *Process) bounded(ctx context.Context, name string, args []string) *exec.Cmd {
	n := s.procLimit()
	if n == 0 {
		return exec.CommandContext(ctx, name, args...) // #nosec G204 -- the sandbox backend with its own argv
	}
	if p, err := exec.LookPath(name); err == nil {
		name = p
	}
	wrapped := append([]string{"-c", `ulimit -u "$1" && shift && exec "$@"`, "abhed-limit", strconv.FormatUint(n, 10), name}, args...)
	return exec.CommandContext(ctx, "/bin/bash", wrapped...) // #nosec G204 -- fixed script; the argv is passed as "$@"
}

// countUserProcesses counts the user's processes; a test replaces it.
var countUserProcesses = userProcesses

// procLimit is what the user runs now plus MaxProcs, as the kernel counts all the
// user's processes, so concurrent commands and the desktop share that headroom;
// zero (no bound asked, root, or no count) leaves the limit alone.
func (s *Process) procLimit() uint64 {
	if s.policy.MaxProcs <= 0 || os.Getuid() == 0 {
		return 0
	}
	running, ok := countUserProcesses()
	if !ok {
		return 0
	}
	limit := uint64(running) + uint64(s.policy.MaxProcs) // #nosec G115 -- both are positive
	// Never above the limit in force, which the operator may have lowered.
	if soft, ok := procSoftLimit(); ok && soft < limit {
		limit = soft
	}
	return limit
}

// workspaces are the workspace as given and with its links resolved.
func (s *Process) workspaces() []string { return PathForms(s.policy.Workspace) }

// protectedInside are the write-protected paths, in both forms, that lie in
// the workspace; a mount for one outside it would only show it to the command.
func (s *Process) protectedInside() []string {
	var out []string
	for _, p := range formsOf(s.policy.WriteProtected) {
		if insideAny(p, s.workspaces()) {
			out = append(out, p)
		}
	}
	return out
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
// ABHED_TRUST_WORKSPACE goes, so a command's own abhed run trusts nothing.
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
	for _, p := range []string{"BASH_ENV=", "CDPATH=", "SHELLOPTS=", "BASHOPTS=", "BASH_FUNC_", "ABHED_TRUST_WORKSPACE="} {
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

// Bounds on the walk for git folders: a deeper or wider tree is not walked
// further, and what lies past the bound is not protected.
const (
	gitWalkDepth   = 6
	gitWalkEntries = 20000
)

// makeEmpty makes an empty folder or file at p, never replacing one.
func makeEmpty(p string, dir bool) {
	if dir {
		_ = os.Mkdir(p, 0o755) // #nosec G301 -- git's own mode for hooks
		return
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil { // #nosec G302 G304 -- git's own mode for config, in a git folder the walk found
		_ = f.Close()
	}
}

// GitProtected are the paths in ws that a git command runs programs from:
// each git folder's config and hooks, made empty where missing, and each .git
// file (a worktree's or a submodule's link to its git folder), at most
// gitWalkDepth folders down.
func GitProtected(ws string) []string {
	var out []string
	seen := 0
	_ = filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a folder that cannot be read is passed over, and the walk goes on
		}
		if seen++; seen > gitWalkEntries {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(ws, p)
		depth := strings.Count(rel, string(filepath.Separator))
		if strings.EqualFold(d.Name(), ".git") {
			if d.IsDir() {
				for _, f := range []string{"config", "hooks"} {
					q := filepath.Join(p, f)
					// A missing one is made empty as the person, so it can be bound read-only.
					if _, err := os.Lstat(q); errors.Is(err, fs.ErrNotExist) {
						makeEmpty(q, f == "hooks")
					}
					if _, err := os.Lstat(q); err == nil {
						out = append(out, q)
					}
				}
				return filepath.SkipDir
			}
			if d.Type().IsRegular() {
				out = append(out, p)
			}
			return nil
		}
		if d.IsDir() && (depth >= gitWalkDepth || d.Name() == "node_modules" || strings.EqualFold(d.Name(), stateDir)) {
			return filepath.SkipDir
		}
		return nil
	})
	return out
}
