package sandbox

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
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/internal/fence/landlock"
	"github.com/zybuu-ai/abhed/internal/fence/mountns"
	"github.com/zybuu-ai/abhed/internal/fence/probe"
	"github.com/zybuu-ai/abhed/internal/fence/seccomp"
)

// Fence is the fence tier, a preview on Linux: Abhed re-executes itself as a
// launcher for every command, which joins the call's own cgroup, confines
// itself with Landlock and a seccomp filter, reports what it applied, and only
// once Abhed has recorded the launch runs the command in its place.
// Chosen only by sandbox.tier; it never falls back, it refuses naming why.
type Fence struct {
	policy Policy

	once sync.Once
	ok   bool
	why  string
	// report is this run's probe; a pass is never kept past the process.
	report probe.Report
	// mounts gives each command a mount namespace: protected paths read-only,
	// .abhed hidden. Without it protected paths are refused.
	mounts bool
	// stateID is the .abhed each namespace covers; stateHeld is whether it held
	// anything at start, madeState whether the fence made it.
	stateID   *folderID
	stateHeld bool
	madeState bool
	// stateLock is shared by every fence on the workspace while it lasts.
	stateLock *os.File
	// eg is the session's egress proxy under the allowlist.
	eg fenceEgress
	// markPath is the session's mark under ~/.abhed/fence, removed at a
	// clean close; watchStop ends the landlock_only check between commands.
	markPath  string
	watchStop chan struct{}
	// aliases are the other mounts of the workspace's filesystem, which each
	// command's namespace covers as it covers the workspace.
	aliases []string
	// unreachableAliases lie behind another user's folder no command can search.
	unreachableAliases []string
	// id names the session's cgroup; tmp is the commands' private temp.
	id  string
	tmp string
	// host is the platform's state once qualified: the cgroups on Linux.
	host *fenceHost
	seq  atomic.Uint64
	// git is what the look for git folders keeps between commands.
	git gitMemory

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

// SetRecord gives the fence the session's record: a launch whose caller passes
// none, such as one the harness makes itself, is recorded there as the
// harness's, and so is state found planted when the session closes.
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

// Close ends what the session's commands left running and removes their cgroup
// and private temp folder. A planted .abhed fails Close; the last fence to
// close removes the .abhed it mounted over, so others' mounts stay attached.
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
		f.removeMadeState()
		errs = append(errs, f.eg.close())
		if f.watchStop != nil {
			close(f.watchStop)
		}
		// Only after the last check: a mark left says that check never ran.
		removeMark(f.markPath)
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
	early := f.spec()
	// State inside the workspace is judged again once the mode is known:
	// the mount mode hides it.
	if !f.policy.fenceNoMounts {
		ws := PathForms(f.policy.Workspace)
		early.Deny = slices.DeleteFunc(early.Deny, func(d string) bool { return insideAny(d, ws) })
	}
	if err := early.Validate(); err != nil {
		return false, err.Error()
	}
	return f.qualifyHost(context.Background())
}

// policyRefusal says why the policy asks what the fence cannot hold, or "".
func (f *Fence) policyRefusal() string {
	switch {
	case f.policy.Workspace == "" || !filepath.IsAbs(f.policy.Workspace):
		return "the workspace must be an absolute path"
	case f.policy.CPUPercent < 0:
		return "fence.cpu_percent is negative"
	}
	return ""
}

// egressRefusal says why this mode cannot hold the allowlist, or "": Landlock
// alone limits TCP by port, not address, so any host is reachable.
func (f *Fence) egressModeRefusal() string {
	if f.policy.Egress == nil || f.mounts || f.policy.FenceEgressPortOnly {
		return ""
	}
	return "sandbox.network is allowlist, and the fence's landlock_only mode cannot keep a command's sockets to the egress proxy alone " +
		"(no network namespace here; Landlock limits TCP by port, not by address); the managed fence.egress_port_only accepts that, " +
		"or leave sandbox.tier unset to use the process tier"
}

// The fence's modes, as fence.qualified and process.launched record them.
const (
	// FenceModeMounts gives each command a user and mount namespace of its own,
	// with protected paths bound read-only and the workspace's .abhed covered
	// by an empty tmpfs, before Landlock and seccomp.
	FenceModeMounts = "mount_namespace"
	// FenceModeLandlock confines each command with Landlock and seccomp
	// alone, where the host gives an ordinary user no user namespace.
	FenceModeLandlock = "landlock_only"
)

// Mode is the fence's mode once qualified.
func (f *Fence) Mode() string {
	if f.mounts {
		return FenceModeMounts
	}
	return FenceModeLandlock
}

// needsMounts is whether paths inside the workspace must stay read-only, which
// needs a mount namespace: Landlock grants a folder whole.
func (f *Fence) needsMounts() bool {
	return len(f.policy.WriteProtected) > 0 || f.policy.ProtectGit && f.policy.GitRequired
}

// Describe states what the fence covers and what it does not.
func (f *Fence) Describe() string {
	abi := f.report.LandlockABI
	net := "network off: every socket refused (Landlock refuses TCP as well)"
	if f.policy.AllowNetwork {
		net = "network on: the host's network, unfiltered; unix sockets refused"
	}
	switch {
	case f.policy.Egress != nil && f.mounts:
		net = "network: the egress proxy alone, through a relay in each command's network namespace, inet TCP streams only"
	case f.policy.Egress != nil:
		net = "network: TCP to the egress proxy's port alone, on any address (no network namespace: Landlock filters ports only), inet TCP streams only"
	}
	signals := "Landlock scopes signals and abstract sockets"
	switch {
	case f.mounts:
		signals = "a PID namespace and /proc of the command's own: it signals and sees only its own processes"
	case abi < 6:
		signals = "commands can signal your other processes, Abhed too by one of its thread ids (Landlock ABI 6, Linux 6.12, scopes them), and read other processes' command lines in /proc"
	default:
		signals += "; other processes' command lines in /proc are readable"
	}
	mode := "mode landlock_only: no mount namespace; a .abhed a command makes at the top of the workspace, in any case, " +
		"is renamed out of the way once the command ends and moved to ~/.abhed/quarantine where it can be, the session's commands still running are ended and its further commands are refused, " +
		"as they are when the workspace can no longer be listed"
	planted := "a file a command writes into the workspace's .abhed while it runs and until the check that follows it (Abhed can read it then), " +
		"a .abhed left after an unclean exit (nothing is moved), a .abhed in a subfolder or an added folder (not checked)"
	if f.mounts {
		mode = "mode mount_namespace: each command in a user and mount namespace of its own, the workspace's .abhed under an empty tmpfs that is gone when the command ends; " +
			"where the workspace has none, the fence makes an empty .abhed to mount over and leaves it in place, and one empty when the session started must stay empty, " +
			"or what is found in it is taken out, the folder left, and the session ends as below"
		if f.needsMounts() || f.policy.ProtectGit {
			mode += ", the surface's protected paths (git's config and hooks, its editor settings) bound read-only, and a file in a read-only folder with a name outside it refused"
		}
		mode += ", the same done at every other mount of the workspace's filesystem (such as /sysroot on an ostree host, or a bind mount), or the command refused where it cannot be" +
			" (a FUSE, bindfs, overlay or NFS view of the workspace that exists before the session is not found; commands cannot make one)" +
			"; one behind another user's folder you cannot search is left, and stays out of reach only while that folder stays closed: opened during a command, it lets that command through, and the next command covers it"
		mode += "; another spelling of .abhed a command makes is still taken out and ends the session"
		planted = "a .abhed in a subfolder or an added folder (not covered)"
	} else if f.policy.ProtectGit {
		planted += ", git's config and hooks (not held read-only without a mount namespace: an \"always allow\" for a git command is asked again once they change; " +
			"sandbox.protect_git: true refuses this mode instead)"
	}
	return fmt.Sprintf("fence (preview, Linux) · each command under Landlock ABI %d: reads and runs system folders, ~/.abhed/skills and skills.dirs, writes the workspace and a private temp folder, "+
		"no other terminal, Abhed's state, record and secrets unreachable · %s · seccomp %s: no ptrace, namespaces, mounts, bpf, keyrings, unix sockets "+
		"or signals to Abhed's process id · %s · cgroup per call: %s · not covered: Abhed itself and its in-process tools (file, web, MCP), network filtering but the allowlist, "+
		"%s, a hard link to state made before the session, %s",
		abi, mode, seccomp.Profile, net, f.limitsText(), signals, planted)
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

// fenceDevices are the devices a command may open. No pty: /dev/tty reaches
// only its own terminal, never the user's others.
var fenceDevices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"}

// spec is what every command may reach. It is built afresh for each command,
// so a protected path that appeared since refuses the next one.
func (f *Fence) spec() landlock.Spec {
	deny := f.denied()
	system := fenceSystem
	if os.Geteuid() == 0 {
		// Root keeps its uid, so what only root may read under /etc is not granted.
		system = append(slices.DeleteFunc(slices.Clone(fenceSystem), func(p string) bool { return p == "/etc" }), rootEtcGrants("/etc")...)
	}
	s := landlock.Spec{
		Exec:    absAll(append(append([]string{}, system...), f.policy.ReadOnlyPaths...)),
		Read:    []string{"/proc", "/sys"},
		Devices: fenceDevices,
		Write:   absAll([]string{f.policy.Workspace}),
		Deny:    deny,
		DenyTCP: !f.policy.AllowNetwork,
	}
	s.Exec = append(s.Exec, (&Process{policy: f.policy}).readableFiles()...)
	// A skill can ship a script: the skills read and run, never written.
	for _, d := range f.skillDirs() {
		s.Exec = append(s.Exec, d)
		s.Within = append(s.Within, d)
	}
	granted, _ := f.configuredSkills(deny)
	s.Exec = append(s.Exec, granted...)
	// /etc/resolv.conf is often a link into /run, which is not granted.
	if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && !strings.HasPrefix(real, "/etc/") {
		s.Read = append(s.Read, real)
	}
	if f.tmp != "" {
		s.Write = append(s.Write, f.tmp)
	}
	return s
}

// skillDirs are ~/.abhed/skills, as written and resolved, where it is a folder.
func (f *Fence) skillDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, st := range PathForms(filepath.Join(home, stateDir)) {
		d := filepath.Join(st, "skills")
		if info, err := os.Lstat(d); err == nil && info.IsDir() && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// configuredSkills splits skills.dirs into folders a command may run and those
// left out: inside Abhed's state or the workspace, or not a folder.
func (f *Fence) configuredSkills(deny []string) (granted, left []string) {
	home := f.skillDirs()
	ws := PathForms(f.policy.Workspace)
	overlaps := func(p string) bool {
		return insideAny(p, ws) || slices.ContainsFunc(deny, func(x string) bool {
			return insideAny(p, []string{x}) || insideAny(x, []string{p})
		})
	}
	for _, d := range f.policy.SkillDirs {
		forms := PathForms(d)
		// Inside ~/.abhed/skills, it is granted already.
		if len(forms) > 0 && !slices.ContainsFunc(forms, func(p string) bool { return !insideAny(p, home) }) {
			continue
		}
		if len(forms) == 0 || slices.ContainsFunc(forms, overlaps) {
			left = append(left, d)
			continue
		}
		var dirs []string
		for _, p := range forms {
			if info, err := os.Lstat(p); err == nil && info.IsDir() && !slices.Contains(granted, p) {
				dirs = append(dirs, p)
			}
		}
		if len(dirs) == 0 {
			left = append(left, d)
		}
		granted = append(granted, dirs...)
	}
	return granted, left
}

// denied are the state paths no command may reach, as written and resolved;
// Landlock is an allowlist, so they check that no grant covers them.
func (f *Fence) denied() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, PathForms(filepath.Join(home, stateDir))...)
	}
	// A missing state path holds nothing yet; once it exists, the next command
	// is refused.
	for _, p := range (&Process{policy: f.policy}).statePaths() {
		cands := []string{p}
		if strings.HasPrefix(filepath.Base(p), "secrets") {
			cands = append(cands, filepath.Join(filepath.Dir(p), "secrets.d"))
		}
		for _, c := range cands {
			if _, err := os.Lstat(c); err != nil {
				continue
			}
			// The command's own mounts hide it.
			if f.mounts && insideAny(c, PathForms(f.policy.Workspace)) {
				continue
			}
			out = append(out, c)
		}
	}
	// A workspace that cannot be listed refuses the command before this.
	ws, _ := stateEntries(f.policy.Workspace)
	for _, ws := range ws {
		if f.isStateMount(ws) {
			continue
		}
		out = append(out, PathForms(ws)...)
	}
	return absAll(out)
}

// isStateMount is whether p is the same .abhed (name, inode, birth time) the
// fence qualified with, still empty if it was: else it was planted.
func (f *Fence) isStateMount(p string) bool {
	if !f.mounts || f.stateID == nil || filepath.Base(p) != stateDir {
		return false
	}
	id, err := folderIdentity(p)
	if err != nil || !id.dir || !id.same(*f.stateID) {
		return false
	}
	if f.stateHeld {
		return true
	}
	holds, err := folderHolds(p, id)
	return err == nil && !holds
}

// removeMadeState removes the .abhed a fence made and this one found empty, if still empty,
// the same folder, and no other fence holds the lock.
func (f *Fence) removeMadeState() {
	if f.stateLock == nil {
		return
	}
	defer func() { _ = f.stateLock.Close() }()
	if f.stateHeld || f.stateID == nil || !lastUser(f.stateLock) {
		return
	}
	p := filepath.Join(f.policy.Workspace, stateDir)
	// Only a folder a fence made: an empty .abhed that was there before stays.
	if id, err := folderIdentity(p); err == nil && id.dir && id.same(*f.stateID) && madeNoted(f.stateLock, id) {
		if holds, err := folderHolds(p, id); err == nil && !holds {
			_ = os.Remove(p)
		}
	}
}

// prepareStateMount finds, or makes empty, the .abhed each namespace covers: a
// tmpfs needs a folder. A non-folder refuses the fence.
func (f *Fence) prepareStateMount() error {
	// Held while the session lasts, so the last fence to close can remove a
	// folder it made, and one closing waits out a fence starting.
	if f.stateLock == nil {
		f.stateLock = stateUseLock(f.policy.Workspace)
	}
	p := filepath.Join(f.policy.Workspace, stateDir)
	if err := os.Mkdir(p, 0o700); err == nil {
		f.madeState = true
	} else if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("making the workspace's %s for the fence to cover: %w", stateDir, err)
	}
	id, err := folderIdentity(p)
	if err != nil {
		return err
	}
	if !id.dir {
		return fmt.Errorf("the workspace's %s is not a folder, and the fence can cover only a folder", stateDir)
	}
	// An entry an earlier fence took out but could not move or remove is not state.
	n, err := plantedEntry(p)
	if err != nil {
		return fmt.Errorf("the workspace's %s cannot be listed to tell whether it holds state (%w); restore its permissions, and the fence will cover it", stateDir, err)
	}
	if n != "" {
		return fmt.Errorf("the workspace's %s holds %s, which an earlier fenced session found planted and could not take out; remove it, and the fence will cover the folder", stateDir, n)
	}
	// Not known to be empty is not taken for state: what appears in it
	// later would then be accepted as Abhed's own.
	holds, err := folderHolds(p, id)
	if err != nil {
		return fmt.Errorf("the workspace's %s cannot be listed to tell whether it holds state (%w); restore its permissions, and the fence will cover it", stateDir, err)
	}
	f.stateID, f.stateHeld = &id, holds
	if f.madeState {
		noteMade(f.stateLock, id)
	}
	return nil
}

// folderID identifies a folder beyond its inode, which a filesystem reuses:
// by its birth time too, where the filesystem records one.
type folderID struct {
	info     os.FileInfo
	dir      bool
	birth    int64
	hasBirth bool
}

func (a folderID) same(b folderID) bool {
	return os.SameFile(a.info, b.info) && a.hasBirth == b.hasBirth && a.birth == b.birth
}

// folderHolds is whether folder p, still id, holds anything; one that cannot be
// listed, or changed meanwhile, is the error.
func folderHolds(p string, id folderID) (bool, error) {
	d, err := os.Open(p) // #nosec G304 -- the workspace's .abhed
	if err != nil {
		return true, err
	}
	defer func() { _ = d.Close() }()
	info, err := d.Stat()
	if err != nil {
		return true, err
	}
	if !os.SameFile(info, id.info) {
		return true, errors.New("it was replaced while it was being listed")
	}
	switch _, err = d.Readdirnames(1); {
	case errors.Is(err, io.EOF):
		return false, nil
	case err != nil:
		return true, err
	}
	return true, nil
}

// plantedEntry is the first entry of the folder p named as a taken-out entry.
func plantedEntry(p string) (string, error) {
	d, err := os.Open(p) // #nosec G304 -- the workspace's .abhed
	if err != nil {
		return "", err
	}
	defer func() { _ = d.Close() }()
	for {
		names, err := d.Readdirnames(512)
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		for _, n := range names {
			if strings.HasPrefix(n, plantedPrefix) {
				return n, nil
			}
		}
	}
}

// isSharedState is whether p is the .abhed each namespace covers; other fences
// mount over it, so it is never moved.
func (f *Fence) isSharedState(p string) bool {
	if !f.mounts || f.stateID == nil || filepath.Base(p) != stateDir {
		return false
	}
	id, err := folderIdentity(p)
	return err == nil && id.dir && id.same(*f.stateID)
}

// plan is what the command's namespace holds read-only or hides, built per
// command; holders are pinned so they cannot be renamed away.
func (f *Fence) plan(ctx context.Context) (*mountns.Plan, error) {
	ws := f.policy.Workspace
	// A view made since the session qualified shows what the plan cannot cover.
	if v, err := mountns.Views(ws); err != nil || len(v) > 0 {
		if err == nil {
			err = fmt.Errorf("%s shows the workspace's files through a filesystem of its own (FUSE, overlay or a network share), "+
				"where Abhed's state and git's files are not covered; unmount it", strings.Join(v, ", "))
		}
		return nil, err
	}
	forms := PathForms(ws)
	p := &mountns.Plan{Root: ws}
	seen := map[string]bool{}
	rel := func(path string) (string, bool) {
		for _, w := range forms {
			if rest, ok := Within(path, w); ok && len(rest) > 0 {
				return filepath.Join(rest...), true
			}
		}
		return "", false
	}
	// Under the .abhed, which is covered whole.
	underState := func(r string) bool {
		first, _, _ := strings.Cut(r, string(filepath.Separator))
		return strings.EqualFold(first, stateDir)
	}
	var protected []string
	for _, q := range formsOf(f.policy.WriteProtected) {
		if insideAny(q, forms) {
			protected = append(protected, q)
		}
	}
	if f.policy.ProtectGit {
		found, err := gitGuardFor(ctx, ws, string(TierFence), &f.git, f.policy.GitRequired)
		if err != nil {
			return nil, errors.New(strings.TrimPrefix(err.Error(), "sandbox: the command was not run: "))
		}
		protected = append(protected, found...)
	}
	for _, h := range holders(forms, protected) {
		r, ok := rel(h)
		if !ok || seen[r] || underState(r) {
			continue
		}
		if info, err := os.Lstat(h); err == nil && info.IsDir() {
			seen[r] = true
			p.Pin = append(p.Pin, r)
		}
	}
	for _, q := range protected {
		r, ok := rel(q)
		if !ok || seen[r] || underState(r) {
			continue
		}
		info, err := os.Lstat(q)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symbolic link, and the fence holds only a real file or folder read-only", q)
		}
		seen[r] = true
		p.ReadOnly = append(p.ReadOnly, r)
	}
	p.Empty = append(p.Empty, stateDir)
	// On a disk that folds case, another spelling reaches the same folder
	// under a name the mount above may not cover; each is covered too.
	for _, name := range caseSpellings(ws, stateDir) {
		if info, err := os.Lstat(filepath.Join(ws, name)); name != stateDir && err == nil && info.IsDir() {
			p.Empty = append(p.Empty, name)
		}
	}
	for _, sp := range (&Process{policy: f.policy}).statePaths() {
		r, ok := rel(sp)
		if !ok || seen[r] || underState(r) {
			continue
		}
		info, err := os.Lstat(sp)
		if err != nil {
			continue
		}
		// Only a folder should reach here; a state file is refused, not left
		// open.
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is a state file inside the workspace and outside its %s, which the fence does not hide", sp, stateDir)
		}
		seen[r] = true
		p.Empty = append(p.Empty, r)
	}
	return p, p.Validate()
}

// stateEntries are the workspace's .abhed entries in any case; a failed listing
// or lookup is the error, as what it hides cannot be ruled out.
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

// plantedPrefix begins the name a planted .abhed, or an entry of the covered
// one, is renamed to; no part of Abhed reads it.
const plantedPrefix = ".abhed-planted-"

// What became of a planted entry, as fence.state_planted records it.
const (
	plantMoved     = "moved"
	plantRenamed   = "renamed_in_place"
	plantRemoved   = "removed"
	plantRemaining = "still_present"
	// plantEmptied is the covered .abhed, its contents taken out one by
	// one and the folder left in place.
	plantEmptied = "contents_taken_out"
)

// checkPlanted quarantines any .abhed a command made in the workspace, reports
// it and refuses later commands; an unlistable workspace counts as one.
func (f *Fence) checkPlanted(rec func(string, map[string]any) error, callID, when string) error {
	f.plantMu.Lock()
	defer f.plantMu.Unlock()
	found, lerr := stateEntries(f.policy.Workspace)
	// The .abhed each command's namespace covers is Abhed's, not planted.
	found = slices.DeleteFunc(found, f.isStateMount)
	if len(found) == 0 && lerr == nil {
		return nil
	}
	var entries []map[string]any
	var done, left, what []string
	tally := func(name string, m map[string]any) {
		switch m["outcome"] {
		case plantMoved:
			done = append(done, name+" was moved to "+m["moved_to"].(string))
		case plantRenamed:
			done = append(done, name+" was renamed in place to "+filepath.Base(m["renamed_to"].(string))+" (it could not be moved out: "+m["reason"].(string)+")")
		case plantRemoved:
			done = append(done, name+" was removed")
		default:
			if r, ok := m["renamed_to"].(string); ok {
				name += " (renamed to " + filepath.Join(filepath.Base(filepath.Dir(r)), filepath.Base(r)) + ")"
			}
			left = append(left, name)
		}
	}
	for _, p := range found {
		// The covered .abhed stays, emptied: moving it would move other
		// fences' mounts with it and uncover their commands.
		if f.isSharedState(p) {
			if contents, ok := f.takeOutContents(p); ok {
				entries = append(entries, map[string]any{"path": p, "outcome": plantEmptied, "contents": contents})
				what = append(what, "the workspace's "+stateDir+", which was empty when this session's fence started, holds something now or could not be listed")
				for _, m := range contents {
					tally(stateDir+"/"+filepath.Base(m["path"].(string)), m)
				}
				continue
			}
		}
		m := f.takeOut(p, filepath.Dir(p))
		entries = append(entries, m)
		what = append(what, "a "+filepath.Base(p)+" appeared in the workspace, where Abhed keeps its own state")
		tally(filepath.Base(p), m)
	}
	parts := what
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

// takeOutContents takes out each entry of the covered .abhed at p, renamed inside
// it where every tmpfs hides it; false when p cannot be listed.
func (f *Fence) takeOutContents(p string) ([]map[string]any, bool) {
	restoreOwnerAccess(p)
	d, err := os.Open(p) // #nosec G304 -- the workspace's .abhed
	if err != nil {
		return nil, false
	}
	// Read in batches to a bound, so a flooded folder cannot hold Abhed's memory.
	var names []string
	more := false
	for {
		batch, err := d.Readdirnames(512)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = d.Close()
			return nil, false
		}
		names = append(names, batch...)
		if len(names) >= takeOutBound {
			more, names = true, names[:takeOutBound]
			break
		}
	}
	_ = d.Close()
	out := make([]map[string]any, 0, len(names)+1)
	for _, n := range names {
		out = append(out, f.takeOutTo(filepath.Join(p, n), p, true))
	}
	if more {
		out = append(out, map[string]any{"path": filepath.Join(p, fmt.Sprintf("entries past the first %d", takeOutBound)), "outcome": plantRemaining,
			"error": fmt.Sprintf("more than %d entries; those past them are left, and the next check takes out more", takeOutBound)})
	}
	return out, true
}

// takeOutBound is how many entries one check takes out of the covered .abhed;
// a variable, so a test can lower it.
var takeOutBound = 4096

// takeOut renames the planted entry p into dir, then quarantines it, and says
// how; where the rename fails it is removed, else still present.
func (f *Fence) takeOut(p, dir string) map[string]any { return f.takeOutTo(p, dir, false) }

// takeOutTo is takeOut; with removeStuck, an entry quarantine cannot take is
// removed rather than left renamed in dir.
func (f *Fence) takeOutTo(p, dir string, removeStuck bool) map[string]any {
	m := map[string]any{"path": p}
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	inert := filepath.Join(dir, plantedPrefix+f.id+"-"+hex.EncodeToString(raw[:]))
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
	// A folder left without write permission cannot be moved, read or removed.
	restoreOwnerAccess(inert)
	dest, err := quarantine(inert, filepath.Base(p), "fence-"+f.id+"-")
	switch {
	case err == nil:
		m["outcome"], m["moved_to"] = plantMoved, dest
	case !removeStuck:
		m["outcome"], m["reason"] = plantRenamed, err.Error()
	default:
		if rerr := os.RemoveAll(inert); rerr != nil {
			// Left in the covered .abhed: the user must remove it.
			m["outcome"], m["error"] = plantRemaining, errors.Join(err, rerr).Error()
			return m
		}
		m["outcome"], m["reason"] = plantRemoved, err.Error()
	}
	return m
}

// quarantine moves planted p into its own folder under ~/.abhed/quarantine,
// which nothing reads as state, and returns where it went.
func quarantine(p, name, prefix string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, stateDir, QuarantineDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, prefix)
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
	out := []string{"ABHED_SANDBOX=" + string(f.Tier()), vimInitEnv()}
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
		"mode":     f.Mode(),
	}
	granted, left := f.configuredSkills(f.denied())
	out["skills"] = append(f.skillDirs(), granted...)
	if len(left) > 0 {
		out["skills_left_out"] = left
	}
	if f.mounts {
		out["state_folder_made"] = f.madeState
		if len(f.aliases) > 0 {
			out["aliases"] = f.aliases
		}
		if len(f.unreachableAliases) > 0 {
			out["aliases_unreachable"] = f.unreachableAliases
		}
		out["protected"] = map[string]any{"git": f.policy.ProtectGit, "paths": f.policy.WriteProtected}
	}
	if c, ok := f.report.Check(probe.CheckMounts); ok && !f.mounts {
		out["mounts_unavailable"] = c.Reason
	}
	if f.host != nil {
		out["cgroup"] = f.host.path()
	}
	return out
}

// sessionRefusals are forks refused at the session pids.max beyond the leaf's;
// since Linux 6.14 the session count includes the leaf's, so they come off.
func sessionRefusals(before, after, leaf uint64) uint64 {
	if after <= before || after-before <= leaf {
		return 0
	}
	return after - before - leaf
}
