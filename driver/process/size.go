package process

import (
	"context"
	"maps"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// size is what a Spec's Config asks one instance to be held to.
type size struct {
	cpuPercent  int   // percent of one CPU; 0 leaves it unbounded
	memoryBytes int64 // 0 leaves it unbounded
}

func (s size) zero() bool { return s == size{} }

// parseSize reads the keys the process driver can apply, cpus and
// memory_mb, and refuses any other key rather than run without it.
func parseSize(cfg map[string]string) (size, error) {
	var out size
	var unknown []string
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		v := cfg[k]
		switch k {
		case "cpus":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 || f > 1024 || math.IsNaN(f) {
				return size{}, driver.Errorf(driver.ErrInvalid, "cpus %q must be a number above 0 and at most 1024", v)
			}
			out.cpuPercent = max(1, int(math.Round(f*100)))
		case "memory_mb":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 16 || n > 4<<20 {
				return size{}, driver.Errorf(driver.ErrInvalid, "memory_mb %q must be a whole number from 16 to %d", v, 4<<20)
			}
			out.memoryBytes = n << 20
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return size{}, driver.Errorf(driver.ErrInvalid, "the process driver cannot apply %s; use a driver that does",
			strings.Join(unknown, ", "))
	}
	return out, nil
}

// sizing holds instances to a size. The process driver uses a cgroup per
// instance on Linux; elsewhere nothing can, and a sized instance is refused.
type sizing interface {
	// Check says why no instance can be sized here, nil when one can.
	Check() error
	// NewBox makes what holds instance id to s.
	NewBox(id string, s size) (box, error)
}

// box holds one instance's commands to its size.
type box interface {
	// Place puts cmd, not yet started, inside the box; release, called once
	// the process has ended, frees what holds it and ends what it left.
	Place(cmd *exec.Cmd) (release func(), err error)
	// Remove ends every process in the box and removes it.
	Remove(ctx context.Context) error
}

// CheckConfig refuses, before an instance is made, a Config this driver
// cannot apply here: an unknown key, or a size on a host with no delegated
// cgroups.
func (d *Driver) CheckConfig(cfg map[string]string) error {
	s, err := parseSize(cfg)
	if err != nil || s.zero() {
		return err
	}
	return d.sizer().Check()
}

func (d *Driver) sizer() sizing {
	if d.opts.sizer != nil {
		return d.opts.sizer
	}
	return hostSizer
}

// sizedTier reports whether commands on tier stay where a box puts them:
// bubblewrap's, whose cgroup files are read-only inside. The fence keeps
// cgroups of its own, a container or VM tier runs its commands in the
// engine's or the guest's, and the none tier could move itself out.
func sizedTier(t sandbox.Tier) bool { return t == sandbox.TierProcess }

// nestIn has sb's own per-command cgroups made inside b, where b can hold them.
func nestIn(b box, sb sandbox.Sandbox) error {
	if n, ok := b.(interface{ nest(sandbox.Sandbox) error }); ok {
		if err := n.nest(sb); err != nil {
			return driver.Errorf(driver.ErrUnavailable, "holding the sandbox's commands to the instance's size: %v", err)
		}
	}
	return nil
}

// placeOn puts cmd in the instance's box, if it has one; the release it
// returns does nothing when it has none.
func placeOn(b box, cmd *exec.Cmd) (func(), error) {
	if b == nil {
		return func() {}, nil
	}
	release, err := b.Place(cmd)
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "holding the command to the instance's size: %v", err)
	}
	return release, nil
}
