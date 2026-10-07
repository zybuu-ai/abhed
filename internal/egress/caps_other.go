//go:build !linux

package egress

import (
	"errors"
	"syscall"
)

var errLinuxOnly = errors.New("the in-sandbox resolver is Linux only")

func dropCaps(bool) error { return errLinuxOnly }

func undumpable() error { return errLinuxOnly }

func nestedUser(_, _ string) (*syscall.SysProcAttr, error) { return nil, errLinuxOnly }
