//go:build !linux

package landlock

import (
	"fmt"
	"runtime"
)

// Detect reports no Landlock: it is a Linux security module.
func Detect() (ABI, error) {
	return 0, fmt.Errorf("%w: Landlock is Linux only, and this is %s", ErrUnsupported, runtime.GOOS)
}

// Restrict always refuses off Linux, so a command never runs unconfined.
func (r *Ruleset) Restrict() error {
	return fmt.Errorf("%w: Landlock is Linux only, and this is %s", ErrUnsupported, runtime.GOOS)
}
