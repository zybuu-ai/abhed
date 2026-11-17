//go:build unix

package plugin

import "syscall"

// oNoFollow refuses to open a link at the last part of a path.
const oNoFollow = syscall.O_NOFOLLOW
