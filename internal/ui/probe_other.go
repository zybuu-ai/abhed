//go:build !unix

package ui

import "os"

// probeBackground cannot poll a console handle, so it does not ask.
func probeBackground(in, out *os.File) (string, []byte) { return "", nil }
