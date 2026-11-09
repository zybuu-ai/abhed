package toolset

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// RecordFired records one hook's verdict, or one simple hook's run, as hook.fired.
func RecordFired(rec *agent.Recorder, f extension.Fired) error {
	h := agent.HookFired{Extension: f.Extension, Event: string(f.Event), Verdict: f.Verdict}
	if f.Kind != "" {
		h.Kind, h.Layer, h.Reason, h.ExitCode = f.Kind, f.Layer, f.Reason, f.ExitCode
		h.DurationMS = f.Duration.Milliseconds()
	} else if f.Verdict == extension.VerdictContinue {
		h.Reason = f.Reason
	}
	_, err := rec.Record(agent.EvHookFired, agent.ActorSystem, agent.Trusted, h)
	return err
}

// maxHookOutput bounds what a hook's command may print that is kept.
const maxHookOutput = 64 << 10

// HookEnv is what the simple hooks run with: their commands in the sandbox
// bash's commands run in, at the same tier and in the workspace, and their
// requests through this set's egress guard.
func HookEnv(bash tools.Bash, workspace string, guard *egress.Guard) extension.HookEnv {
	client := &http.Client{
		Transport: &egress.Transport{Kind: egress.KindHook, Guard: guard},
		// An event is sent where it was configured to go, nowhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return extension.HookEnv{Run: hookRunner(bash, workspace), HTTP: client}
}

func hookRunner(bash tools.Bash, workspace string) extension.CommandRunner {
	return func(ctx context.Context, command string, stdin []byte) (extension.CommandResult, error) {
		var cmd *exec.Cmd
		if bash.Sandbox != nil {
			cmd = bash.Sandbox(ctx, workspace, command)
		} else {
			cmd = exec.CommandContext(ctx, "bash", "-c", command)
			cmd.Dir = workspace
			cmd.Env = append(sandbox.HostCommandEnv(), "ABHED_SESSION=1")
		}
		sandbox.EndWithCommand(cmd)
		var gitNames []string
		cmd.Env, gitNames = tools.WithGitSafeConfig(cmd.Env)
		if bash.Forward != nil {
			bash.Forward(cmd, gitNames)
		} else {
			sandbox.ForwardEnv(cmd, gitNames)
		}
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errOut limited
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		if cmd.Process == nil {
			sandbox.Release(cmd)
		}
		res := extension.CommandResult{Stdout: out.b.Bytes(), Stderr: errOut.b.Bytes(), Exit: -1}
		var exit *exec.ExitError
		switch {
		case err == nil:
			res.Exit = 0
		case errors.As(err, &exit) && exit.ExitCode() >= 0:
			res.Exit = exit.ExitCode()
		default:
			return res, err
		}
		return res, nil
	}
}

// limited keeps the first maxHookOutput bytes written and drops the rest.
type limited struct{ b bytes.Buffer }

func (l *limited) Write(p []byte) (int, error) {
	if room := maxHookOutput - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var _ io.Writer = (*limited)(nil)
