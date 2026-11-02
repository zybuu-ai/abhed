//go:build !(darwin && cgo)

package sandbox

// pathDenied cannot tell here: a build without cgo, or not macOS.
func pathDenied(int, string) (denied, known bool) { return false, false }

// userPIDs are not listed here, as no process can be proven a sandbox's.
func userPIDs() []int { return nil }

// canTellSandboxed is whether pathDenied can answer in this build; without
// it listening stays off, as a takeover could not be ended safely.
const canTellSandboxed = false
