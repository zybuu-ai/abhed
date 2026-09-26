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
	"os/exec"
	"strings"
)

// Tier is the isolation strength actually in force.
type Tier string

const (
	// TierNone runs commands directly on the host. Suitable only for trusted
	// repositories, and refused for untrusted work.
	TierNone Tier = "none"
	// TierProcess adds process-level confinement (macOS sandbox-exec, Linux
	// seccomp/landlock via bubblewrap). Filesystem and network scoping, but a
	// shared kernel.
	TierProcess Tier = "process"
	// TierContainer runs in an OCI container: namespace isolation, shared
	// kernel. Not sufficient for genuinely hostile code.
	TierContainer Tier = "container"
	// TierVM runs in a microVM (Firecracker/Kata) or gVisor's userspace kernel:
	// a hardware or syscall-interception boundary. Abhed's production default.
	TierVM Tier = "vm"
)

// Strength orders tiers so callers can compare against a required minimum.
func (t Tier) Strength() int {
	switch t {
	case TierProcess:
		return 1
	case TierContainer:
		return 2
	case TierVM:
		return 3
	default:
		return 0
	}
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
	// ReadOnlyPaths are additional paths mounted read-only (toolchains, caches).
	ReadOnlyPaths []string
	// StatePaths are files or folders holding Abhed's state outside .abhed,
	// such as a configured users file. Commands can neither read nor write
	// them, as for .abhed.
	StatePaths []string
	// MaxMemoryMB and MaxProcs bound resource exhaustion (threat T7).
	MaxMemoryMB int
	MaxProcs    int
	// TimeoutSeconds is a hard ceiling enforced by the backend, independent of
	// the caller's context.
	TimeoutSeconds int
}

func DefaultPolicy(workspace string) Policy {
	return Policy{
		MinTier:        TierProcess,
		Workspace:      workspace,
		AllowNetwork:   false,
		MaxMemoryMB:    4096,
		MaxProcs:       512,
		TimeoutSeconds: 600,
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
