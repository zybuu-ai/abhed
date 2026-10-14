//go:build cgo

package egress

// cgoBuild is whether this binary is built with cgo, where the relay cannot
// drop its capabilities on every thread.
const cgoBuild = true
