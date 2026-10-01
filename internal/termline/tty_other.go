//go:build !darwin && !linux

package termline

import "os"

// TTYNow cannot ask the terminal on this platform.
func TTYNow(*os.File) (int, bool, bool) { return 0, false, false }
