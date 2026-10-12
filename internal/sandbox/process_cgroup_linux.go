package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/internal/fence/cgroup"
)

// processCgroups is the cgroup a bubblewrap sandbox's commands run in, one
// leaf each, where this user has a delegated one; err says why there is none.
type processCgroups struct {
	sb   *cgroup.Sandbox
	err  error
	next atomic.Int64
}

// cgroups finds and makes the sandbox's cgroup once.
func (s *Process) cgroups() *processCgroups {
	s.cgOnce.Do(func() {
		s.cg = &processCgroups{}
		base, err := cgroup.Discover()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = base.ConfirmDelegated(ctx)
			cancel()
		}
		if err != nil {
			s.cg.err = err
			return
		}
		var raw [4]byte
		_, _ = rand.Read(raw[:])
		s.cg.sb, s.cg.err = base.NewSandbox(fmt.Sprintf("pt-%d-%s", os.Getpid(), hex.EncodeToString(raw[:])), cgroup.Limits{})
	})
	return s.cg
}

// place puts cmd in its own bounded cgroup leaf, gone when ctx ends; without a
// delegated cgroup, a memory bound refuses cmd.
func (s *Process) place(ctx context.Context, cmd *exec.Cmd) *exec.Cmd {
	if s.backend != "bwrap" || cmd.Err != nil {
		return cmd
	}
	cg := s.cgroups()
	if cg.sb == nil {
		if s.policy.MemoryRequired {
			return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: sandbox.max_memory_mb is set, and only a cgroup holds a command's memory here, "+
				"which this user has none delegated (%v); start Abhed in a delegated scope (systemd-run --user --scope -p Delegate=yes)", cg.err)}
		}
		return cmd
	}
	l := cgroup.Limits{PidsMax: int64(s.policy.MaxProcs), OOMGroup: s.policy.MaxMemoryMB > 0}
	if s.policy.MaxMemoryMB > 0 {
		l.MemoryMax = int64(s.policy.MaxMemoryMB) << 20
	}
	leaf, err := cg.sb.NewLeaf("c"+strconv.FormatInt(cg.next.Add(1), 10), l)
	if err == nil {
		err = leaf.Place(cmd)
	}
	if err != nil {
		if leaf != nil {
			_ = leaf.Remove(context.Background())
		}
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: its cgroup: %w", err)}
	}
	context.AfterFunc(ctx, func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = leaf.Remove(rctx)
	})
	return cmd
}

// cgroupRefusal is why bubblewrap cannot hold a memory bound asked for here.
func (s *Process) cgroupRefusal() string {
	if !s.policy.MemoryRequired {
		return ""
	}
	if cg := s.cgroups(); cg.sb == nil {
		return fmt.Sprintf("sandbox.max_memory_mb is set, and only a cgroup holds a command's memory on the process tier, which this user has none delegated (%v); "+
			"start Abhed in a delegated scope (systemd-run --user --scope -p Delegate=yes) or use the container, vm or fence tier", cg.err)
	}
	return ""
}

// cgroupDescribe says how the sandbox's commands are bounded by cgroups.
func (s *Process) cgroupDescribe() string {
	if s.backend != "bwrap" {
		return ""
	}
	if cg := s.cgroups(); cg.sb != nil {
		return "a cgroup per command: processes and memory bounded"
	}
	return ""
}

// removeCgroups ends what the sandbox's commands left and removes its cgroup.
func (s *Process) removeCgroups() error {
	s.cgOnce.Do(func() { s.cg = &processCgroups{} })
	if s.cg.sb == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.cg.sb.Remove(ctx)
}
