package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// systemctls are where systemctl is looked for; PATH is not trusted for it.
var systemctls = []string{"/usr/bin/systemctl", "/bin/systemctl"}

// askSystemd asks systemd for unit's Delegate property.
func askSystemd(ctx context.Context, unit string, user bool) (string, error) {
	bin := ""
	for _, p := range systemctls {
		if _, err := os.Stat(p); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return "", errors.New("systemctl is not installed")
	}
	args := []string{"show", "-p", "Delegate", "--value", "--", unit}
	if user {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- systemctl at a fixed path, asking for one property
	env := os.Environ()
	if user && os.Getenv("XDG_RUNTIME_DIR") == "" {
		env = append(env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// ownedByUser checks that dir and the files that move processes and enable
// controllers belong to this user, as a runtime that delegates leaves them.
func ownedByUser(dir string) error {
	uid := os.Getuid()
	for _, f := range []string{dir, filepath.Join(dir, "cgroup.procs"), filepath.Join(dir, "cgroup.subtree_control")} {
		var st unix.Stat_t
		if err := unix.Stat(f, &st); err != nil {
			return notDelegated(dir, "%s cannot be read (%v)", f, err)
		}
		if int(st.Uid) != uid {
			return notOwned(dir, f, uid, int(st.Uid))
		}
	}
	return nil
}

// notOwned words a cgroup that is not this user's.
func notOwned(dir, file string, uid, owner int) error {
	return notDelegated(dir, "%s belongs to user %d, not %d, so nothing delegated it to Abhed", file, owner, uid)
}
