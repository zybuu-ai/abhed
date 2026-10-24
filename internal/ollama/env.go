package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// MacApp is where the Ollama app for macOS installs.
const MacApp = "/Applications/Ollama.app"

// Registry is Ollama's model registry, read for a model's download size.
const Registry = "https://registry.ollama.ai"

// Env is the machine as this package sees it. DefaultEnv is the real one;
// tests replace every part, so none of them reaches a real Ollama.
type Env struct {
	GOOS string
	// LookPath finds a program, as exec.LookPath does.
	LookPath func(string) (string, error)
	// AppPath is the macOS app's location; "" where there is none to look for.
	AppPath string
	// HTTP reaches the local API; Web reaches the registry and download hosts.
	HTTP *http.Client
	Web  *http.Client
	// Registry is the registry's base URL.
	Registry string
	// LogDir holds the log of a server Abhed starts.
	LogDir string
	// StateDir holds the person's standing choice to start the server.
	StateDir string
	// ReadyWait bounds the wait for a started server to answer.
	ReadyWait time.Duration
	// MemTotal is the system's memory; DiskFree the free bytes at a path.
	MemTotal func() (uint64, error)
	DiskFree func(string) (uint64, error)
	// ModelsDir is where Ollama keeps models.
	ModelsDir string
	// OSRelease is the Linux os-release file.
	OSRelease string
	// Environ is the environment a started or installing program inherits.
	Environ func() []string
	// Launch starts a program detached from Abhed: its own process group,
	// output to log. It returns the process id.
	Launch func(path string, args, env []string, log *os.File) (int, error)
	// Run runs a program to its end, for an install.
	Run func(ctx context.Context, path string, args []string, env []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// testMarker is testenv.Marker: set in an isolated test binary and in
// every program it runs. Under it DefaultEnv finds no programs and runs
// none, so no test, and no Abhed binary a test runs, can start, install or
// pull with the real Ollama of the machine the tests run on.
const testMarker = "ABHED_TESTENV"

var errUnderTest = errors.New("a test may not run a real program; give the test an Env of its own")

// DefaultEnv is this machine.
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	models := os.Getenv("OLLAMA_MODELS")
	if models == "" && home != "" {
		models = filepath.Join(home, ".ollama", "models")
	}
	app := ""
	if runtime.GOOS == "darwin" {
		app = MacApp
	}
	return Env{
		GOOS: runtime.GOOS, LookPath: exec.LookPath, AppPath: app,
		HTTP:     &http.Client{Timeout: 0},
		Web:      &http.Client{Timeout: 30 * time.Second},
		Registry: Registry,
		LogDir:   filepath.Join(home, ".abhed", "logs"), StateDir: filepath.Join(home, ".abhed", "state"),
		ReadyWait: 30 * time.Second,
		MemTotal:  memTotal, DiskFree: diskFree, ModelsDir: models,
		OSRelease: "/etc/os-release",
		Environ:   os.Environ,
		Launch:    launch,
		Run:       run,
	}.underTest()
}

// underTest takes away every program when the test marker is set.
func (e Env) underTest() Env {
	if os.Getenv(testMarker) == "" {
		return e
	}
	e.AppPath = ""
	e.LookPath = func(name string) (string, error) { return "", errUnderTest }
	e.Launch = func(string, []string, []string, *os.File) (int, error) { return 0, errUnderTest }
	e.Run = func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error {
		return errUnderTest
	}
	e.Web = &http.Client{Transport: refuseTransport{}}
	return e
}

// refuseTransport answers no request: a test does not reach the registry.
type refuseTransport struct{}

func (refuseTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, errUnderTest }

func run(ctx context.Context, path string, args []string, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, stdout, stderr
	return cmd.Run()
}

// Status is what was found for a target.
type Status struct {
	Target
	// Installed is set when the ollama program or the macOS app is here.
	Installed bool   `json:"installed"`
	Binary    string `json:"binary,omitempty"`
	App       string `json:"app,omitempty"`
	// Running is set when Ollama answers at the target's API.
	Running bool   `json:"running"`
	Version string `json:"version,omitempty"`
	// PortBusy is set when something that is not Ollama holds the port.
	PortBusy bool `json:"port_busy,omitempty"`
	// Pulled is set when the model is among the server's models.
	Pulled bool `json:"pulled"`
	// Ready is Running and Pulled.
	Ready bool `json:"ready"`
	// Next is the step that would make it ready: install, start, pull, or "".
	Next string `json:"next,omitempty"`
	// Problem says what is wrong and how to fix it by hand; nil when ready.
	Problem *Problem `json:"problem,omitempty"`
}

// Check finds whether Ollama is installed, answering and has the model.
// It only reads: it never starts, installs or downloads anything.
func (e Env) Check(ctx context.Context, t Target) Status {
	st := Status{Target: t}
	if t.Loopback {
		st.Binary, st.App = e.binary(), e.app()
		st.Installed = st.Binary != "" || st.App != ""
	}
	ver, err := e.version(ctx, t)
	switch {
	case err == nil:
		st.Running, st.Version = true, ver
	case t.Loopback && !refused(err):
		// Something answered at the port, but not as Ollama does.
		st.PortBusy = e.listening(ctx, t)
	}
	if st.Running && t.Model != "" {
		models, err := e.tags(ctx, t)
		st.Pulled = err == nil && hasModel(models, t.Model)
	}
	st.Ready = st.Running && (st.Pulled || t.Model == "")
	switch {
	case st.Ready:
	case !t.Loopback && !st.Running:
		st.Problem = &Problem{Kind: KindRemote, Message: fmt.Sprintf("Ollama at %s is not answering. It is on another machine, so Abhed will not start or install anything there.", t.API),
			Steps: []string{"On " + t.Host + ": " + t.hostPrefix() + "ollama serve", retryStep(t)}, Retry: true}
	case !t.Loopback:
		st.Problem = &Problem{Kind: KindRemote, Message: fmt.Sprintf("Ollama at %s does not have %s. It is on another machine, so Abhed will not download to it.", t.API, t.Model),
			Steps: []string{"On " + t.Host + ": ollama pull " + t.Model, retryStep(t)}, Retry: true}
	case st.PortBusy:
		// Nothing Abhed may do frees the port; the person decides what holds it.
		st.Problem = e.problem(KindPortBusy, fmt.Sprintf("Something other than Ollama is using port %s, so Ollama cannot start there.", t.Port), t, st)
		st.Problem.Steps = append([]string{portStep(e.GOOS, t.Port)}, st.Problem.Steps...)
	case !st.Installed && !st.Running:
		st.Next = "install"
		st.Problem = e.problem(KindNotInstalled, "Ollama is not installed on this machine.", t, st)
	case !st.Running:
		st.Next = "start"
		st.Problem = e.problem(KindNotRunning, fmt.Sprintf("Ollama is installed but not running at %s.", t.API), t, st)
	default:
		st.Next = "pull"
		st.Problem = e.problem(KindNotPulled, fmt.Sprintf("Ollama is running but does not have %s yet.", t.Model), t, st)
	}
	return st
}

func portStep(goos, port string) string {
	if goos == "windows" {
		return "See what holds the port: netstat -ano | findstr :" + port
	}
	return "See what holds the port: lsof -nP -iTCP:" + port + " -sTCP:LISTEN"
}

// binary is the ollama program: on PATH, or inside the macOS app.
func (e Env) binary() string {
	if e.LookPath != nil {
		if p, err := e.LookPath("ollama"); err == nil {
			return p
		}
	}
	if a := e.app(); a != "" {
		p := filepath.Join(a, "Contents", "Resources", "ollama")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func (e Env) app() string {
	if e.AppPath == "" {
		return ""
	}
	if fi, err := os.Stat(e.AppPath); err == nil && fi.IsDir() {
		return e.AppPath
	}
	return ""
}

// version asks the API for its version, which only Ollama answers.
func (e Env) version(ctx context.Context, t Target) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var v struct {
		Version string `json:"version"`
	}
	if err := e.getJSON(ctx, t.API+"/api/version", &v); err != nil {
		return "", err
	}
	if v.Version == "" {
		return "", errors.New("the answer to /api/version names no version")
	}
	return v.Version, nil
}

// tags lists the server's models.
func (e Env) tags(ctx context.Context, t Target) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var body struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := e.getJSON(ctx, t.API+"/api/tags", &body); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range body.Models {
		out = append(out, m.Name, m.Model)
	}
	return out, nil
}

func hasModel(models []string, tag string) bool {
	want := normalTag(tag)
	return slices.ContainsFunc(models, func(m string) bool { return m != "" && normalTag(m) == want })
}

func (e Env) getJSON(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(into); err != nil {
		return fmt.Errorf("the answer from %s is not Ollama's: %w", url, err)
	}
	return nil
}

func (e Env) client() *http.Client {
	if e.HTTP != nil {
		return e.HTTP
	}
	return http.DefaultClient
}

func (e Env) web() *http.Client {
	if e.Web != nil {
		return e.Web
	}
	return http.DefaultClient
}

// listening reports whether anything accepts a connection at the target.
func (e Env) listening(ctx context.Context, t Target) bool {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(strings.Trim(t.Host, "[]"), t.Port))
	if c != nil {
		_ = c.Close()
	}
	return err == nil
}

// refused reports whether nothing listens: the connection was refused.
func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
