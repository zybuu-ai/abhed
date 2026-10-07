package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/internal/fence/landlock"
	"github.com/zybuu-ai/abhed/internal/fence/probe"
	"github.com/zybuu-ai/abhed/internal/fence/seccomp"
)

// Fence is the fence tier, a preview on Linux: Abhed re-executes itself as a
// launcher for every command, which joins the call's own cgroup, confines
// itself with Landlock and a seccomp filter, reports what it applied, and
// only once Abhed has recorded the launch runs the command in its place.
// Abhed itself stays outside the fence, as on every tier.
//
// It is chosen with sandbox.tier, never picked by Select's walk, and it never
// falls back: a host the probe does not qualify, or a policy it cannot hold,
// refuses the tier, naming why.
type Fence struct {
	policy Policy

	once sync.Once
	ok   bool
	why  string
	// report is this run's probe; a pass is never kept past the process.
	report probe.Report
	// id names the session's cgroup; tmp is the commands' private temp.
	id  string
	tmp string
	// host is the platform's state once qualified: the cgroups on Linux.
	host *fenceHost
	seq  atomic.Uint64

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error

	// record is the session's record, for a launch that carries none and
	// for state found planted at Close; nil outside a session.
	recMu  sync.Mutex
	record func(event string, payload map[string]any) error
	// planted is why the session's commands are refused once a .abhed
	// appeared in the workspace; plantMu orders the checks.
	plantMu sync.Mutex
	planted atomic.Pointer[string]
}

// NewFence builds the fence for p; Available qualifies it.
func NewFence(p Policy) *Fence { return &Fence{policy: p} }

func (f *Fence) Tier() Tier { return TierFence }

// Backend names the mechanisms, for a terminal's banner.
func (f *Fence) Backend() string { return "landlock+seccomp+cgroup" }

// Available qualifies the fence once: the policy, then this host by the
// probe, then the session's cgroup. Any failure refuses the tier.
func (f *Fence) Available() (bool, string) {
	f.once.Do(func() { f.ok, f.why = f.qualify() })
	return f.ok, f.why
}

// SetRecord gives the fence the session's record: a launch whose caller
// passes none, such as one the harness makes itself, is recorded there as
// the harness's, and so is state found planted when the session closes.
func (f *Fence) SetRecord(rec func(event string, payload map[string]any) error) {
	f.recMu.Lock()
	defer f.recMu.Unlock()
	f.record = rec
}

func (f *Fence) sessionRecord() func(string, map[string]any) error {
	f.recMu.Lock()
	defer f.recMu.Unlock()
	return f.record
}

// Close ends what the session's commands left running and removes their
// cgroup and private temp folder. Commands built after it do not start. A
// .abhed a command left in the workspace is taken out and reported, and
// Close fails when it finds one or cannot list the workspace.
func (f *Fence) Close() error {
	f.closed.Store(true)
	// A fence never qualified is not qualified now; one qualifying finishes first.
	f.once.Do(func() { f.why = "the session's fence was closed" })
	f.closeOnce.Do(func() {
		var errs []error
		if f.host != nil {
			errs = append(errs, f.host.close())
			// Checked once nothing of the session's is left running.
			errs = append(errs, f.checkPlanted(f.sessionRecord(), "", "at_close"))
		}
		if f.tmp != "" {
			errs = append(errs, os.RemoveAll(f.tmp))
		}
		f.closeErr = errors.Join(errs...)
	})
	return f.closeErr
}

// Close releases what a backend holds for its session: the fence's cgroup
// and private temp. Other backends hold nothing.
func Close(sb Sandbox) error {
	if c, ok := sb.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// Report is the probe's report, once Available has run.
func (f *Fence) Report() probe.Report { return f.report }

func (f *Fence) qualify() (bool, string) {
	if why := f.policyRefusal(); why != "" {
		return false, why
	}
	if err := f.spec().Validate(); err != nil {
		return false, err.Error()
	}
	return f.qualifyHost(context.Background())
}

// policyRefusal says why the policy asks what the fence cannot hold, or "".
// Landlock grants a folder and everything under it, so a read-only area
// inside the writable workspace cannot be kept.
func (f *Fence) policyRefusal() string {
	switch {
	case f.policy.Workspace == "" || !filepath.IsAbs(f.policy.Workspace):
		return "the workspace must be an absolute path"
	case len(f.policy.WriteProtected) > 0 || f.policy.ProtectGit:
		return "this surface keeps paths inside the workspace read-only (its editor settings or git's config and hooks), " +
			"and the fence cannot carve a read-only area out of the writable workspace; use the command line, or another tier"
	case f.policy.CPUPercent < 0:
		return "fence.cpu_percent is negative"
	case f.policy.Egress != nil:
		// Landlock limits TCP connects by port, not address, and seccomp
		// cannot read the address a socket connects to: a command allowed the
		// proxy's port could reach that port on any host.
		return "sandbox.network is allowlist, and the fence cannot keep a command's sockets to the egress proxy alone " +
			"(Landlock limits TCP by port, not by address, and seccomp cannot read the address); " +
			"leave sandbox.tier unset to use the process tier, which enforces it, or turn the allowlist off"
	}
	return ""
}

// Describe states what the fence covers and what it does not.
func (f *Fence) Describe() string {
	abi := f.report.LandlockABI
	net := "network off: every socket refused (Landlock refuses TCP as well)"
	if f.policy.AllowNetwork {
		net = "network on: the host's network, unfiltered; unix sockets refused"
	}
	signals := "Landlock scopes signals and abstract sockets"
	if abi < 6 {
		signals = "commands can signal your other processes, Abhed too by one of its thread ids (Landlock ABI 6, Linux 6.12, scopes them)"
	}
	return fmt.Sprintf("fence (preview, Linux) · each command under Landlock ABI %d: reads and runs system folders, writes the workspace and a private temp folder, "+
		"no other terminal, Abhed's state, record and secrets unreachable; a .abhed a command makes at the top of the workspace, in any case, "+
		"is renamed out of the way once the command ends and moved to ~/.abhed/quarantine where it can be, the session's commands still running are ended and its further commands are refused, "+
		"as they are when the workspace can no longer be listed · seccomp %s: no ptrace, namespaces, mounts, bpf, keyrings, unix sockets "+
		"or signals to Abhed's process id · %s · cgroup per call: %s · not covered: Abhed itself and its in-process tools (file, web, MCP), network filtering, "+
		"%s, other processes' command lines in /proc, a hard link to state made before the session, "+
		"a file a command writes into the workspace's .abhed while it runs and until the check that follows it (Abhed can read it then), "+
		"a .abhed left after an unclean exit (nothing is moved), a .abhed in a subfolder or an added folder (not checked)",
		abi, seccomp.Profile, net, f.limitsText(), signals)
}

// limitsText words the cgroup limits.
func (f *Fence) limitsText() string {
	var parts []string
	if f.policy.MaxMemoryMB > 0 {
		parts = append(parts, fmt.Sprintf("memory %d MB, no swap", f.policy.MaxMemoryMB))
	} else {
		parts = append(parts, "memory not bounded")
	}
	if f.policy.MaxProcs > 0 {
		parts = append(parts, fmt.Sprintf("at most %d processes and threads", f.policy.MaxProcs))
	} else {
		parts = append(parts, "processes not bounded")
	}
	if f.policy.CPUPercent > 0 {
		parts = append(parts, fmt.Sprintf("CPU %d%% of one core", f.policy.CPUPercent))
	} else {
		parts = append(parts, "CPU not bounded")
	}
	return strings.Join(parts, ", ") + ", what is left running ended with the command"
}

func (f *Fence) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	return f.wrap(ctx, cwd, f.env(), "/bin/bash", "-c", command)
}

// Shell starts an interactive bash under the same fence as Command.
func (f *Fence) Shell(ctx context.Context, cwd string) *exec.Cmd {
	return hangUp(f.wrap(ctx, cwd, append(f.env(), shellEnv(f.Tier())...), shellArgv...))
}

// refusedCmd is a command that does not start, with the reason.
func refusedCmd(format string, a ...any) *exec.Cmd {
	return &exec.Cmd{Err: fmt.Errorf("fence: the command was not run: "+format, a...)}
}

// fenceSystem are the folders a command reads and runs programs from.
var fenceSystem = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/etc"}

// fenceDevices are the device files a command may open. No pty: a command
// keeps the terminal it was given through its descriptors, and /dev/tty
// reaches only its own, so the user's other terminals are out of reach.
var fenceDevices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"}

// spec is what every command may reach. It is built afresh for each command,
// so a protected path that appeared since refuses the next one.
func (f *Fence) spec() landlock.Spec {
	s := landlock.Spec{
		Exec:    absAll(append(append([]string{}, fenceSystem...), f.policy.ReadOnlyPaths...)),
		Read:    []string{"/proc", "/sys"},
		Devices: fenceDevices,
		Write:   absAll([]string{f.policy.Workspace}),
		Deny:    f.denied(),
		DenyTCP: !f.policy.AllowNetwork,
	}
	s.Exec = append(s.Exec, (&Process{policy: f.policy}).readableFiles()...)
	// /etc/resolv.conf is often a link into /run, which is not granted.
	if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && !strings.HasPrefix(real, "/etc/") {
		s.Read = append(s.Read, real)
	}
	if f.tmp != "" {
		s.Write = append(s.Write, f.tmp)
	}
	return s
}

// denied are the paths no command may reach: home's .abhed, the state the
// configuration keeps elsewhere, and the workspace's .abhed in any case
// when it exists, each as written and resolved. Landlock is an allowlist, so these only
// check that no grant covers them.
func (f *Fence) denied() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, PathForms(filepath.Join(home, stateDir))...)
	}
	// A state path that does not exist holds nothing yet, such as the
	// workspace's users file a command-line run never makes; once it does,
	// the next command is refused. sandboxconfig refuses one a command could
	// create in the workspace.
	for _, p := range (&Process{policy: f.policy}).statePaths() {
		cands := []string{p}
		if strings.HasPrefix(filepath.Base(p), "secrets") {
			cands = append(cands, filepath.Join(filepath.Dir(p), "secrets.d"))
		}
		for _, c := range cands {
			if _, err := os.Lstat(c); err == nil {
				out = append(out, c)
			}
		}
	}
	// A workspace that cannot be listed refuses the command before this.
	ws, _ := stateEntries(f.policy.Workspace)
	for _, ws := range ws {
		out = append(out, PathForms(ws)...)
	}
	return absAll(out)
}

// stateEntries are the workspace's entries named .abhed in any case: where
// Abhed keeps its own state, on a filesystem that folds case as well. Each
// spelling is also looked up by name, which needs only search permission,
// so a workspace that cannot be listed still shows one there. A listing
// or a lookup that fails is returned as the error: what it hides cannot be
// ruled out.
func stateEntries(workspace string) ([]string, error) {
	var out []string
	var seen []os.FileInfo
	// A lookup that fails other than by absence, such as a workspace without
	// search permission, hides what it names: it fails the listing too.
	var lookErr error
	add := func(p string, listed bool) {
		info, err := os.Lstat(p)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				if lookErr == nil {
					lookErr = err
				}
				// The listing showed it, so it is there, whatever its file.
				if listed {
					out = append(out, p)
				}
			}
			return
		}
		for _, s := range seen {
			if os.SameFile(s, info) {
				return
			}
		}
		seen = append(seen, info)
		out = append(out, p)
	}
	entries, lerr := os.ReadDir(workspace)
	for _, e := range entries {
		if strings.EqualFold(e.Name(), stateDir) {
			add(filepath.Join(workspace, e.Name()), true)
		}
	}
	for _, name := range everySpelling(stateDir) {
		add(filepath.Join(workspace, name), false)
	}
	return out, errors.Join(lerr, lookErr)
}

// everySpelling is every spelling of name's letters in either case.
func everySpelling(name string) []string {
	out := []string{""}
	for _, r := range name {
		lo, up := strings.ToLower(string(r)), strings.ToUpper(string(r))
		next := make([]string, 0, 2*len(out))
		for _, s := range out {
			next = append(next, s+lo)
			if up != lo {
				next = append(next, s+up)
			}
		}
		out = next
	}
	return out
}

// EvFenceStatePlanted is the event for a .abhed a fenced command made, or a
// workspace that can no longer be listed to look for one.
const EvFenceStatePlanted = "fence.state_planted"

// plantedPrefix begins the name a planted .abhed is renamed to in the
// workspace, which no part of Abhed reads.
const plantedPrefix = ".abhed-planted-"

// What became of a planted entry, as fence.state_planted records it.
const (
	plantMoved     = "moved"
	plantRenamed   = "renamed_in_place"
	plantRemoved   = "removed"
	plantRemaining = "still_present"
)

// checkPlanted looks for a .abhed in the workspace, which none can hold when
// the fence qualifies, so one there now was made by a command (or beside the
// session). Each is first renamed within the workspace to a name Abhed never
// reads, which cannot fail across filesystems, then moved to
// ~/.abhed/quarantine where it can be. A workspace that cannot be listed
// counts as holding one. Either way it is reported to rec, and the session's
// commands are refused from then on. callID is the command just ended, if
// any, and when says which check found it. It reports why, or nil when the
// workspace was listed and held none.
func (f *Fence) checkPlanted(rec func(string, map[string]any) error, callID, when string) error {
	f.plantMu.Lock()
	defer f.plantMu.Unlock()
	found, lerr := stateEntries(f.policy.Workspace)
	if len(found) == 0 && lerr == nil {
		return nil
	}
	var entries []map[string]any
	var done, left []string
	for _, p := range found {
		m := f.takeOut(p)
		entries = append(entries, m)
		name := filepath.Base(p)
		switch m["outcome"] {
		case plantMoved:
			done = append(done, name+" was moved to "+m["moved_to"].(string))
		case plantRenamed:
			done = append(done, name+" was renamed in place to "+filepath.Base(m["renamed_to"].(string))+" (it could not be moved out: "+m["reason"].(string)+")")
		case plantRemoved:
			done = append(done, name+" was removed")
		default:
			left = append(left, name)
		}
	}
	var parts []string
	if len(found) > 0 {
		parts = append(parts, "a "+names(found)+" appeared in the workspace, where Abhed keeps its own state")
	}
	parts = append(parts, done...)
	if len(left) > 0 {
		parts = append(parts, strings.Join(left, ", ")+" is still in the workspace and could not be taken out; remove it before Abhed runs there again")
	}
	if lerr != nil {
		parts = append(parts, "the workspace cannot be listed ("+lerr.Error()+"), so a .abhed in it cannot be ruled out; "+
			"restore its permissions and check it before Abhed runs there again")
	}
	why := strings.Join(parts, "; ") + "; this session's fence runs no further command"
	f.planted.Store(&why)
	// What the session's commands still run is ended too: one may be the
	// planter, or read what was planted. At close nothing is left running.
	var killErr error
	killed := false
	if f.host != nil && when != "at_close" {
		killErr = f.host.killAll()
		killed = killErr == nil
	}
	if rec != nil {
		pay := map[string]any{"call_id": callID, "found": when, "sandbox": f.id, "workspace": f.policy.Workspace,
			"entries": entries, "reason": why, "still_present": len(left) > 0 || lerr != nil, "session_killed": killed}
		if lerr != nil {
			pay["listing_error"] = lerr.Error()
		}
		if killErr != nil {
			pay["kill_error"] = killErr.Error()
		}
		_ = rec(EvFenceStatePlanted, pay)
	}
	return errors.New("fence: " + why)
}

func names(paths []string) string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return strings.Join(out, ", ")
}

// takeOut makes the planted entry p inert and says how: renamed within the
// workspace, then moved to quarantine where it can be. Where the rename
// fails, the entry is removed as a last resort, else it is still present.
func (f *Fence) takeOut(p string) map[string]any {
	m := map[string]any{"path": p}
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	inert := filepath.Join(filepath.Dir(p), plantedPrefix+f.id+"-"+hex.EncodeToString(raw[:]))
	if err := os.Rename(p, inert); err != nil {
		restoreOwnerAccess(p)
		if rerr := os.RemoveAll(p); rerr == nil {
			m["outcome"], m["reason"] = plantRemoved, err.Error()
		} else {
			m["outcome"], m["error"] = plantRemaining, errors.Join(err, rerr).Error()
		}
		return m
	}
	m["renamed_to"] = inert
	// A folder the command left without write permission cannot be moved
	// to another parent, nor later read or removed.
	restoreOwnerAccess(inert)
	if dest, err := quarantine(inert, filepath.Base(p), f.id); err == nil {
		m["outcome"], m["moved_to"] = plantMoved, dest
	} else {
		m["outcome"], m["reason"] = plantRenamed, err.Error()
	}
	return m
}

// quarantine moves p, a planted entry first named name, into a folder of its
// own under ~/.abhed/quarantine, which nothing reads as state, and returns
// where it went.
func quarantine(p, name, id string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, stateDir, QuarantineDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "fence-"+id+"-")
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name)
	if err := os.Rename(p, dest); err != nil {
		_ = os.Remove(dir)
		return "", err
	}
	return dest, nil
}

// QuarantineDir is the folder under ~/.abhed that holds what the fence took
// out of workspaces. It is not state: the state walk passes over it.
const QuarantineDir = "quarantine"

// absAll makes each path absolute and drops those that cannot be.
func absAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// fenceKeep is the environment a command keeps, as on the process tier,
// less the Go cache, which the private temp holds instead.
var fenceKeep = []string{"PATH", "LANG", "LC_ALL", "TERM",
	"GOPATH", "GOROOT", "GOMODCACHE", "NODE_PATH", "CARGO_HOME", "RUSTUP_HOME",
	"JAVA_HOME", "PYTHONPATH", "VIRTUAL_ENV"}

// env is an allowlist, as on the process tier. Home is unreachable, so HOME,
// TMPDIR and the caches point into the session's private temp.
func (f *Fence) env() []string {
	out := []string{"ABHED_SANDBOX=" + string(f.Tier()), "VIMINIT=" + vimInit}
	for _, k := range fenceKeep {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	if f.tmp != "" {
		out = append(out, "TMPDIR="+f.tmp, "HOME="+filepath.Join(f.tmp, "home"),
			"XDG_CACHE_HOME="+filepath.Join(f.tmp, "cache"), "npm_config_cache="+filepath.Join(f.tmp, "npm"))
	}
	return out
}

// leafName is a cgroup name for the call: its id where it is one, made
// unique, since a call can run more than one command.
func (f *Fence) leafName(callID string) string {
	var b strings.Builder
	for _, r := range callID {
		if r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	name := strings.TrimLeft(b.String(), ".")
	if name == "" {
		name = "cmd"
	}
	return fmt.Sprintf("%s-%d", name, f.seq.Add(1))
}

// Qualification is the fence.qualified event's payload: the probe's report
// and what the session's commands get.
func (f *Fence) Qualification() map[string]any {
	out := map[string]any{
		"tier":     string(TierFence),
		"preview":  true,
		"probe":    f.report,
		"sandbox":  f.id,
		"network":  f.policy.AllowNetwork,
		"seccomp":  seccomp.Command(f.policy.AllowNetwork, os.Getpid(), processGroup()).Describe(),
		"limits":   f.limitsText(),
		"describe": f.Describe(),
	}
	if f.host != nil {
		out["cgroup"] = f.host.path()
	}
	return out
}
