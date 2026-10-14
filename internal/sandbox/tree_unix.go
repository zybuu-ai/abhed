//go:build linux || darwin

package sandbox

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 64
