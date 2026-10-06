//go:build !linux

package mountns

import (
	"errors"
	"syscall"
)

var errLinux = errors.New("a mount namespace of the command's own needs Linux")

// Attr does nothing off Linux, where no command is fenced.
func Attr(*syscall.SysProcAttr) {}

// Apply refuses off Linux.
func Apply(Plan) error { return errLinux }

// Drop refuses off Linux.
func Drop() error { return errLinux }

// SelfTest refuses off Linux.
func SelfTest(string) (string, error) { return "", errLinux }
