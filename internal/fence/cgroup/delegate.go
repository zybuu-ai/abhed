package cgroup

import (
	"context"
	"path"
	"strings"
)

// The kernel lets a user write a cgroup that systemd still manages (any
// scope under the user's own manager), and systemd then rewrites or removes
// what Abhed made there. So writability is not delegation: the manager that
// made the cgroup is asked.

// Manager answers whether a systemd unit has Delegate=yes. user selects the
// person's own manager (systemctl --user) rather than the system's.
type Manager func(ctx context.Context, unit string, user bool) (string, error)

// ConfirmDelegated asks the manager of the base cgroup whether it really
// delegated it, and refuses, wrapping ErrNotDelegated, when it did not or
// cannot say. Outside systemd, as in a container whose runtime delegated its
// cgroup, the cgroup and its interface files must belong to this user.
func (b *Base) ConfirmDelegated(ctx context.Context) error {
	return confirmDelegated(ctx, b.dir, askSystemd, ownedByUser)
}

// confirmDelegated is ConfirmDelegated with its two questions passed in.
func confirmDelegated(ctx context.Context, dir string, ask Manager, owned func(string) error) error {
	unit, user, managed := unitOf(dir)
	if unit == "" {
		if managed {
			return notDelegated(dir, "it sits under systemd but in no unit that could be delegated")
		}
		return owned(dir)
	}
	v, err := ask(ctx, unit, user)
	if err != nil {
		return notDelegated(dir, "systemd could not be asked whether %s is delegated (%v)", unit, err)
	}
	if v = strings.TrimSpace(v); v != "yes" {
		return notDelegated(dir, "systemd reports Delegate=%s for %s, so it still manages this cgroup", orNone(v), unit)
	}
	return nil
}

// unitOf is the nearest systemd unit at or above dir that can be delegated
// (a scope or a service), whether it belongs to a user's own manager, and
// whether dir is under systemd at all.
func unitOf(dir string) (unit string, user, managed bool) {
	parts := strings.Split(strings.TrimPrefix(path.Clean(dir), cgroupMount), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		if strings.HasSuffix(p, ".scope") || strings.HasSuffix(p, ".service") {
			for _, above := range parts[:i] {
				if strings.HasPrefix(above, "user@") && strings.HasSuffix(above, ".service") {
					user = true
				}
			}
			return p, user, true
		}
	}
	for _, p := range parts {
		if strings.HasSuffix(p, ".slice") {
			managed = true
		}
	}
	return "", false, managed
}

// cgroupMount is where the unified hierarchy is mounted.
const cgroupMount = "/sys/fs/cgroup"

func orNone(s string) string {
	if s == "" {
		return "(nothing)"
	}
	return s
}
