// Package sandbox isolates agent-generated command execution. Tiers name
// themselves honestly; none claims to stop a kernel exploit.
// Design position: docs/architecture/03-security.md.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Tier is the isolation strength actually in force.
type Tier string

const (
	// TierNone runs commands directly on the host. Suitable only for trusted
	// repositories, and refused for untrusted work.
	TierNone Tier = "none"
	// TierProcess adds process-level confinement (macOS sandbox-exec, Linux
	// bubblewrap namespaces and mounts; no seccomp filter and no Landlock).
	TierProcess Tier = "process"
	// TierContainer runs in an OCI container: namespace isolation, shared
	// kernel. Not sufficient for genuinely hostile code.
	TierContainer Tier = "container"
	// TierVM runs in a container under gVisor (runsc), a user-space kernel that
	// intercepts system calls. Not a microVM; the name is kept for compatibility.
	TierVM Tier = "vm"
	// TierFence confines each command with Landlock, a seccomp filter and a
	// cgroup of its own, on Linux, as a preview chosen with sandbox.tier.
	TierFence Tier = "fence"
)

// Strength orders tiers so callers can compare against a required minimum.
func (t Tier) Strength() int {
	switch t {
	case TierProcess, TierFence:
		return 1
	case TierContainer:
		return 2
	case TierVM:
		return 3
	default:
		return 0
	}
}

// ReadableFile is a file pinned when it was checked: Path is resolved, with
// no symlink in it, and Info is what it was then.
type ReadableFile struct {
	Path string
	Info os.FileInfo
}

// PinReadable resolves path and pins the regular file it names.
func PinReadable(path string) (ReadableFile, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ReadableFile{}, err
	}
	info, err := os.Lstat(real)
	if err != nil {
		return ReadableFile{}, err
	}
	if !info.Mode().IsRegular() {
		return ReadableFile{}, fmt.Errorf("%s is not a regular file", real)
	}
	return ReadableFile{Path: real, Info: info}, nil
}

// Same reports whether the file at Path is still the one pinned: a swap of it,
// or of a folder above it, for another file or a folder is not, nor is the same
// file changed since.
func (f ReadableFile) Same() bool {
	if f.Info == nil {
		return false
	}
	cur, err := os.Lstat(f.Path)
	if err != nil || !cur.Mode().IsRegular() || !os.SameFile(cur, f.Info) {
		return false
	}
	// An inode number can be handed out again once freed, as ext4 does, so
	// the size and both change times must match as well.
	if cur.Size() != f.Info.Size() || !cur.ModTime().Equal(f.Info.ModTime()) || cur.Mode() != f.Info.Mode() {
		return false
	}
	c1, ok1 := changeTime(cur)
	c2, ok2 := changeTime(f.Info)
	return ok1 == ok2 && c1 == c2
}

// Policy declares what a session's execution environment must provide.
type Policy struct {
	// MinTier is refused at startup if no available backend meets it.
	MinTier Tier
	// Workspace is the only host path the command may reach.
	Workspace string
	// AllowNetwork permits egress from inside the sandbox. Default false, so a
	// prompt injection has no exfiltration channel.
	AllowNetwork bool
	// Egress, when set, is sandbox.network allowlist: commands reach the
	// network only through the session's egress proxy, which applies this
	// policy, and AllowNetwork is not read. Other tiers refuse it.
	Egress *egress.Policy
	// ReadOnlyPaths are additional paths mounted read-only (toolchains, caches).
	ReadOnlyPaths []string
	// ReadableFiles are single files a command may read, and run, even where
	// they sit in an area the process tier hides, such as a statusline script
	// outside the workspace on Linux.
	ReadableFiles []ReadableFile
	// StatePaths are files or folders holding Abhed's state outside .abhed,
	// such as a configured users file. Commands cannot read or write them.
	StatePaths []string
	// WriteProtected are paths inside the workspace a command may read but
	// not write, such as an editor's own settings there.
	WriteProtected []string
	// Hidden are folders commands can neither read nor write, apart from the
	// workspace when it lies inside one: the folder holding other sandboxes'
	// workspaces beside this one's.
	Hidden []string
	// ProtectGit write-protects the gitPointers of every git folder in the
	// workspace and each .git file; see gitprotect.go for how each backend does it.
	ProtectGit bool
	// ScopeSignals requires a TierFence that keeps signals to the command's
	// own processes (ABI 6), for sessions of several accounts on one host.
	ScopeSignals bool
	// GitRequired refuses a fence that cannot hold ProtectGit (no mount
	// namespace); without it, that fence runs and says git's files are not held.
	GitRequired bool
	// MaxMemoryMB and MaxProcs bound resource exhaustion (T7) on Linux tiers (a
	// cgroup per command, else each process's data); on macOS MaxProcs alone.
	MaxMemoryMB int
	MaxProcs    int
	// ReadableHome lets the macOS process tier read all of home but the
	// HomeSecrets; otherwise only the toolchain folders there are readable.
	ReadableHome bool
	// RelayBinary is a Linux build of Abhed run inside a container as the
	// egress relay where the engine is in a VM or on another host.
	RelayBinary string
	// SharedCaches lets the macOS process tier write the real toolchain
	// caches; otherwise each sandbox's commands get caches of their own.
	SharedCaches bool
	// PrivateTemp gives the process tier's commands a temp folder of this
	// sandbox's own as TMPDIR (on Linux, as /tmp) and keeps them out of the
	// shared temp areas, for sandboxes of several accounts on one host.
	PrivateTemp bool
	// MemoryRequired refuses a Linux process tier command whose memory no
	// cgroup can hold, for max_memory_mb set in a configuration file.
	MemoryRequired bool
	// MaxCPUPercent bounds a container tier command's CPU, in percent of one
	// CPU; zero means 200.
	MaxCPUPercent int
	// Tier, when set, is the one backend Select builds, in place of the
	// strongest available; only TierFence can be chosen.
	Tier Tier
	// CPUPercent bounds the fence tier's commands' CPU time, in percent of
	// one CPU; zero leaves it unbounded.
	CPUPercent int
	// FenceEgressPortOnly lets the fence hold the egress allowlist without a
	// network namespace, by the proxy's port alone.
	FenceEgressPortOnly bool
	// FenceAllowRoot lets the fence run as root; files under /etc only root
	// may read are then hidden from commands. Managed only.
	FenceAllowRoot bool
	// FenceSocketcall lets the fence's commands on s390x make sockets
	// through socketcall(2) when AllowNetwork is set.
	FenceSocketcall bool
	// SkillDirs are the configured skills.dirs, absolute. The fence lets
	// commands run them, never inside Abhed's state.
	SkillDirs []string
	// fenceNoMounts keeps the fence from giving commands a mount
	// namespace of their own, for tests of the mode without one.
	fenceNoMounts bool
}

func DefaultPolicy(workspace string) Policy {
	// No time limit here: a command's is its caller's, and a shell lives as
	// long as its terminal.
	return Policy{
		MinTier:      TierProcess,
		Workspace:    workspace,
		AllowNetwork: false,
		MaxMemoryMB:  4096,
		MaxProcs:     512,
	}
}

// Sandbox builds an isolated command.
type Sandbox interface {
	// Tier reports the isolation actually provided, not the one requested.
	Tier() Tier
	// Available reports whether this backend can run here, with a reason when
	// it cannot, so startup can explain itself.
	Available() (bool, string)
	// Command returns an exec.Cmd that runs `command` under isolation.
	Command(ctx context.Context, cwd, command string) *exec.Cmd
	// Describe is shown by `abhed doctor` and recorded in the audit trail.
	Describe() string
}

// Caveat is what a working sandbox lacks on this host, such as "no /proc",
// or "" when it lacks nothing it would normally give a command.
func Caveat(sb Sandbox) string {
	if c, ok := sb.(interface{ Caveat() string }); ok {
		return c.Caveat()
	}
	return ""
}

// Select returns the strongest available backend meeting policy.MinTier. It
// never silently downgrades: with none meeting it, it errors.
func Select(p Policy) (Sandbox, error) {
	if err := checkHidden(p); err != nil {
		return nil, err
	}
	if p.Tier != "" {
		return selectChosen(p)
	}
	candidates := []Sandbox{
		NewGVisor(p),
		NewContainer(p),
		NewProcess(p),
		NewNone(p),
	}

	var tried []string
	for _, c := range candidates {
		available, why := c.Available()
		if !available {
			tried = append(tried, fmt.Sprintf("%s (%s)", c.Tier(), why))
			continue
		}
		if c.Tier().Strength() < p.MinTier.Strength() {
			tried = append(tried, fmt.Sprintf("%s (weaker than required %s)", c.Tier(), p.MinTier))
			continue
		}
		return c, nil
	}
	return nil, fmt.Errorf(
		"no sandbox backend meets the required minimum tier %q.\nTried: %s.\n"+
			"Install gVisor (runsc) or a container runtime, or lower sandbox.min_tier "+
			"in config — but do not run untrusted repositories below tier %q",
		p.MinTier, strings.Join(tried, "; "), TierProcess)
}

// checkHidden refuses a hidden folder that a read-only path or a skills
// folder would show to commands, on the tiers that mount what they show.
func checkHidden(p Policy) error {
	for _, h := range p.Hidden {
		for _, shown := range append(append([]string{}, p.ReadOnlyPaths...), p.SkillDirs...) {
			for _, hf := range PathForms(h) {
				for _, sf := range PathForms(shown) {
					if _, in := Within(hf, sf); in {
						return fmt.Errorf("%s is shown to commands (read_only_paths or skills), so the sandboxes in %s cannot be kept apart", shown, h)
					}
					if _, in := Within(sf, hf); in {
						return fmt.Errorf("%s lies among other sandboxes' folders in %s, which commands may not see", shown, h)
					}
				}
			}
		}
	}
	return nil
}

// selectChosen builds the one tier the configuration chose, or refuses
// naming why: never another tier in its place.
func selectChosen(p Policy) (Sandbox, error) {
	if p.Tier != TierFence {
		return nil, fmt.Errorf("sandbox.tier %q cannot be chosen; only %q can", p.Tier, TierFence)
	}
	if TierFence.Strength() < p.MinTier.Strength() {
		return nil, fmt.Errorf("sandbox.tier is fence, which counts as tier %q, weaker than the required minimum %q", TierProcess, p.MinTier)
	}
	f := NewFence(p)
	if ok, why := f.Available(); !ok {
		return nil, fmt.Errorf("sandbox.tier is fence, but commands cannot be fenced here, and no other tier is used in its place: %s", why)
	}
	return f, nil
}
