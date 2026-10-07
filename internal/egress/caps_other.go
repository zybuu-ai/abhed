//go:build !linux

package egress

import (
	"errors"
	"syscall"
)

// dropCaps has nothing to drop: only a Linux sandbox grants the relay one.
func dropCaps() error { return nil }

// nestedUser is Linux only, where the resolver runs.
func nestedUser(_, _ string) (*syscall.SysProcAttr, error) {
	return nil, errors.New("the in-sandbox resolver is Linux only")
}
