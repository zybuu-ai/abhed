//go:build !unix

package sandbox

// processExists cannot tell here, so no container is taken as orphaned.
func processExists(int) existence { return existsUnknown }
