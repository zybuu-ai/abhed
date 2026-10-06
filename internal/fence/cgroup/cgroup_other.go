//go:build !linux

package cgroup

import "os/exec"

// Discover returns ErrUnsupported: only Linux has cgroups.
func Discover() (*Base, error) { return nil, ErrUnsupported }

// Place returns ErrUnsupported: only Linux has cgroups.
func (l *Leaf) Place(*exec.Cmd) error { return ErrUnsupported }

func killPid(int) error { return ErrUnsupported }

func ownerOf(int) (int, bool) { return 0, false }
