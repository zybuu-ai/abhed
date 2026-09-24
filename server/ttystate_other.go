//go:build !darwin && !linux

package server

import "os"

// ttyNow cannot ask the terminal on this platform.
func ttyNow(*os.File) (int, bool, bool) { return 0, false, false }
