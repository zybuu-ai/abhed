//go:build unix

package customcmd

import "syscall"

// noFollow makes an open fail on a link in the last component.
const noFollow = syscall.O_NOFOLLOW
