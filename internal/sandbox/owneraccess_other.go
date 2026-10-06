//go:build !unix

package sandbox

// restoreOwnerAccess does nothing where no command is fenced.
func restoreOwnerAccess(string) {}
