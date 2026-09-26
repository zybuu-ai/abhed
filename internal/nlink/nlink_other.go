//go:build !unix

package nlink

import "os"

// Of returns 1: the link count is not read on this platform.
func Of(os.FileInfo) uint64 { return 1 }
