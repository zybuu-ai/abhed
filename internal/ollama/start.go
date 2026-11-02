package ollama

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// StartPlan is how the server would be started, shown before it is.
type StartPlan struct {
	// Display is the exact command, as the person is shown it.
	Display string `json:"command"`
	// Log is where the server's output goes, when Abhed starts it itself.
	Log string `json:"log,omitempty"`
	// Program is the program run: the resolved ollama binary, or the app.
	// A standing "always start" is kept for this path only.
	Program string `json:"program,omitempty"`
	// AppBinds is set when the macOS app is opened: it listens where its
	// own settings say, which Abhed does not control.
	AppBinds bool     `json:"app_binds,omitempty"`
	path     string   // what is executed
	args     []string // its arguments
	env      []string // what is added to the environment
	// handsOff is set for a launcher that exits once it has handed off
	// (open), so its exit is not the server failing.
	handsOff bool
}

// PlanStart says how the server would be started for t, or a Problem when
// it cannot be started from here. With the ollama program present it is
// always `ollama serve` with OLLAMA_HOST forced to the target's loopback
// address. Only when the macOS app alone is installed is the app opened,
// by its path, and only for the default address it listens on.
func (e Env) PlanStart(t Target, st Status) (StartPlan, *Problem) {
	if !t.Loopback {
		return StartPlan{}, &Problem{Kind: KindRemote, Message: fmt.Sprintf("%s is on another machine; Abhed starts Ollama only on this one.", t.API),
			Steps: []string{"On " + t.Host + ": " + t.hostPrefix() + "ollama serve", retryStep(t)}, Retry: true}
	}
	if !st.Installed {
		return StartPlan{}, e.problem(KindNotInstalled, "Ollama is not installed on this machine, so it cannot be started.", t, st)
	}
	if st.Binary != "" {
		logPath := filepath.Join(e.LogDir, "ollama-serve.log")
		host := t.serveHost()
		env := e.serveEnv(host)
		return StartPlan{Display: commandLineOf(env) + " " + st.Binary + " serve", Log: logPath, Program: st.Binary,
			path: st.Binary, args: []string{"serve"}, env: env}, nil
	}
	if e.GOOS == "darwin" && st.App != "" {
		if !t.Default() {
			return StartPlan{}, e.problem(KindStartFailed, "Only the Ollama app is installed, and it listens on its own address, not on port "+t.Port+".", t, st)
		}
		if open, err := e.look("open"); err == nil {
			return StartPlan{Display: "open " + st.App, Program: st.App, AppBinds: true, path: open, args: []string{st.App}, handsOff: true}, nil
		}
	}
	return StartPlan{}, e.problem(KindNotInstalled, "The ollama program was not found, so the server cannot be started.", t, st)
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

// starting serializes starts per target, so two requests cannot launch two
// servers on one port.
var starting sync.Map // API -> chan struct{}, one slot

// startSlot is the per-target slot a start holds; waiting for it ends with
// ctx, so a request whose client has gone stops waiting behind another.
func startSlot(api string) chan struct{} {
	m, _ := starting.LoadOrStore(api, make(chan struct{}, 1))
	return m.(chan struct{})
}

// Start starts the server as plan says, detached as the person's own
// process in its own process group with a minimal environment, and waits up
// to ReadyWait for it to answer, or for it to exit. Abhed does not stop it
// on exit: it is the person's server.
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
	env := append(MinimalEnv(e.environ()), plan.env...)
	pid, exited, err := e.Launch(plan.path, plan.args, env, logFile)
	if err != nil {
		return out, e.problem(KindStartFailed, fmt.Sprintf("Ollama could not be started (%s): %v", plan.Display, err), t, st)
	}
	out.PID = pid
	wait := e.ReadyWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if v, err := e.version(ctx, t); err == nil {
			out.Version = v
			return out, nil
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
			return out, &Problem{Kind: KindCancelled, Message: "Stopped waiting for Ollama to start.", Steps: e.Steps(t, st), Retry: true}
		case werr := <-exited:
			exited = nil
			if plan.handsOff && werr == nil {
				continue // open handed off to the app; keep waiting for it
			}
			if portBusyIn(plan.Log, logStart) {
				continue // said as port busy at the top of the loop
			}
			// A server that exits before it answers has failed; say how, from its log.
			msg := fmt.Sprintf("Ollama exited before it answered (%s)", plan.Display)
			if werr != nil {
				msg += ": " + werr.Error()
			}
			if tail := logTail(plan.Log, logStart); tail != "" {
				msg += ". The end of its log:\n" + tail
			}
			return out, e.problem(KindStartFailed, msg, t, st)
		case <-tick.C:
		}
	}
}

// logTail is the last lines the server wrote to its log from offset on.
func logTail(path string, offset int64) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := f.ReadAt(buf, offset)
	lines := strings.Split(strings.TrimSpace(string(bytes.ToValidUTF8(buf[:n], nil))), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// keepEnv are the variables a started or installing program is given: the
// basics, the proxy and certificate settings a download needs behind a
// proxy, and the GPU selection and library path a Linux server needs to use
// the GPU as the person's own `ollama serve` would. The rest of Abhed's
// environment, which on a server can hold provider keys, Abhed's tokens and
// store addresses, is not passed on.
var keepEnv = []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "USER", "LOGNAME", "SHELL",
	"SYSTEMROOT", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "PROGRAMDATA", "XDG_RUNTIME_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"CUDA_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "GPU_DEVICE_ORDINAL", "LD_LIBRARY_PATH"}

// MinimalEnv keeps from env only keepEnv (in either case, so http_proxy
// too), the LC_* locale variables, the HOMEBREW_* ones brew reads, and
// OLLAMA_* other than OLLAMA_HOST.
func MinimalEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		switch {
		case u == "OLLAMA_HOST":
		case strings.HasPrefix(u, "OLLAMA_"), strings.HasPrefix(u, "LC_"), strings.HasPrefix(u, "HOMEBREW_"):
			out = append(out, kv)
		default:
			for _, keep := range keepEnv {
				if strings.EqualFold(k, keep) {
					out = append(out, kv)
					break
				}
			}
		}
	}
	return out
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

// serveEnv is what a server Abhed starts is given: the target's loopback
// address and the models folder Abhed checks (OLLAMA_MODELS when the person
// set it, else ~/.ollama/models), so the server and the check read the same
// models.
func (e Env) serveEnv(host string) []string {
	env := []string{"OLLAMA_HOST=" + host}
	if e.ModelsDir != "" {
		env = append(env, "OLLAMA_MODELS="+e.ModelsDir)
	}
	// The configured upstream proxy, for the server's own model pulls. One
	// with credentials is not passed: they would sit in the server's
	// environment and in this command line; the operator configures Ollama's.
	if u := egress.CurrentUpstream(); u != nil && u.Credentials == nil {
		np := strings.Join(append([]string{"localhost", "127.0.0.1", "::1"}, u.NoProxy...), ",")
		env = append(env, "HTTPS_PROXY="+u.URL.String(), "HTTP_PROXY="+u.URL.String(), "NO_PROXY="+np)
	}
	return env
}

// commandLineOf is assignments as a shell shows them, a value with a space
// or a quote quoted.
func commandLineOf(env []string) string {
	out := make([]string, len(env))
	for i, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.ContainsAny(v, " '\"$`\\") {
			v = "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
		}
		out[i] = k + "=" + v
	}
	return strings.Join(out, " ")
}
