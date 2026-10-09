//go:build !unix

package container

// owner is unknown where files carry no uid.
func owner(string) (int, int) { return -1, -1 }
