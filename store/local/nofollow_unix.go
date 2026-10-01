//go:build unix

package local

import "syscall"

// oNoFollow refuses a link at the last step of a path.
const oNoFollow = syscall.O_NOFOLLOW
