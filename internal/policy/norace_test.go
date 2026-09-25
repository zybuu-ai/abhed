//go:build !race

package policy

// slowdown scales a time bound for the race detector's instrumentation.
const slowdown = 1
