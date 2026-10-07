package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// fenceSessionBash gives each served session a fence of its own, started
// from the registry's bash.
func fenceSessionBash(cfg config.Config, workspace string) func(string, *agent.Recorder, tools.Bash) (tools.Bash, func() error) {
	return func(_ string, rec *agent.Recorder, shared tools.Bash) (tools.Bash, func() error) {
		sf := &sessionFence{build: func() (sandbox.Sandbox, error) { return buildSandbox(cfg, workspace) }, rec: rec}
		shared.Sandbox, shared.Shell = sf.Command, sf.Shell
		return shared, sf.Close
	}
}

// sessionFence is one served session's fence, with a cgroup of its own. It
// is qualified at the session's first command, so a session opened only to
// be read records nothing, and it records fence.qualified before that
// command runs. It refuses every command once closed or when it cannot be
// qualified or recorded; nothing runs under another tier in its place.
type sessionFence struct {
	build func() (sandbox.Sandbox, error)
	rec   *agent.Recorder

	mu     sync.Mutex
	fence  *sandbox.Fence
	err    error
	closed bool
}

func (s *sessionFence) get() (*sandbox.Fence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("the session's fence was closed")
	}
	if s.fence != nil || s.err != nil {
		return s.fence, s.err
	}
	sb, err := s.build()
	if err != nil {
		s.err = err
		return nil, err
	}
	f, ok := sb.(*sandbox.Fence)
	if !ok {
		_ = sandbox.Close(sb)
		s.err = fmt.Errorf("the session's sandbox is %s, not the fence", sb.Tier())
		return nil, s.err
	}
	if f.Mode() != sandbox.FenceModeMounts {
		_ = f.Close()
		s.err = errors.New("the session's fence has no mount namespace, which serve needs")
		return nil, s.err
	}
	if _, err := s.rec.Record(agent.EvFenceQualified, agent.ActorSystem, agent.Trusted, f.Qualification()); err != nil {
		_ = f.Close()
		s.err = fmt.Errorf("recording the fence's qualification: %w; the fence is closed, so no command will run", err)
		return nil, s.err
	}
	f.SetRecord(agent.SandboxRecord(s.rec))
	s.fence = f
	return f, nil
}

// Command runs command under the session's fence, or not at all.
func (s *sessionFence) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	f, err := s.get()
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("fence: the command was not run: %w", err)}
	}
	return f.Command(ctx, cwd, command)
}

// Shell starts the workbench terminal's shell under the session's fence.
func (s *sessionFence) Shell(ctx context.Context, cwd string) *exec.Cmd {
	f, err := s.get()
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("fence: the shell was not started: %w", err)}
	}
	return f.Shell(ctx, cwd)
}

// Close ends what the session's commands left running and removes its
// cgroup; later commands are refused.
func (s *sessionFence) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.fence == nil {
		return nil
	}
	return s.fence.Close()
}
