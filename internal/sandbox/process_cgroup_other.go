//go:build !linux

package sandbox

import (
	"context"
	"os/exec"

	"github.com/zybuu-ai/abhed/internal/fence/cgroup"
)

type processCgroups struct{}

// NestCgroups does nothing off Linux, where commands get no cgroup.
func (s *Process) NestCgroups(*cgroup.Sandbox) bool { return false }

func (s *Process) place(_ context.Context, cmd *exec.Cmd) *exec.Cmd { return cmd }

func (s *Process) cgroupDescribe() string { return "" }

func (s *Process) removeCgroups() error { return nil }

func (s *Process) cgroupRefusal() string { return "" }
