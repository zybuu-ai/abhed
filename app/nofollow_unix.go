//go:build unix

package app

import "syscall"

// oNoFollow refuses a link at the last step of a path.
const oNoFollow = syscall.O_NOFOLLOW
