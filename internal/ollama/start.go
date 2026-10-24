package ollama

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StartPlan is how the server would be started, shown before it is.
type StartPlan struct {
	// Display is the exact command, as the person is shown it.
	Display string `json:"command"`
	// Log is where the server's output goes, when Abhed starts it itself.
	Log  string   `json:"log,omitempty"`
	path string   // the program
	args []string // its arguments
	env  []string // what is added to the environment
}

// PlanStart says how the server would be started for t, or a Problem when
// it cannot be started from here.
func (e Env) PlanStart(t Target, st Status) (StartPlan, *Problem) {
	if !t.Loopback {
		return StartPlan{}, &Problem{Kind: KindRemote, Message: fmt.Sprintf("%s is on another machine; Abhed starts Ollama only on this one.", t.API),
			Steps: []string{"On " + t.Host + ": " + t.hostPrefix() + "ollama serve", retryStep(t)}, Retry: true}
	}
	if !st.Installed {
		return StartPlan{}, e.problem(KindNotInstalled, "Ollama is not installed on this machine, so it cannot be started.", t, st)
	}
	// The app listens only on the default address, so it is used only for that.
	if e.GOOS == "darwin" && st.App != "" && t.Default() {
		if open, err := e.look("open"); err == nil {
			return StartPlan{Display: "open -a Ollama", path: open, args: []string{"-a", "Ollama"}}, nil
		}
	}
	if st.Binary == "" {
		return StartPlan{}, e.problem(KindNotInstalled, "The ollama program was not found, so the server cannot be started.", t, st)
	}
	logPath := filepath.Join(e.LogDir, "ollama-serve.log")
	host := t.serveHost()
	return StartPlan{Display: "OLLAMA_HOST=" + host + " " + st.Binary + " serve", Log: logPath,
		path: st.Binary, args: []string{"serve"}, env: []string{"OLLAMA_HOST=" + host}}, nil
}

func (e Env) look(name string) (string, error) {
	if e.LookPath == nil {
		return "", os.ErrNotExist
	}
	return e.LookPath(name)
}

// Started is a server Abhed started.
type Started struct {
	PID     int    `json:"pid,omitempty"`
	Version string `json:"version"`
	Log     string `json:"log,omitempty"`
}

// Start starts the server as plan says, detached as the person's own
// process in its own process group, and waits up to ReadyWait for it to
// answer. Abhed does not stop it on exit: it is the person's server.
func (e Env) Start(ctx context.Context, t Target, st Status, plan StartPlan) (Started, *Problem) {
	var out Started
	if plan.path == "" {
		return out, e.problem(KindStartFailed, "There is no command to start Ollama with.", t, st)
	}
	var logFile *os.File
	var logStart int64
	if plan.Log != "" {
		if err := os.MkdirAll(filepath.Dir(plan.Log), 0o700); err != nil {
			return out, e.problem(KindStartFailed, "The log folder for Ollama could not be made: "+err.Error(), t, st)
		}
		f, err := os.OpenFile(plan.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return out, e.problem(KindStartFailed, "The log file for Ollama could not be opened: "+err.Error(), t, st)
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			logStart = fi.Size()
		}
		logFile, out.Log = f, plan.Log
	}
	env := append(withoutKey(e.environ(), "OLLAMA_HOST"), plan.env...)
	pid, err := e.Launch(plan.path, plan.args, env, logFile)
	if err != nil {
		return out, e.problem(KindStartFailed, fmt.Sprintf("Ollama could not be started (%s): %v", plan.Display, err), t, st)
	}
	out.PID = pid
	wait := e.ReadyWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		if v, err := e.version(ctx, t); err == nil {
			out.Version = v
			return out, nil
		}
		if ctx.Err() != nil {
			return out, &Problem{Kind: KindCancelled, Message: "Stopped waiting for Ollama to start.", Steps: e.Steps(t, st), Retry: true}
		}
		if busy := portBusyIn(plan.Log, logStart); busy {
			st.PortBusy = true
			p := e.problem(KindPortBusy, fmt.Sprintf("Ollama could not listen on port %s: something else is using it.", t.Port), t, st)
			p.Steps = append([]string{portStep(e.GOOS, t.Port)}, p.Steps...)
			return out, p
		}
		if time.Now().After(deadline) {
			p := e.problem(KindNotReady, fmt.Sprintf("Ollama was started but did not answer at %s within %s.", t.API, wait.Round(time.Second)), t, st)
			if plan.Log != "" {
				p.Message += " Its log is " + plan.Log + "."
			}
			return out, p
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// portBusyIn reports whether the server's log, from offset on, says the
// address is taken.
func portBusyIn(path string, offset int64) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := f.ReadAt(buf, offset)
	return bytes.Contains(bytes.ToLower(buf[:n]), []byte("address already in use"))
}

func (e Env) environ() []string {
	if e.Environ == nil {
		return os.Environ()
	}
	return e.Environ()
}

// withoutKey drops key from an environment.
func withoutKey(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, key) {
			out = append(out, kv)
		}
	}
	return out
}
