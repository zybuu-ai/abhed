//go:build !windows

package microvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// The helper runs on macOS only; these are also tested on Linux.

type helperCheck struct {
	Protocol  int    `json:"protocol"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// runHelperCheck runs `<path> check` and reads its answer.
func runHelperCheck(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, path, "check").Output()
	if err != nil {
		return fmt.Errorf("%s check: %w", path, err)
	}
	var c helperCheck
	if err := json.Unmarshal(out, &c); err != nil {
		return fmt.Errorf("%s check: unreadable answer: %w", path, err)
	}
	if c.Protocol != HelperProtocol {
		return fmt.Errorf("%s speaks helper protocol %d, not %d", path, c.Protocol, HelperProtocol)
	}
	if !c.Supported {
		return fmt.Errorf("%s cannot run a VM here: %s", path, c.Reason)
	}
	return nil
}

// helperFolderSafe refuses a helper another user could replace: its folder
// must be writable by its owner only, and no folder above it writable by
// others unless sticky, so the file checked is the file run.
func helperFolderSafe(path string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(real)
	for first := true; ; first = false {
		fi, err := os.Stat(dir)
		if err != nil {
			return err
		}
		perm := fi.Mode()
		if perm.Perm()&0o022 != 0 && (first || perm&os.ModeSticky == 0) {
			return fmt.Errorf("the helper %s is in %s, which others may write (mode %v); keep it in a folder only its owner writes", path, dir, perm.Perm())
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}
