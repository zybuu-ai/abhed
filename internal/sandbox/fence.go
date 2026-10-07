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
	// mounts is the mode: each command gets a mount namespace of its own,
	// where protected paths are read-only and Abhed's state in the workspace
	// is covered by an empty tmpfs. Without it, Landlock alone confines the
	// command, and a surface with protected paths is refused.
	mounts bool
	// stateID is the workspace's .abhed each command's namespace covers;
	// stateHeld is whether it held anything when the fence qualified, and
	// madeState whether the fence made it, empty, to mount over.
	stateID   *folderID
	stateHeld bool
	madeState bool
	// aliases are the other mounts of the workspace's filesystem, which each
	// command's namespace covers as it covers the workspace.
	aliases []string
	// unreachableAliases lie behind a folder no command can search or chmod.
	unreachableAliases []string
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
// Close fails when it finds one or cannot list the workspace. The empty
// .abhed the fence made to mount over stays: other fences on the same
// workspace mount over it too, and removing it would detach their mounts.
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

// The fence's modes, as fence.qualified and process.launched record them.
const (
	// FenceModeMounts gives each command a user and mount namespace of its
	// own, with protected paths bound read-only and the workspace's .abhed
	// covered by an empty tmpfs, before Landlock and seccomp.
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

// needsMounts is whether the policy keeps paths inside the workspace
// read-only, which only a mount namespace can hold: Landlock grants a folder
// and everything under it.
func (f *Fence) needsMounts() bool {
	return len(f.policy.WriteProtected) > 0 || f.policy.ProtectGit
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
	mode := "mode landlock_only: no mount namespace; a .abhed a command makes at the top of the workspace, in any case, " +
		"is renamed out of the way once the command ends and moved to ~/.abhed/quarantine where it can be, the session's commands still running are ended and its further commands are refused, " +
		"as they are when the workspace can no longer be listed"
	planted := "a file a command writes into the workspace's .abhed while it runs and until the check that follows it (Abhed can read it then), " +
		"a .abhed left after an unclean exit (nothing is moved), a .abhed in a subfolder or an added folder (not checked)"
	if f.mounts {
		mode = "mode mount_namespace: each command in a user and mount namespace of its own, the workspace's .abhed under an empty tmpfs that is gone when the command ends; " +
			"where the workspace has none, the fence makes an empty .abhed to mount over and leaves it in place, and one empty when the session started must stay empty, " +
			"or what is found in it is taken out, the folder left, and the session ends as below"
		if f.needsMounts() {
			mode += ", the surface's protected paths (git's config and hooks, its editor settings) bound read-only, and a file in a read-only folder with a name outside it refused"
		}
		mode += ", the same done at every other mount of the workspace's filesystem (such as /sysroot on an ostree host, or a bind mount), or the command refused where it cannot be" +
			" (a FUSE, bindfs, overlay or NFS view of the workspace that exists before the session is not found; commands cannot make one)"
		mode += "; another spelling of .abhed a command makes is still taken out and ends the session"
		planted = "a .abhed in a subfolder or an added folder (not covered)"
	}
	return fmt.Sprintf("fence (preview, Linux) · each command under Landlock ABI %d: reads and runs system folders, ~/.abhed/skills and skills.dirs, writes the workspace and a private temp folder, "+
		"no other terminal, Abhed's state, record and secrets unreachable · %s · seccomp %s: no ptrace, namespaces, mounts, bpf, keyrings, unix sockets "+
		"or signals to Abhed's process id · %s · cgroup per call: %s · not covered: Abhed itself and its in-process tools (file, web, MCP), network filtering, "+
		"%s, other processes' command lines in /proc, a hard link to state made before the session, %s",
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

// fenceDevices are the device files a command may open. No pty: a command
// keeps the terminal it was given through its descriptors, and /dev/tty
// reaches only its own, so the user's other terminals are out of reach.
var fenceDevices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"}

// spec is what every command may reach. It is built afresh for each command,
// so a protected path that appeared since refuses the next one.
func (f *Fence) spec() landlock.Spec {
	deny := f.denied()
	s := landlock.Spec{
		Exec:    absAll(append(append([]string{}, fenceSystem...), f.policy.ReadOnlyPaths...)),
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

// configuredSkills splits skills.dirs into the folders a command may read
// and run, as written and resolved, and those left out: one that holds or
// sits inside Abhed's state, which ~/.abhed/skills alone may, one inside the
// workspace, which is granted already and whose .abhed is covered, and one
// that is not a folder.
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

// isStateMount is whether p is the workspace's .abhed that each command's
// mount namespace covers. It must be that folder by its own name, a real
// folder, and the same one the fence qualified with: the same device, inode
// and, where the filesystem records one, birth time, so a folder made again
// on a reused inode is not taken for it. One that was empty then, as one a
// fence made always is, must still be empty: no command can write there
// past its mounts, so anything in it now came from elsewhere and is planted.
// One that already held state is Abhed's own, which the mounts cover.
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

// prepareStateMount finds the workspace's .abhed for each command's
// namespace to cover, and makes it, empty, when it is missing: a tmpfs needs
// a folder to mount on. One that is not a folder refuses the fence. Another
// fence on the workspace may make it at the same time, and that one serves.
func (f *Fence) prepareStateMount() error {
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
	// Not known to be empty is not taken for state: what appears in it
	// later would then be accepted as Abhed's own.
	holds, err := folderHolds(p, id)
	if err != nil {
		return fmt.Errorf("the workspace's %s cannot be listed to tell whether it holds state (%w); restore its permissions, and the fence will cover it", stateDir, err)
	}
	f.stateID, f.stateHeld = &id, holds
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

// folderHolds is whether the folder p, still the folder id, holds anything.
// One that cannot be listed, or changed meanwhile, is not known either way,
// and is the error.
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

// isSharedState is whether p is the workspace's .abhed each command's
// namespace covers, by name and identity, whatever it holds now. Other
// fences on the workspace mount over it, so it is never moved itself.
func (f *Fence) isSharedState(p string) bool {
	if !f.mounts || f.stateID == nil || filepath.Base(p) != stateDir {
		return false
	}
	id, err := folderIdentity(p)
	return err == nil && id.dir && id.same(*f.stateID)
}

// plan is what the command's mount namespace holds read-only or hides: the
// protected paths and the folders holding them (pinned, so they cannot be
// renamed away), git's config and hooks, the workspace's .abhed, and another
// state folder in the workspace. It is built afresh for each command, so a
// protected path is held only once it exists when a command starts.
func (f *Fence) plan() (*mountns.Plan, error) {
	ws := f.policy.Workspace
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
		protected = append(protected, GitProtected(ws)...)
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
	for _, sp := range (&Process{policy: f.policy}).statePaths() {
		r, ok := rel(sp)
		if !ok || seen[r] || underState(r) {
			continue
		}
		info, err := os.Lstat(sp)
		if err != nil {
			continue
		}
		// sandboxconfig refuses a state file in the workspace outside its
		// .abhed, so only a folder reaches here, such as another
		// checkout's .abhed; a file that does is refused, not left open.
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is a state file inside the workspace and outside its %s, which the fence does not hide", sp, stateDir)
		}
		seen[r] = true
		p.Empty = append(p.Empty, r)
	}
	return p, p.Validate()
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

// takeOutContents takes out everything in the covered .abhed at p, renamed
// inside it, where every fence's tmpfs still hides it, and leaves the folder.
// It is false when the folder cannot be listed.
func (f *Fence) takeOutContents(p string) ([]map[string]any, bool) {
	restoreOwnerAccess(p)
	d, err := os.Open(p) // #nosec G304 -- the workspace's .abhed
	if err != nil {
		return nil, false
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return nil, false
	}
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, f.takeOutTo(filepath.Join(p, n), p, true))
	}
	return out, true
}

// takeOut makes the planted entry p inert and says how: renamed into the
// folder dir in the workspace, then moved to quarantine where it can be.
// Where the rename fails, the entry is removed as a last resort, else it is
// still present.
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
	// A folder the command left without write permission cannot be moved
	// to another parent, nor later read or removed.
	restoreOwnerAccess(inert)
	dest, err := quarantine(inert, filepath.Base(p), f.id)
	switch {
	case err == nil:
		m["outcome"], m["moved_to"] = plantMoved, dest
	case removeStuck && os.RemoveAll(inert) == nil:
		m["outcome"], m["reason"] = plantRemoved, err.Error()
	default:
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
