package toolset

import (
	"bytes"
	"context"
	"errors"
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
	} else if f.Verdict == extension.VerdictContinue || f.Verdict == extension.VerdictHalt {
		h.Reason = f.Reason
	}
	_, err := rec.Record(agent.EvHookFired, agent.ActorSystem, agent.Trusted, h)
	return err
}

// HookEnv is what the simple hooks run with: their commands as bash's run,
// in the same sandbox at the same tier, in the session's folder (the
// workspace when none is given), and their requests through this set's
// egress guard.
func HookEnv(bash tools.Bash, workspace string, guard *egress.Guard) extension.HookEnv {
	client := &http.Client{
		Transport: &egress.Transport{Kind: egress.KindHook, Guard: guard},
		// An event is sent where it was configured to go, nowhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return extension.HookEnv{Run: hookRunner(bash, workspace), HTTP: client}
}

func hookRunner(bash tools.Bash, workspace string) extension.CommandRunner {
	return func(ctx context.Context, dir, command string, stdin []byte) (extension.CommandResult, error) {
		if dir == "" {
			dir = workspace
		}
		cmd, err := bash.CommandFor(ctx, dir, command)
		if err != nil {
			return extension.CommandResult{Exit: -1}, err
		}
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errOut limited
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err = cmd.Run()
		if cmd.Process == nil {
			sandbox.Release(cmd)
		} else {
			sandbox.Exited(cmd, err)
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

// limited keeps the first extension.MaxHookOutput bytes written and drops the rest.
type limited struct{ b bytes.Buffer }

func (l *limited) Write(p []byte) (int, error) {
	if room := extension.MaxHookOutput - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
