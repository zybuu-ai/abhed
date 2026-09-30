//go:build unix

package customcmd

import "syscall"

// noFollow makes an open fail on a link in the last component, and return at
// once on a FIFO or device rather than wait for a writer.
const noFollow = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
