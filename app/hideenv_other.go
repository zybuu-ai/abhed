//go:build !darwin

package app

// HideEnvironment does nothing here: on Linux each command's PID namespace
// keeps Abhed's environment from it.
func HideEnvironment() {}
