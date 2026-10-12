//go:build !linux

package sandbox

import (
	"context"
	"os/exec"
)

type processCgroups struct{}

func (s *Process) place(_ context.Context, cmd *exec.Cmd) *exec.Cmd { return cmd }

func (s *Process) cgroupDescribe() string { return "" }

func (s *Process) removeCgroups() error { return nil }

func (s *Process) cgroupRefusal() string { return "" }
