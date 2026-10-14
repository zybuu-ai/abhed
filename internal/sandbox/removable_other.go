//go:build !unix

package sandbox

// makeRemovable does nothing where the process tier does not run.
func makeRemovable(string) {}
