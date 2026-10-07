package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/internal/fence/cgroup"
	"github.com/zybuu-ai/abhed/internal/fence/landlock"
	"github.com/zybuu-ai/abhed/internal/fence/mountns"
	"github.com/zybuu-ai/abhed/internal/fence/probe"
	"github.com/zybuu-ai/abhed/internal/fence/seccomp"
)

// The launcher runs before anything else in any binary that links this
// package, so the abhed command and an SDK embedder fence commands alike.
// It runs whole on this thread, which executes the command: the mounts'
// capability, its drop, Landlock and the checks of them are all per thread.
func init() {
	if len(os.Args) > 1 && os.Args[1] == fenceLauncherArg {
		runtime.LockOSThread()
		os.Exit(runLauncher(os.Args[2:]))
	}
}

// folderIdentity is p's identity, not following a link, with its birth time
// where the filesystem records one; both are read from one descriptor, so a
// folder swapped in between cannot lend the other its birth time.
func folderIdentity(p string) (folderID, error) {
	fd, err := unix.Open(p, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return folderID{}, &os.PathError{Op: "open", Path: p, Err: err}
	}
	file := os.NewFile(uintptr(fd), p) // #nosec G115 -- a descriptor fits
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return folderID{}, err
	}
	id := folderID{info: info, dir: info.IsDir()}
	var stx unix.Statx_t
	if unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_INO|unix.STATX_BTIME, &stx) == nil && stx.Mask&unix.STATX_BTIME != 0 {
		id.birth, id.hasBirth = stx.Btime.Sec*1e9+int64(stx.Btime.Nsec), true
	}
	return id, nil
}

// fenceLauncherArg is the first argument with which Abhed re-executes itself
// to launch a command under the fence.
const fenceLauncherArg = "__abhed_fence_exec"

// fenceLaunchWait bounds how long a built command has to start and report.
// A launcher that reports later finds nobody listening and runs nothing.
const fenceLaunchWait = 2 * time.Minute

// fenceSpec is what the launcher is given for one command.
type fenceSpec struct {
	Landlock landlock.Spec `json:"landlock"`
	Network  bool          `json:"network"`
	// Leaf is the call's cgroup, which the launcher joins first.
	Leaf string `json:"leaf"`
	// Qualified and ProbePID pass this run's probe: the launcher trusts it
	// only from the process that started it.
	Qualified bool `json:"qualified"`
	ProbePID  int  `json:"probe_pid"`
	// Mounts, when set, is applied in the launcher's own user and mount
	// namespace before anything else confines it.
	Mounts *mountns.Plan `json:"mounts,omitempty"`
}

// fenceReport is what the launcher applied, sent before it runs the command.
type fenceReport struct {
	PID      int    `json:"pid"`
	Cgroup   string `json:"cgroup"`
	ABI      int    `json:"landlock_abi"`
	Landlock string `json:"landlock"`
	Seccomp  string `json:"seccomp"`
	Digest   string `json:"seccomp_sha256"`
}

// fenceHost is the session's cgroup and the limits each call gets.
type fenceHost struct {
	sb   *cgroup.Sandbox
	leaf cgroup.Limits
}

func (h *fenceHost) path() string { return h.sb.Path() }

func (h *fenceHost) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return h.sb.Remove(ctx)
}

// fenceTempPrefix begins each private temp folder's name, which goes on
// with the owning process's id, so one a crashed run left can be found.
const fenceTempPrefix = "abhed-fence-"

// sweepFenceTemps removes the private temp folders of runs that ended
// without closing their fence: this user's, whose process is gone.
func sweepFenceTemps() {
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), fenceTempPrefix)
		if !ok || !e.IsDir() {
			continue
		}
		pidText, _, _ := strings.Cut(rest, "-")
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		var st unix.Stat_t
		path := filepath.Join(dir, e.Name())
		if unix.Lstat(path, &st) != nil || int(st.Uid) != os.Getuid() {
			continue
		}
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			_ = os.RemoveAll(path)
		}
	}
}

// sweepFenceCgroups removes the cgroups of runs that ended without closing
// their fence, killing what they left running: those whose Abhed process,
// named first in the id, is gone.
func sweepFenceCgroups(base *cgroup.Base) {
	for _, id := range base.Sandboxes() {
		pidText, _, ok := strings.Cut(id, "-")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = base.RemoveSandbox(ctx, id)
			cancel()
		}
	}
}

// dirSearchOpen opens the planted entry's parent folder to walk from: by
// path only, so a workspace with search permission alone still serves.
const dirSearchOpen = unix.O_PATH

// chmodNoFollow changes the mode of name in the folder parent, never
// following a link there. Before Linux 6.6 there is no fchmodat2, so the
// entry is opened by path alone, checked to be no link, and changed through
// its descriptor.
func chmodNoFollow(parent int, name string, mode uint32) error {
	err := unix.Fchmodat(parent, name, mode, unix.AT_SYMLINK_NOFOLLOW)
	if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ENOSYS) {
		return err
	}
	fd, oerr := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if oerr != nil {
		return oerr
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if serr := unix.Fstat(fd, &st); serr != nil {
		return serr
	}
	if t := st.Mode & unix.S_IFMT; t != unix.S_IFDIR && t != unix.S_IFREG {
		return err
	}
	return unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), mode)
}

// statMode is st's mode.
func statMode(st *unix.Stat_t) uint32 { return st.Mode }

// killAll ends every process of the session's commands, those still running
// included.
func (h *fenceHost) killAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.sb.Kill(ctx)
}

// processGroup is this process's group, which commands are kept from signalling.
func processGroup() int { return unix.Getpgrp() }

// FenceProbe runs the fence's probe for p afresh, with the cgroup manager's
// answer on delegation added, and makes nothing; doctor shows it.
func FenceProbe(ctx context.Context, p Policy) probe.Report {
	r, _ := fenceProbe(ctx, p)
	return r
}

func fenceProbe(ctx context.Context, p Policy) (probe.Report, *cgroup.Base) {
	base, derr := cgroup.Discover()
	req := probe.Requirements{AllowNetwork: p.AllowNetwork, Mounts: len(p.WriteProtected) > 0 || p.ProtectGit}
	if derr == nil {
		req.CgroupDir = base.Path()
	}
	r := probe.Run(ctx, req)
	c := probe.Check{ID: probe.CheckDelegated, Required: true}
	switch {
	case derr != nil:
		c.Status, c.Reason = probe.Fail, derr.Error()
	default:
		c.Value = base.Path()
		if err := base.ConfirmDelegated(ctx); err != nil {
			c.Status, c.Reason = probe.Fail, err.Error()
		} else {
			c.Status, c.Reason = probe.Pass, "the cgroup's manager confirms it is delegated"
		}
	}
	r.Add(c)
	return r, base
}

// mountsRefusal is why a surface with protected paths is refused on a host
// that gives commands no mount namespace of their own.
func mountsRefusal(why string) string {
	return "this surface keeps paths inside the workspace read-only (its editor settings or git's config and hooks), " +
		"which the fence holds only in a mount namespace of the command's own, and this host gives an ordinary user none (" + why + "); " +
		"Landlock alone cannot carve a read-only area out of the writable workspace, so use the command line, or another tier"
}

// qualifyHost runs the probe afresh, asks the cgroup's manager whether it
// delegated the cgroup, and makes the session's cgroup and private temp.
func (f *Fence) qualifyHost(ctx context.Context) (bool, string) {
	var base *cgroup.Base
	f.report, base = fenceProbe(ctx, f.policy)
	mounts, _ := f.report.Check(probe.CheckMounts)
	f.mounts = mounts.Status == probe.Pass && !f.policy.fenceNoMounts
	if f.needsMounts() && !f.mounts {
		why := mounts.Reason
		if f.policy.fenceNoMounts {
			why = "the mount namespace was turned off"
		}
		return false, mountsRefusal(why)
	}
	if !f.report.Qualified {
		return false, f.report.Summary
	}
	if f.mounts {
		// Another mount of the workspace's filesystem, such as /sysroot on an
		// ostree host, is covered as the workspace is, or the fence refuses.
		aliases, unreachable, err := mountns.Aliases(f.policy.Workspace)
		if err != nil {
			return false, "mode mount_namespace: " + err.Error() + "; the fence covers every other mount of the workspace's filesystem, and refuses where it cannot"
		}
		f.aliases, f.unreachableAliases = aliases, unreachable
		if err := f.prepareStateMount(); err != nil {
			return false, err.Error()
		}
	}
	// A .abhed made to mount over stays when the session cannot be made:
	// another fence on the workspace may already mount over it.
	return f.qualifySession(base)
}

// qualifySession makes the session's cgroup and private temp.
func (f *Fence) qualifySession(base *cgroup.Base) (bool, string) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return false, "naming the session's cgroup: " + err.Error()
	}
	// Named by this process first, so a run that ends without closing its
	// fence leaves what the next one can tell is stale.
	f.id = fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(raw[:]))
	sweepFenceTemps()
	sweepFenceCgroups(base)
	tmp, err := os.MkdirTemp("", fenceTempPrefix+f.id+"-")
	if err != nil {
		return false, "making the commands' private temp folder: " + err.Error()
	}
	for _, d := range []string{"home", "cache", "npm"} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0o700); err != nil {
			_ = os.RemoveAll(tmp)
			return false, "making the commands' private temp folder: " + err.Error()
		}
	}
	f.tmp = tmp
	if err := f.spec().Validate(); err != nil {
		_ = os.RemoveAll(tmp)
		f.tmp = ""
		return false, err.Error()
	}
	limits := cgroup.Limits{CPUPercent: f.policy.CPUPercent, PidsMax: int64(f.policy.MaxProcs)}
	if f.policy.MaxMemoryMB > 0 {
		limits.MemoryMax = int64(f.policy.MaxMemoryMB) << 20
	}
	sb, err := base.NewSandbox(f.id, limits)
	if err != nil {
		_ = os.RemoveAll(tmp)
		f.tmp = ""
		return false, err.Error()
	}
	// Each call is bounded as the session is, so a call that runs into a
	// limit is the one its events name; OOM takes the whole call.
	leaf := limits
	leaf.OOMGroup = true
	f.host = &fenceHost{sb: sb, leaf: leaf}
	return true, ""
}

// wrap runs argv through the launcher in a cgroup leaf of its own. A command
// that cannot be fenced does not start.
func (f *Fence) wrap(ctx context.Context, cwd string, env []string, argv ...string) *exec.Cmd {
	if ok, why := f.Available(); !ok {
		return refusedCmd("%s", why)
	}
	if f.closed.Load() {
		return refusedCmd("the session's fence was closed")
	}
	launch := LaunchOf(ctx)
	rec, source := launch.Record, "call"
	if rec == nil {
		rec, source = f.sessionRecord(), "harness"
	}
	// A .abhed left since the last check is moved out before anything runs.
	_ = f.checkPlanted(rec, "", "before_command")
	if why := f.planted.Load(); why != nil {
		return refusedCmd("%s", *why)
	}
	spec := f.spec()
	if err := spec.Validate(); err != nil {
		return refusedCmd("%v", err)
	}
	leaf, err := f.host.sb.NewLeaf(f.leafName(launch.CallID), f.host.leaf)
	if err != nil {
		return refusedCmd("%v", err)
	}
	drop := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = leaf.Remove(rctx)
	}
	var plan *mountns.Plan
	if f.mounts {
		pctx := ctx
		if launch.Record == nil {
			l := launch
			l.Record = rec
			pctx = WithLaunch(ctx, l)
		}
		if plan, err = f.plan(pctx); err != nil {
			drop()
			return refusedCmd("%v", err)
		}
	}
	data, err := json.Marshal(fenceSpec{Landlock: spec, Network: f.policy.AllowNetwork, Leaf: leaf.Path(),
		Qualified: f.report.Qualified, ProbePID: os.Getpid(), Mounts: plan})
	if err != nil {
		drop()
		return refusedCmd("%v", err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		drop()
		return refusedCmd("making the launcher's channel: %v", err)
	}
	if err := unix.SetNonblock(fds[0], true); err != nil {
		_, _ = unix.Close(fds[0]), unix.Close(fds[1])
		drop()
		return refusedCmd("making the launcher's channel: %v", err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "fence-channel"), os.NewFile(uintptr(fds[1]), "fence-channel") // #nosec G115 -- descriptors fit
	// This very program, as the kernel holds it, even once its file is replaced.
	cmd := exec.CommandContext(ctx, "/proc/self/exe", append([]string{fenceLauncherArg, string(data), "--"}, argv...)...) // #nosec G204 -- this binary, with the launcher's fixed argv
	cmd.Dir = cwd
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{theirs}
	if plan != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		mountns.Attr(cmd.SysProcAttr)
	}
	// Release ends the wait at once for a command that never starts.
	abandoned := make(chan struct{})
	var once sync.Once
	pendingLaunches.Store(cmd, func() {
		once.Do(func() {
			close(abandoned)
			_ = ours.SetReadDeadline(time.Now())
		})
	})
	sup := supervision{ours: ours, theirs: theirs, leaf: leaf, launch: launch, rec: rec, source: source, drop: drop, plan: plan,
		abandoned: abandoned, done: func() { pendingLaunches.Delete(cmd) }}
	// The supervisor outlives this call by design: it holds the launch until
	// the command exits, and cleans up with contexts of its own.
	go f.supervise(ctx, sup) // #nosec G118 -- the cleanup must outlive the caller's context, which may already be cancelled
	return cmd
}

// supervision is what the supervisor of one launch holds.
type supervision struct {
	ours, theirs *os.File
	leaf         *cgroup.Leaf
	launch       Launch
	// rec is where the launch is recorded, and source whose it is: "call"
	// for a tool call's, "harness" for one Abhed makes itself.
	rec    func(string, map[string]any) error
	source string
	drop   func()
	// plan is what the command's mount namespace holds, nil without one.
	plan *mountns.Plan
	// abandoned closes when the command is released unstarted; done
	// forgets it once the launcher has reported or the wait is over.
	abandoned <-chan struct{}
	done      func()
}

// ackByte lets the launcher run the command, once its launch is recorded.
const ackByte = 'y'

// supervise answers one launcher: it records the launch before acking, so
// a command runs only once the record holds it, then waits for the command
// to exit, ends what it left running, records the limits it ran into, and
// checks the command planted no state.
func (f *Fence) supervise(ctx context.Context, sv supervision) {
	defer func() { _ = sv.ours.Close() }()
	rep, err := readReport(ctx, sv.ours, sv.abandoned)
	sv.done()
	// The launcher holds its own copy from here; without a report, closing
	// ours leaves it nothing to read, and it runs nothing.
	_ = sv.theirs.Close()
	if err != nil {
		sv.drop()
		return
	}
	leaf := sv.leaf
	pidfd, err := unix.PidfdOpen(rep.PID, 0)
	if err != nil || !slices.Contains(leafPids(leaf.Path()), rep.PID) {
		if err == nil {
			_ = unix.Close(pidfd)
		}
		sv.drop()
		return
	}
	defer func() { _ = unix.Close(pidfd) }()
	// Refused, the launcher finds the channel closed and exits 126 on its own.
	refuse := func() {
		_ = sv.ours.Close()
		waitExit(pidfd)
		sv.drop()
	}
	// A session marked planted after this command was built still refuses it.
	if f.planted.Load() != nil {
		refuse()
		return
	}
	l := sv.launch
	if sv.rec != nil {
		if err := sv.rec("process.launched", map[string]any{
			"call_id": l.CallID, "source": sv.source, "pid": rep.PID, "tier": string(TierFence), "sandbox": f.id, "cgroup": rep.Cgroup,
			"landlock_abi": rep.ABI, "landlock": rep.Landlock, "seccomp": rep.Seccomp, "seccomp_sha256": rep.Digest,
			"network": f.policy.AllowNetwork, "mode": f.Mode(), "mounts": planText(sv.plan),
		}); err != nil {
			refuse()
			return
		}
	}
	if _, err := sv.ours.Write([]byte{ackByte}); err != nil {
		refuse()
		return
	}
	waitExit(pidfd)
	// The limits are recorded before what the command left is ended: while
	// it runs, it holds the output the caller waits on, so the event comes
	// before the call's result.
	if ev, err := leaf.Events(); err == nil && sv.rec != nil && (ev.OOM > 0 || ev.OOMKill > 0 || ev.OOMGroupKill > 0 || ev.PidsMax > 0) {
		_ = sv.rec("fence.limit", map[string]any{
			"call_id": l.CallID, "sandbox": f.id, "oom": ev.OOM, "oom_kill": ev.OOMKill, "oom_group_kill": ev.OOMGroupKill,
			"pids_max": ev.PidsMax, "memory_max_mb": f.policy.MaxMemoryMB, "max_procs": f.policy.MaxProcs,
		})
	}
	rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = leaf.Remove(rctx)
	// Once nothing of the command's is left running, no .abhed it made stays.
	_ = f.checkPlanted(sv.rec, l.CallID, "after_command")
}

// planText words a launch's mounts for its record.
func planText(p *mountns.Plan) string {
	if p == nil {
		return "none"
	}
	return p.Describe()
}

// readReport waits for the launcher's report, until ctx ends, the command
// is released unstarted, or the wait runs out.
func readReport(ctx context.Context, ch *os.File, abandoned <-chan struct{}) (fenceReport, error) {
	var rep fenceReport
	deadline := time.Now().Add(fenceLaunchWait)
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		select {
		case <-abandoned:
			return rep, errors.New("the command did not start")
		default:
		}
		if time.Now().After(deadline) {
			return rep, errors.New("the launcher did not report")
		}
		_ = ch.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, err := ch.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			return rep, err
		}
		if err := json.Unmarshal(buf[:n], &rep); err != nil || rep.PID <= 1 {
			return rep, fmt.Errorf("the launcher's report cannot be read: %w", err)
		}
		return rep, nil
	}
}

// waitExit returns once the process behind pidfd has exited.
func waitExit(pidfd int) {
	for {
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}} // #nosec G115 -- a descriptor fits
		if _, err := unix.Poll(fds, -1); err == nil || !errors.Is(err, unix.EINTR) {
			return
		}
	}
}

// leafPids are the processes in the cgroup at dir.
func leafPids(dir string) []int {
	b, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs")) // #nosec G304 -- a cgroup interface file
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// launcherFD is the channel to Abhed, the launcher's first extra file.
const launcherFD = 3

// runLauncher fences this process, in order: the probe's pass, the call's
// cgroup, Landlock, then seccomp; reports what it applied, waits for Abhed
// to record it, and executes the command in its own place. Any failure
// exits 126 having run nothing.
func runLauncher(args []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "abhed: the command was not run: the fence could not confine it: "+format+"\n", a...)
		return 126
	}
	if len(args) < 3 || args[1] != "--" {
		return fail("the launcher was started without a command")
	}
	var s fenceSpec
	if err := json.Unmarshal([]byte(args[0]), &s); err != nil {
		return fail("the launcher's spec cannot be read: %v", err)
	}
	// 1. The probe: this run's pass, from the Abhed that started this
	// launcher, never one kept from another run; and what it checked of the
	// would-be command, checked again here.
	if !s.Qualified || s.ProbePID != os.Getppid() {
		return fail("no qualification from the Abhed process that started it")
	}
	// 1a. The mounts, in this launcher's own namespace, where it holds
	// CAP_SYS_ADMIN only to make them; then every capability is dropped.
	if s.Mounts != nil {
		if err := mountns.Apply(*s.Mounts); err != nil {
			return fail("%v", err)
		}
		// The working folder was entered before the mounts; entered again,
		// it is seen through them.
		wd, err := os.Getwd()
		if err == nil {
			err = unix.Chdir(wd)
		}
		if err != nil {
			return fail("entering the working folder again: %v", err)
		}
		if err := mountns.Drop(); err != nil {
			return fail("%v", err)
		}
	}
	if err := holdsNothing(); err != nil {
		return fail("%v", err)
	}
	// 2. The call's cgroup, and a session and process group apart from
	// Abhed's: the command's own kill(0) cannot reach Abhed, and /dev/tty
	// opens only a terminal it was started on, never Abhed's.
	if err := joinLeaf(s.Leaf); err != nil {
		return fail("joining the call's cgroup: %v", err)
	}
	if sid, err := unix.Getsid(0); err != nil || sid != os.Getpid() {
		if _, err := unix.Setsid(); err != nil {
			return fail("leaving Abhed's session: %v", err)
		}
	}
	abhed := os.Getppid()
	abhedGroup, err := unix.Getpgid(abhed)
	if err != nil {
		return fail("reading Abhed's process group: %v", err)
	}
	if unix.Getpgrp() == abhedGroup {
		if err := unix.Setpgid(0, 0); err != nil {
			return fail("leaving Abhed's process group: %v", err)
		}
	}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return fail("turning core dumps off: %v", err)
	}
	// 3. Landlock, on this thread, from which the command is executed.
	abi, err := landlock.Detect()
	if err != nil {
		return fail("%v", err)
	}
	rs, err := landlock.New(s.Landlock, abi)
	if err != nil {
		return fail("%v", err)
	}
	if err := rs.Restrict(); err != nil {
		return fail("%v", err)
	}
	// 4. seccomp, on every thread, refusing signals to Abhed by its id.
	pol := seccomp.Command(s.Network, abhed, abhedGroup)
	if err := seccomp.Install(pol); err != nil {
		return fail("%v", err)
	}
	prog, _ := pol.Program(runtime.GOARCH)
	rep, _ := json.Marshal(fenceReport{PID: os.Getpid(), Cgroup: s.Leaf, ABI: int(abi), Landlock: rs.Describe(),
		Seccomp: seccomp.Profile, Digest: prog.Digest()})
	if _, err := unix.Write(launcherFD, rep); err != nil {
		return fail("reporting to Abhed: %v", err)
	}
	ack := make([]byte, 1)
	if n, err := unix.Read(launcherFD, ack); err != nil || n != 1 || ack[0] != ackByte {
		return fail("Abhed did not record the launch")
	}
	_ = unix.Close(launcherFD)

	// 5. The command, in this process's place.
	argv := args[2:]
	path := argv[0]
	if p, err := exec.LookPath(path); err == nil {
		path = p
	}
	err = unix.Exec(path, argv, os.Environ()) // #nosec G204 -- the command Abhed's policy allowed, now fenced
	fmt.Fprintf(os.Stderr, "abhed: %s: %v\n", argv[0], err)
	return 127
}

// holdsNothing refuses root and any capability this thread holds: the one
// that executes the command, whose capabilities the command starts with.
// Other threads may still hold the mounts' capability, which an exec drops.
func holdsNothing() error {
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return errors.New("the fence refuses root in this release")
	}
	f, err := os.Open("/proc/thread-self/status")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || !slices.Contains([]string{"CapInh", "CapPrm", "CapEff", "CapAmb"}, k) {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64); err != nil || n != 0 {
			return fmt.Errorf("the launcher holds capabilities (%s %s); the fence needs none", k, strings.TrimSpace(v))
		}
	}
	return sc.Err()
}

// joinLeaf moves this process into the cgroup at dir and checks it is there.
func joinLeaf(dir string) error {
	if !strings.HasPrefix(dir, "/sys/fs/cgroup/") {
		return fmt.Errorf("%s is not a cgroup", dir)
	}
	procs, err := os.OpenFile(filepath.Join(dir, "cgroup.procs"), os.O_WRONLY, 0) // #nosec G304 -- the call's cgroup, checked above
	if err != nil {
		return err
	}
	_, err = procs.WriteString("0")
	if cerr := procs.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	want := "/" + filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)
	for line := range strings.SplitSeq(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && strings.HasSuffix(p, want) {
			return nil
		}
	}
	return fmt.Errorf("this process is not in %s after joining it", dir)
}
