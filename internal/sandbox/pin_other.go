//go:build !unix

package sandbox

import "os"

func openPinned(path string) (*os.File, error) { return os.Open(path) }
