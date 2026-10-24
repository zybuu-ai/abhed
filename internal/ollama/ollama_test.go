package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/testenv"
)

// No test here starts a real Ollama or reaches 127.0.0.1:11434: the API is
// an httptest server, the ollama program a script on a PATH of its own, and
// the guard below fails the run on any dial to a real model service.
func TestMain(m *testing.M) { testenv.Main(m) }

// fakeAPI is an Ollama API: up answers, models are pulled, pull streams.
type fakeAPI struct {
	srv    *httptest.Server
	up     atomic.Bool
	mu     sync.Mutex
	models []string
	pulls  []string
	// pullLines are the lines a pull streams; block holds it open.
	pullLines []string
	block     chan struct{}
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.up.Load() {
			// Not Ollama: something else holding the port.
			http.Error(w, "not here", http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.40.10"}`)
		case "/api/tags":
			f.mu.Lock()
			var ms []map[string]string
			for _, m := range f.models {
				ms = append(ms, map[string]string{"name": m, "model": m})
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
		case "/api/pull":
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.pulls = append(f.pulls, body.Model)
			lines := f.pullLines
			f.mu.Unlock()
			fl, _ := w.(http.Flusher)
			for _, l := range lines {
				fmt.Fprintln(w, l)
				if fl != nil {
					fl.Flush()
				}
			}
			if f.block != nil {
				select {
				case <-f.block:
				case <-r.Context().Done():
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) target(model string) Target {
	tg, ok := Resolve("local", "ollama", f.srv.URL+"/v1", model, "")
	if !ok {
		panic("not a target")
	}
	return tg
}

// fakeBin writes a script named name into dir that records its arguments
// and OLLAMA_HOST to dir/calls, then runs body.
func fakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake programs are shell scripts")
	}
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"" + name + " $* OLLAMA_HOST=$OLLAMA_HOST\" >> " + filepath.Join(dir, "calls") + "\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// testEnv is an Env with nothing of this machine's: programs from bin
// only, no macOS app, a temp home, and fixed memory and disk.
func testEnv(t *testing.T, bin string) Env {
	home := t.TempDir()
	return Env{
		GOOS: "linux",
		LookPath: func(name string) (string, error) {
			p := filepath.Join(bin, name)
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("%s: not found", name)
			}
			return p, nil
		},
		HTTP: &http.Client{}, Web: &http.Client{},
		LogDir: filepath.Join(home, "logs"), StateDir: filepath.Join(home, "state"),
		ReadyWait: 3 * time.Second,
		MemTotal:  func() (uint64, error) { return 64 << 30, nil },
		DiskFree:  func(string) (uint64, error) { return 500 << 30, nil },
		ModelsDir: filepath.Join(home, "models"),
		OSRelease: filepath.Join(home, "os-release"),
		Environ:   func() []string { return []string{"PATH=" + bin + ":/usr/bin:/bin", "OLLAMA_HOST=0.0.0.0:11434"} },
		Launch:    launch,
		Run:       run,
	}
}

// The default Env of a test binary finds and runs nothing real.
func TestDefaultEnvUnderTestRunsNothing(t *testing.T) {
	if testMarker != testenv.Marker {
		t.Fatalf("the marker is %q, testenv's is %q", testMarker, testenv.Marker)
	}
	e := DefaultEnv()
	if _, err := e.LookPath("ollama"); err == nil {
		t.Fatal("a test found the real ollama")
	}
	if _, err := e.Launch("/bin/sh", nil, nil, nil); err == nil {
		t.Fatal("a test could launch a program")
	}
	if err := e.Run(context.Background(), "/bin/sh", nil, nil, nil, nil, nil); err == nil {
		t.Fatal("a test could run a program")
	}
	if _, err := e.Size(context.Background(), "qwen3:8b"); err == nil {
		t.Fatal("a test reached the registry")
	}
	if e.AppPath != "" {
		t.Fatal("a test looks for the macOS app")
	}
}

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		typ, base, host string
		ok, loop        bool
		api             string
	}{
		{"ollama", "", "", true, true, "http://127.0.0.1:11434"},
		{"ollama", "http://10.0.0.2:11434/v1", "", true, false, "http://10.0.0.2:11434"},
		{"openai-compatible", "http://localhost:11434/v1", "", true, true, "http://localhost:11434"},
		{"openai-compatible", "http://127.0.0.1:4000/v1", "", false, false, ""},
		// OLLAMA_HOST does not make another OpenAI-compatible endpoint Ollama's.
		{"openai-compatible", "http://127.0.0.1:11500/v1", "127.0.0.1:11500", false, false, ""},
		{"anthropic", "http://127.0.0.1:11434", "", false, false, ""},
		{"openai-compatible", "::bad", "", false, false, ""},
	} {
		tg, ok := Resolve("p", c.typ, c.base, "m", "http://127.0.0.1:11434/v1")
		if ok != c.ok || ok && (tg.Loopback != c.loop || tg.API != c.api) {
			t.Errorf("%s %s: got %+v %v", c.typ, c.base, tg, ok)
		}
	}
	tg, _ := Resolve("p", "ollama", "http://localhost:11500/v1", "m", "")
	if tg.serveHost() != "127.0.0.1:11500" || tg.hostPrefix() != "OLLAMA_HOST=127.0.0.1:11500 " {
		t.Fatalf("serve host %q prefix %q", tg.serveHost(), tg.hostPrefix())
	}
}

func TestCatalogAndRAM(t *testing.T) {
	if _, ok := Lookup("qwen3:8b"); !ok {
		t.Fatal("qwen3:8b is curated")
	}
	if gb, est := RAMNeededGB("gemma4:31b", 0); gb != 32 || est {
		t.Fatalf("gemma4:31b wants %d (estimate %v)", gb, est)
	}
	if gb, est := RAMNeededGB("other:1b", 4<<30); !est || gb != 7 {
		t.Fatalf("an estimate for 4 GiB: %d %v", gb, est)
	}
	s, ok := Smaller("gemma4:31b", 16)
	if !ok || s.Tag != "qwen3:8b" {
		t.Fatalf("smaller than gemma4:31b in 16 GB: %+v %v", s, ok)
	}
	if _, ok := Smaller("qwen2.5-coder:7b", 64); ok {
		t.Fatal("nothing is smaller than the smallest")
	}
}

func TestCheckStates(t *testing.T) {
	api := newFakeAPI(t)
	bin := t.TempDir()
	env := testEnv(t, bin)
	tg := api.target("qwen3:8b")

	st := env.Check(context.Background(), tg)
	// The fake answers 404 while down: something that is not Ollama holds the port.
	if st.Installed || st.Running || !st.PortBusy || st.Next != "" || st.Problem == nil || st.Problem.Kind != KindPortBusy {
		t.Fatalf("nothing installed, port held: %+v", st)
	}
	fakeBin(t, bin, "ollama", "exec sleep 30")
	if st = env.Check(context.Background(), tg); !st.Installed || !st.PortBusy || st.Problem.Kind != KindPortBusy || !strings.Contains(st.Problem.Steps[0], "lsof") {
		t.Fatalf("port held by something else: %+v", st)
	}
	api.up.Store(true)
	if st = env.Check(context.Background(), tg); !st.Running || st.Pulled || st.Next != "pull" || st.Problem.Kind != KindNotPulled {
		t.Fatalf("running, not pulled: %+v", st)
	}
	want := []string{"ollama pull qwen3:8b"}
	if !strings.Contains(strings.Join(st.Problem.Steps, "\n"), want[0]) {
		t.Fatalf("steps %q lack %q", st.Problem.Steps, want[0])
	}
	api.models = []string{"qwen3:8b"}
	if st = env.Check(context.Background(), tg); !st.Ready || st.Problem != nil {
		t.Fatalf("ready: %+v", st)
	}
	// :latest is the same model as no tag.
	api.models = []string{"llama3:latest"}
	if st = env.Check(context.Background(), api.target("llama3")); !st.Ready {
		t.Fatalf("llama3 is llama3:latest: %+v", st)
	}
}

func TestCheckNothingListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	env := testEnv(t, t.TempDir())
	tg, _ := Resolve("local", "ollama", "http://"+addr+"/v1", "qwen3:8b", "")
	st := env.Check(context.Background(), tg)
	if st.PortBusy || st.Running || st.Problem.Kind != KindNotInstalled {
		t.Fatalf("nothing listening and nothing installed: %+v", st)
	}
	text := st.Problem.Text()
	for _, s := range []string{"1. ", ScriptCommand, "ollama serve", "ollama pull qwen3:8b", "/model local"} {
		if !strings.Contains(text, s) {
			t.Errorf("the steps lack %q:\n%s", s, text)
		}
	}
}

func TestRemoteIsGuidanceOnly(t *testing.T) {
	env := testEnv(t, t.TempDir())
	tg, _ := Resolve("far", "ollama", "http://192.0.2.1:11434/v1", "qwen3:8b", "")
	a := Assist{Env: env, Mode: ModeAsk, Admin: true}
	for _, op := range []string{OpStart, OpInstall, OpPull} {
		if p := a.May(op, tg, Status{Target: tg}); p == nil || p.Kind != KindRemote {
			t.Errorf("%s on another machine: %+v", op, p)
		}
	}
}

func TestMayRefuses(t *testing.T) {
	api := newFakeAPI(t)
	env := testEnv(t, t.TempDir())
	tg := api.target("qwen3:8b")
	st := Status{Target: tg}
	var recorded []agent.OllamaAction
	rec := func(_ agent.EventType, a agent.OllamaAction) { recorded = append(recorded, a) }
	for _, c := range []struct {
		a    Assist
		op   string
		kind string
	}{
		{Assist{Env: env, Mode: ModeOff, Admin: true}, OpStart, KindAssistOff},
		{Assist{Env: env, Mode: ModeLocal, Admin: true}, OpPull, KindAssistOff},
		{Assist{Env: env, Mode: ModeLocal, Admin: true}, OpInstall, KindAssistOff},
		{Assist{Env: env, Mode: ModeAsk, Admin: false}, OpStart, KindNotAdmin},
		{Assist{Env: env, Mode: ModeAsk, Admin: true, InAgent: "ABHED_IN_AGENT is set"}, OpPull, KindInAgent},
	} {
		c.a.Record = rec
		if p := c.a.May(c.op, tg, st); p == nil || p.Kind != c.kind || len(p.Steps) == 0 {
			t.Errorf("%+v %s: %+v", c.a.Mode, c.op, p)
		}
	}
	if p := (Assist{Env: env, Mode: ModeLocal, Admin: true}).May(OpStart, tg, st); p != nil {
		t.Fatalf("local may start: %+v", p)
	}
	// A refused attempt is recorded as refused, and nothing ran.
	a := Assist{Env: env, Mode: ModeOff, Admin: true, Record: rec, Surface: "cli"}
	if _, p := a.Start(context.Background(), tg, st, "asked"); p == nil {
		t.Fatal("off started")
	}
	if p := a.Pull(context.Background(), tg, st, Fit{}, nil); p == nil {
		t.Fatal("off pulled")
	}
	if len(recorded) != 2 || recorded[0].Outcome != agent.OllamaRefused || recorded[0].Op != OpStart || recorded[1].Op != OpPull || recorded[0].By != agent.ByUser {
		t.Fatalf("recorded %+v", recorded)
	}
	if len(api.pulls) != 0 {
		t.Fatalf("a refused pull reached the API: %v", api.pulls)
	}
}

func TestStartLaunchesOnLoopbackAndWaits(t *testing.T) {
	api := newFakeAPI(t)
	bin := t.TempDir()
	marker := filepath.Join(bin, "started")
	fakeBin(t, bin, "ollama", "touch "+marker+"\nexec sleep 30")
	env := testEnv(t, bin)
	// The fake API comes up once the fake program has run.
	go func() {
		for range 200 {
			if _, err := os.Stat(marker); err == nil {
				api.up.Store(true)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	tg := api.target("qwen3:8b")
	var got []agent.OllamaAction
	a := Assist{Env: env, Mode: ModeAsk, Admin: true, Surface: "cli", Record: func(typ agent.EventType, ev agent.OllamaAction) {
		if typ != agent.EvOllamaStart {
			t.Errorf("recorded as %s", typ)
		}
		got = append(got, ev)
	}}
	st := env.Check(context.Background(), tg)
	if !st.Installed || st.Running {
		t.Fatalf("before start: %+v", st)
	}
	st.PortBusy = false
	s, p := a.Start(context.Background(), tg, st, "asked")
	if p != nil {
		t.Fatalf("start: %s", p.Text())
	}
	if s.Version != "0.40.10" || s.PID == 0 || s.Log == "" {
		t.Fatalf("started %+v", s)
	}
	calls, _ := os.ReadFile(filepath.Join(bin, "calls"))
	// Bound to loopback on the target's port, whatever OLLAMA_HOST was.
	if want := "ollama serve OLLAMA_HOST=" + tg.serveHost(); !strings.Contains(string(calls), want) {
		t.Fatalf("calls %q lack %q", calls, want)
	}
	if strings.Contains(string(calls), "0.0.0.0") {
		t.Fatalf("started on every interface: %q", calls)
	}
	if len(got) != 1 || got[0].Outcome != agent.OllamaOK || got[0].PID != s.PID || got[0].Consent != "asked" || !strings.Contains(got[0].Command, "serve") {
		t.Fatalf("recorded %+v", got)
	}
	if fi, err := os.Stat(s.Log); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("log %v %v", fi, err)
	}
	killPID(t, s.PID)
}

// killPID stops a fake server this test started, by the pid it was given.
func killPID(t *testing.T, pid int) {
	t.Helper()
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func TestStartNotReadyAndPortBusy(t *testing.T) {
	api := newFakeAPI(t)
	bin := t.TempDir()
	fakeBin(t, bin, "ollama", "exec sleep 30")
	env := testEnv(t, bin)
	env.ReadyWait = 600 * time.Millisecond
	tg := api.target("qwen3:8b")
	st := Status{Target: tg, Installed: true, Binary: filepath.Join(bin, "ollama")}
	plan, p := env.PlanStart(tg, st)
	if p != nil {
		t.Fatal(p)
	}
	s, p := env.Start(context.Background(), tg, st, plan)
	if p == nil || p.Kind != KindNotReady || !strings.Contains(p.Message, plan.Log) || !p.Retry {
		t.Fatalf("not ready: %+v", p)
	}
	killPID(t, s.PID)

	fakeBin(t, bin, "ollama", "echo 'Error: listen tcp 127.0.0.1:11434: bind: address already in use'\nexit 1")
	s, p = env.Start(context.Background(), tg, st, plan)
	if p == nil || p.Kind != KindPortBusy {
		t.Fatalf("port busy: %+v", p)
	}
	killPID(t, s.PID)
}

func TestPlanStartPrefersTheMacAppOnlyForTheDefaultPort(t *testing.T) {
	bin := t.TempDir()
	fakeBin(t, bin, "open", "")
	env := testEnv(t, bin)
	env.GOOS = "darwin"
	def, _ := Resolve("l", "ollama", "http://127.0.0.1:11434/v1", "m", "")
	st := Status{Target: def, Installed: true, App: "/Applications/Ollama.app", Binary: "/usr/local/bin/ollama"}
	plan, p := env.PlanStart(def, st)
	if p != nil || plan.Display != "open -a Ollama" {
		t.Fatalf("default port on macOS: %+v %v", plan, p)
	}
	other, _ := Resolve("l", "ollama", "http://127.0.0.1:11500/v1", "m", "")
	st.Target = other
	if plan, _ = env.PlanStart(other, st); plan.Display != "OLLAMA_HOST=127.0.0.1:11500 /usr/local/bin/ollama serve" {
		t.Fatalf("another port: %+v", plan)
	}
}

func TestPullStreamsAndChecksFit(t *testing.T) {
	api := newFakeAPI(t)
	api.up.Store(true)
	var handler atomic.Value
	handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/qwen3/manifests/8b":
			fmt.Fprint(w, `{"config":{"size":500},"layers":[{"size":5000000000},{"size":1000}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.Load().(http.HandlerFunc)(w, r)
	}))
	defer reg.Close()
	env := testEnv(t, t.TempDir())
	env.Registry = reg.URL
	tg := api.target("qwen3:8b")
	st := env.Check(context.Background(), tg)

	fit, p := env.CheckFit(context.Background(), tg, st)
	if p != nil || fit.Bytes != 5000001500 || !fit.SizeKnown || len(fit.Warnings) != 0 {
		t.Fatalf("fit %+v %v", fit, p)
	}
	env.MemTotal = func() (uint64, error) { return 4 << 30, nil }
	if fit, _ = env.CheckFit(context.Background(), tg, st); len(fit.Warnings) != 1 || !strings.Contains(fit.Warnings[0], "8 GB") {
		t.Fatalf("short of memory: %+v", fit)
	}
	gemma, _ := Resolve("g", "ollama", api.srv.URL, "gemma4:31b", "")
	handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"config":{"size":1},"layers":[{"size":20000000000}]}`)
	}))
	env.MemTotal = func() (uint64, error) { return 16 << 30, nil }
	if fit, _ = env.CheckFit(context.Background(), gemma, st); fit.Smaller != "qwen3:8b" {
		t.Fatalf("a smaller model for 16 GB: %+v", fit)
	}
	env.DiskFree = func(string) (uint64, error) { return 3 << 30, nil }
	if _, p = env.CheckFit(context.Background(), gemma, st); p == nil || p.Kind != KindDiskShort || !strings.Contains(p.Steps[0], "Free at least") {
		t.Fatalf("short of disk: %+v", p)
	}
	handler.Store(http.NotFoundHandler().(http.HandlerFunc))
	if _, p = env.CheckFit(context.Background(), tg, st); p == nil || p.Kind != KindModelNotFound {
		t.Fatalf("no such model: %+v", p)
	}

	api.pullLines = []string{`{"status":"pulling manifest"}`, `{"status":"downloading","digest":"sha256:a","total":100,"completed":50}`, `{"status":"success"}`}
	var seen []Progress
	var rec []agent.OllamaAction
	a := Assist{Env: env, Mode: ModeAsk, Admin: true, Record: func(_ agent.EventType, ev agent.OllamaAction) { rec = append(rec, ev) }}
	if p := a.Pull(context.Background(), tg, st, Fit{Bytes: 42}, func(pr Progress) { seen = append(seen, pr) }); p != nil {
		t.Fatalf("pull: %s", p.Text())
	}
	if len(seen) != 3 || seen[1].Completed != 50 || len(api.pulls) != 1 || api.pulls[0] != "qwen3:8b" {
		t.Fatalf("progress %+v pulls %v", seen, api.pulls)
	}
	if len(rec) != 1 || rec[0].Outcome != agent.OllamaOK || rec[0].Bytes != 42 || rec[0].Command != "ollama pull qwen3:8b" && !strings.HasSuffix(rec[0].Command, "ollama pull qwen3:8b") {
		t.Fatalf("recorded %+v", rec)
	}

	api.pullLines = []string{`{"error":"pull model manifest: file does not exist"}`}
	if p := env.Pull(context.Background(), tg, st, nil); p == nil || p.Kind != KindModelNotFound {
		t.Fatalf("not found: %+v", p)
	}
	api.pullLines = []string{`{"status":"pulling manifest"}`}
	if p := env.Pull(context.Background(), tg, st, nil); p == nil || p.Kind != KindPullFailed {
		t.Fatalf("no success line: %+v", p)
	}
}

func TestPullIsCancellable(t *testing.T) {
	api := newFakeAPI(t)
	api.up.Store(true)
	api.pullLines = []string{`{"status":"downloading","total":100,"completed":1}`}
	api.block = make(chan struct{})
	defer close(api.block)
	env := testEnv(t, t.TempDir())
	tg := api.target("qwen3:8b")
	ctx, cancel := context.WithCancel(context.Background())
	var rec []agent.OllamaAction
	a := Assist{Env: env, Mode: ModeAsk, Admin: true, Record: func(_ agent.EventType, ev agent.OllamaAction) { rec = append(rec, ev) }}
	done := make(chan *Problem)
	go func() {
		done <- a.Pull(ctx, tg, Status{Target: tg, Running: true}, Fit{}, func(Progress) { cancel() })
	}()
	select {
	case p := <-done:
		if p == nil || p.Kind != KindCancelled {
			t.Fatalf("cancelled pull: %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pull did not stop when cancelled")
	}
	if len(rec) != 1 || rec[0].Outcome != agent.OllamaCancelled {
		t.Fatalf("recorded %+v", rec)
	}
}

func TestPlanInstall(t *testing.T) {
	bin := t.TempDir()
	env := testEnv(t, bin)
	env.GOOS = "darwin"
	if p := env.PlanInstall(); p.Kind != InstallOpen || p.Display != "open "+MacDownload || p.SizeURL != MacDMG {
		t.Fatalf("macOS without Homebrew: %+v", p)
	}
	fakeBin(t, bin, "brew", "")
	if p := env.PlanInstall(); p.Kind != InstallRun || p.Display != "brew install ollama" || !strings.Contains(p.Source, "formulae.brew.sh") {
		t.Fatalf("macOS with Homebrew: %+v", p)
	}
	os.Remove(filepath.Join(bin, "brew"))
	env.GOOS = "linux"
	if p := env.PlanInstall(); p.Kind != InstallScript || p.Display != ScriptCommand || !p.NeedsTerminal {
		t.Fatalf("linux: %+v", p)
	}
	_ = os.WriteFile(env.OSRelease, []byte("NAME=\"Arch Linux\"\nID=arch\n"), 0o644)
	if p := env.PlanInstall(); p.Kind != InstallManual || !strings.Contains(p.Manual[0], "sudo pacman -S ollama") {
		t.Fatalf("arch: %+v", p)
	}
	env.GOOS = "windows"
	if p := env.PlanInstall(); p.Kind != InstallManual || !strings.Contains(p.Manual[0], WindowsPage) {
		t.Fatalf("windows: %+v", p)
	}
}

func TestInstallRunsAsThePersonAndRefusesTheScriptWithoutATerminal(t *testing.T) {
	bin := t.TempDir()
	fakeBin(t, bin, "brew", "echo installed")
	fakeBin(t, bin, "sh", "exit 0")
	env := testEnv(t, bin)
	env.GOOS = "darwin"
	tg, _ := Resolve("l", "ollama", "http://127.0.0.1:11434/v1", "m", "")
	var rec []agent.OllamaAction
	a := Assist{Env: env, Mode: ModeAsk, Admin: true, Record: func(_ agent.EventType, ev agent.OllamaAction) { rec = append(rec, ev) }}
	var out bytes.Buffer
	if p := a.Install(context.Background(), tg, Status{Target: tg}, env.PlanInstall(), false, nil, &out, io.Discard); p != nil {
		t.Fatalf("brew: %s", p.Text())
	}
	calls, _ := os.ReadFile(filepath.Join(bin, "calls"))
	if !strings.Contains(string(calls), "brew install ollama") || strings.Contains(string(calls), "sudo") {
		t.Fatalf("calls %q", calls)
	}
	if len(rec) != 1 || rec[0].Outcome != agent.OllamaOK || rec[0].Command != "brew install ollama" {
		t.Fatalf("recorded %+v", rec)
	}

	env.GOOS = "linux"
	os.Remove(filepath.Join(bin, "brew"))
	a.Env = env
	if p := a.Install(context.Background(), tg, Status{Target: tg}, env.PlanInstall(), false, nil, io.Discard, io.Discard); p == nil || p.Kind != KindNeedsTerminal {
		t.Fatalf("script without a terminal: %+v", p)
	}
	if rec[1].Outcome != agent.OllamaManual {
		t.Fatalf("recorded %+v", rec[1])
	}
	a.Mode = ModeLocal
	if p := a.Install(context.Background(), tg, Status{Target: tg}, env.PlanInstall(), true, nil, io.Discard, io.Discard); p == nil || p.Kind != KindAssistOff {
		t.Fatalf("local installs nothing: %+v", p)
	}
}

func TestInstallSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method %s", r.Method)
		}
		w.Header().Set("Content-Length", "221053130")
	}))
	defer srv.Close()
	env := testEnv(t, t.TempDir())
	p := InstallPlan{SizeURL: srv.URL + "/Ollama.dmg"}
	env.InstallSize(context.Background(), &p)
	if !strings.Contains(p.SizeNote, "221 MB") {
		t.Fatalf("size %q", p.SizeNote)
	}
	p = InstallPlan{SizeURL: "http://127.0.0.1:1/none"}
	env.InstallSize(context.Background(), &p)
	if p.SizeNote != "not known until the download starts" {
		t.Fatalf("unknown size %q", p.SizeNote)
	}
}

func TestReportAsksNothingOfTheNetworkWhenAssistIsOff(t *testing.T) {
	api := newFakeAPI(t)
	api.up.Store(true)
	var hits atomic.Int32
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"config":{"size":1},"layers":[{"size":10}]}`)
	}))
	defer reg.Close()
	env := testEnv(t, t.TempDir())
	env.Registry = reg.URL
	tg := api.target("qwen3:8b")
	for _, mode := range []string{ModeOff, ModeLocal} {
		r := Assist{Env: env, Mode: mode, Admin: true}.Report(context.Background(), tg)
		if r.CanAct || r.Refusal == nil || r.Pull != nil || r.Problem == nil || r.Next != "pull" {
			t.Fatalf("%s: %+v", mode, r)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the registry was asked %d times with assist off or local", hits.Load())
	}
	r := Assist{Env: env, Mode: ModeAsk, Admin: true}.Report(context.Background(), tg)
	if !r.CanAct || r.Pull == nil || r.Pull.Bytes != 11 || hits.Load() != 1 {
		t.Fatalf("ask: %+v", r)
	}
	if b, _ := json.Marshal(r); !strings.Contains(string(b), `"next":"pull"`) || !strings.Contains(string(b), `"catalog"`) {
		t.Fatalf("json %s", b)
	}
}

func TestStartAlwaysIsRevocable(t *testing.T) {
	env := testEnv(t, t.TempDir())
	if env.StartAlways() {
		t.Fatal("always before it was chosen")
	}
	if err := env.SetStartAlways(true); err != nil || !env.StartAlways() {
		t.Fatalf("set: %v", err)
	}
	if fi, _ := os.Stat(env.consentFile()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if err := env.SetStartAlways(false); err != nil || env.StartAlways() {
		t.Fatalf("revoke: %v", err)
	}
	if err := env.SetStartAlways(false); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]string{"": ModeAsk, "ASK": ModeAsk, "local": ModeLocal, "off": ModeOff} {
		if got, ok := ParseMode(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	if _, ok := ParseMode("auto"); ok {
		t.Fatal("auto is not a mode")
	}
}

func TestManifestPath(t *testing.T) {
	for in, want := range map[string]string{"qwen3:8b": "/v2/library/qwen3/manifests/8b", "llama3": "/v2/library/llama3/manifests/latest",
		"me/model:q4": "/v2/me/model/manifests/q4"} {
		if got, err := manifestPath(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"a/b/c:1", "x y:1", ":1"} {
		if _, err := manifestPath(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

// A server that serves the OpenAI API Ollama also serves, but not
// /api/version, is running, not something else holding the port.
func TestCheckAcceptsAnOpenAIStandIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"stub-model"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	env := testEnv(t, t.TempDir())
	tg, _ := Resolve("p", "ollama", srv.URL+"/v1", "stub-model", "")
	st := env.Check(context.Background(), tg)
	if !st.Running || st.PortBusy || !st.Pulled || !st.Ready || st.Problem != nil {
		t.Fatalf("an OpenAI stand-in: %+v", st)
	}
	tg.Model = "other:1b"
	if st = env.Check(context.Background(), tg); !st.Running || st.Pulled || st.Next != "pull" {
		t.Fatalf("a model it does not list: %+v", st)
	}
}
