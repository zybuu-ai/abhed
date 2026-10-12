//go:build !unix && !windows

package nlink

import "os"

// Of returns 1: the link count is not read on this platform.
func Of(os.FileInfo) uint64 { return 1 }

// pathCount is Of for the file at path.
func pathCount(_ string, info os.FileInfo) uint64 { return Of(info) }
