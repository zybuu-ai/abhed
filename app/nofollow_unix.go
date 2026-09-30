//go:build unix

package app

import "syscall"

// oNoFollow refuses a link at the last step of a path.
const oNoFollow = syscall.O_NOFOLLOW

// oNonBlock keeps an open from waiting on a pipe.
const oNonBlock = syscall.O_NONBLOCK
