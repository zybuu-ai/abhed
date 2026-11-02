//go:build !(darwin && cgo)

package sandbox

// processSandboxed cannot tell here: a build without cgo, or not macOS.
func processSandboxed(int) (sandboxed, known bool) { return false, false }

// canTellSandboxed is whether processSandboxed can answer in this build;
// without it listening stays off, as a takeover could not be found.
const canTellSandboxed = false
