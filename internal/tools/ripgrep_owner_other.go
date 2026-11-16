//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package tools

// ripgrepWritable cannot judge ownership here, so ripgrep is not run.
func ripgrepWritable(string) bool { return true }
