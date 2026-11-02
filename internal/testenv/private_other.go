//go:build !unix

package testenv

// privateDir is unknown here; the caller makes a folder of its own instead.
func privateDir(string) bool { return false }
