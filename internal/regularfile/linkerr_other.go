//go:build !(darwin || freebsd || netbsd || openbsd || dragonfly)

package regularfile

import (
	"errors"
	"syscall"
)

// swappedLink reports the error O_NOFOLLOW gives on a link.
func swappedLink(err error) bool { return errors.Is(err, syscall.ELOOP) }
