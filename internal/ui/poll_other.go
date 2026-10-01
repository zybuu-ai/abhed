//go:build !unix

package ui

import (
	"os"
	"time"
)

// readyFunc has no portable way to wait on a console handle, so an Esc is
// read as the start of a sequence only when the rest is already buffered.
func readyFunc(*os.File) func(time.Duration) bool { return nil }
