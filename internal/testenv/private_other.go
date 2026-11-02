//go:build !unix

package testenv

// privateFolder cannot check ownership here; a helper makes a folder of its
// own instead.
func privateFolder(string) bool { return false }
