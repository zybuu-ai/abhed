//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package regularfile

import (
	"errors"
	"syscall"
)

// swappedLink reports the error O_NOFOLLOW gives on a link: ELOOP, or EMLINK
// on FreeBSD and EFTYPE on NetBSD.
func swappedLink(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) || errors.Is(err, syscall.EFTYPE)
}
