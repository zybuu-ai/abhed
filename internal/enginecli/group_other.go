//go:build !unix

package enginecli

import "os/exec"

// ownGroup leaves cmd as it is where there are no process groups.
func ownGroup(*exec.Cmd) {}
