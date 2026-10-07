// Package sandbox isolates agent-generated command execution.
//
// Design position (docs/architecture/03-security.md): the deep-research pass
// produced ZERO verified claims on sandboxing and REFUTED two candidate claims,
// so the isolation posture of comparable agents is unverified. Abhed therefore
// treats isolation as a requirement to establish rather than a solved problem
// to copy, and this package is written to that stance:
//
//   - Tiers are explicit and self-reporting, so an operator can always answer
//     "what is actually containing this command right now?"
//   - The weakest tier refuses untrusted work rather than pretending.
//   - Nothing here claims to stop a determined kernel exploit. The boundary is
//     the tier, and the tier is named honestly.
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
	// Filesystem and network scoping, but a shared kernel.
	TierProcess Tier = "process"
	// TierContainer runs in an OCI container: namespace isolation, shared
	// kernel. Not sufficient for genuinely hostile code.
	TierContainer Tier = "container"
	// TierVM runs in a container under gVisor (runsc), a user-space kernel that
	// intercepts system calls. Not a microVM; the name is kept for compatibility.
	TierVM Tier = "vm"
	// TierFence confines each command with Landlock, a seccomp filter and a
	// cgroup of its own, on Linux, as a preview chosen with sandbox.tier. It
	// counts as the process tier for min_tier.
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

// Same reports whether the file at Path is still the one pinned: a swap
// of it, or of a folder above it, for another file or a folder is not, nor
// is the same file changed since.
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
	// AllowNetwork permits egress from inside the sandbox. Default false: a
	// successful prompt injection then has no channel to exfiltrate through
	// (docs §03 L4).
	AllowNetwork bool
	// Egress, when set, is sandbox.network allowlist: commands reach the
	// network only through the session's egress proxy, which applies this
	// policy, and AllowNetwork is not read. Only the process tier holds it;
	// every other tier refuses it.
	Egress *egress.Policy
	// ReadOnlyPaths are additional paths mounted read-only (toolchains, caches).
	ReadOnlyPaths []string
	// ReadableFiles are single files a command may read, and run, even where
	// they sit in an area the process tier hides, such as a statusline
	// script outside the workspace on Linux. The process tier only; each is
	// read-only, and dropped unless it is still the file that was pinned.
	ReadableFiles []ReadableFile
	// StatePaths are files or folders holding Abhed's state outside .abhed,
	// such as a configured users file. Commands can neither read nor write
	// them, as for .abhed.
	StatePaths []string
	// WriteProtected are paths inside the workspace a command may read but
	// not write, such as an editor's own settings there.
	WriteProtected []string
	// ProtectGit write-protects the config and hooks of every git folder in
	// the workspace, and each .git file. Seatbelt names them by pattern, at any
	// depth and for folders made later; bubblewrap and the container bind those
	// found when a command starts, down to gitWalkDepth folders.
	ProtectGit bool
	// MaxMemoryMB and MaxProcs bound resource exhaustion (threat T7): memory on the
	// container and vm tiers only, processes on those and the process tier.
	MaxMemoryMB int
	MaxProcs    int
	// Tier, when set, is the one backend Select builds, in place of the
	// strongest available; only TierFence can be chosen.
	Tier Tier
	// CPUPercent bounds the fence tier's commands' CPU time, in percent of
	// one CPU; zero leaves it unbounded.
	CPUPercent int
}

func DefaultPolicy(workspace string) Policy {
	// No time limit here: a command's is its caller's (the bash tool's
	// timeout_ms, a background shell's lifetime), and a shell runs for as
	// long as its terminal is open.
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

// Select returns the strongest available backend meeting policy.MinTier.
//
// It never silently downgrades: if nothing meets the minimum, it returns an
// error naming what was tried. A sandbox that quietly weakens itself is worse
// than no sandbox, because the operator stops checking.
func Select(p Policy) (Sandbox, error) {
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
