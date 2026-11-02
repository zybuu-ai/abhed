package app

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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ollama"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// appOllama is the machine the CLI's and ACP's assist see in these tests:
// an Ollama API that refuses connections until it is "started", a
// registry, and an ollama program that is only a file. Launch runs
// nothing. No test starts a real Ollama or reaches 127.0.0.1:11434.
type appOllama struct {
	url      string
	api, reg *httptest.Server
	once     sync.Once
	up       atomic.Bool
	mu       sync.Mutex
	pulled   []string
	launched []string
	// regHits counts requests to the fake registry.
	regHits atomic.Int32
	env     ollama.Env
	// program is the fake ollama's resolved path.
	program string
}

func newAppOllama(t *testing.T) *appOllama {
	t.Helper()
	f := &appOllama{}
	f.api = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.40.10"}`)
		case "/api/tags":
			f.mu.Lock()
			var ms []map[string]string
			for _, m := range f.pulled {
				ms = append(ms, map[string]string{"name": m})
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
		case "/api/pull":
			var b struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			fmt.Fprintln(w, `{"status":"downloading","total":100,"completed":40}`)
			fmt.Fprintln(w, `{"status":"success"}`)
			f.mu.Lock()
			f.pulled = append(f.pulled, b.Model)
			f.mu.Unlock()
		}
	}))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.url = "http://" + l.Addr().String()
	_ = l.Close()
	t.Cleanup(func() {
		if f.up.Load() {
			f.api.Close()
		}
	})
	f.reg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.regHits.Add(1)
		fmt.Fprint(w, `{"config":{"size":10},"layers":[{"size":5000}]}`)
	}))
	t.Cleanup(f.reg.Close)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ollama"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	f.program = filepath.Join(bin, "ollama")
	f.env = ollama.Env{
		GOOS: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ollama" {
				return filepath.Join(bin, "ollama"), nil
			}
			return "", fmt.Errorf("%s: not found", name)
		},
		HTTP: &http.Client{}, Web: &http.Client{}, Registry: f.reg.URL,
		LogDir: filepath.Join(home, "logs"), StateDir: filepath.Join(home, "state"), ReadyWait: 2 * time.Second,
		MemTotal: func() (uint64, error) { return 64 << 30, nil }, DiskFree: func(string) (uint64, error) { return 500 << 30, nil },
		ModelsDir: home, Environ: func() []string { return nil },
		Launch: func(path string, args, env []string, _ *os.File) (int, <-chan error, error) {
			f.mu.Lock()
			f.launched = append(f.launched, strings.Join(append(append([]string{}, env...), append([]string{path}, args...)...), " "))
			f.mu.Unlock()
			f.start(t)
			return 4242, nil, nil
		},
		Listening: func(context.Context, string) bool { return false },
	}
	old := ollamaEnv
	ollamaEnv = func() ollama.Env { return f.env }
	t.Cleanup(func() { ollamaEnv = old })
	return f
}

func (f *appOllama) start(t *testing.T) {
	f.once.Do(func() {
		l, err := net.Listen("tcp", strings.TrimPrefix(f.url, "http://"))
		if err != nil {
			t.Errorf("the fake API's port was taken: %v", err)
			return
		}
		f.api.Listener = l
		f.api.Start()
		f.up.Store(true)
	})
}

func (f *appOllama) provider() config.ProviderConfig {
	return config.ProviderConfig{Type: "ollama", BaseURL: f.url + "/v1", Model: "qwen3:8b", ContextWindow: 8192}
}

// ollamaEnvFor is recordedEnv with a provider "olm" the fake serves.
func ollamaEnvFor(t *testing.T, f *appOllama, assist string, answers ...string) (*cmdEnv, *scriptedSurface, func() []agent.OllamaAction) {
	t.Helper()
	cfg := config.Default()
	cfg.Model.Providers["olm"] = f.provider()
	cfg.Model.Ollama.Assist = assist
	env, surface, events := recordedEnv(t, cfg, policy.ModeDefault, answers...)
	return env, surface, func() []agent.OllamaAction {
		var out []agent.OllamaAction
		for _, ev := range events() {
			switch ev.Type {
			case agent.EvOllamaStart, agent.EvOllamaInstall, agent.EvOllamaPull:
				if ev.Actor != agent.ActorUser {
					t.Errorf("%s recorded as %s", ev.Type, ev.Actor)
				}
				var a agent.OllamaAction
				_ = json.Unmarshal(ev.Payload, &a)
				out = append(out, a)
			}
		}
		return out
	}
}

func outcomes(acts []agent.OllamaAction) string {
	var s []string
	for _, a := range acts {
		s = append(s, a.Op+":"+a.Outcome)
	}
	return strings.Join(s, ",")
}

// A yes to start, then a yes to the download, makes the model ready; each
// is recorded as the person's, with the command shown.
func TestOllamaFlowAsksThenStartsAndPulls(t *testing.T) {
	f := newAppOllama(t)
	env, surface, acts := ollamaEnvFor(t, f, "", "start", ui.ChoiceYes)
	if !env.st.ensureOllamaFor(context.Background(), surface, "olm", true) {
		t.Fatalf("not ready:\n%s", surface.text())
	}
	if got := outcomes(acts()); got != "start:ok,pull:ok" {
		t.Fatalf("recorded %s", got)
	}
	if a := acts()[0]; a.Consent != "asked" || a.PID != 4242 || !strings.Contains(a.Command, "serve") {
		t.Fatalf("start %+v", a)
	}
	if len(surface.asked) != 2 || !strings.Contains(dialogText(surface.asked[0]), "OLLAMA_HOST=127.0.0.1:") || !strings.Contains(dialogText(surface.asked[1]), "Size: 5 KB") {
		t.Fatalf("asked %+v", surface.asked)
	}
	if surface.asked[0].Default != ui.ChoiceNo || surface.asked[1].Default != ui.ChoiceNo {
		t.Fatal("a dialog defaults to yes")
	}
	if len(f.launched) != 1 || !strings.Contains(f.launched[0], "OLLAMA_HOST=127.0.0.1:") {
		t.Fatalf("launched %v", f.launched)
	}
}

func dialogText(d ui.DialogSpec) string {
	var b strings.Builder
	b.WriteString(d.Title + "\n" + d.Ask + "\n")
	for _, x := range d.Body {
		b.WriteString(x.Text + "\n")
	}
	return b.String()
}

// No answer is no: nothing runs, the steps are shown, and the decline is recorded.
func TestOllamaFlowDeclinedRunsNothing(t *testing.T) {
	f := newAppOllama(t)
	env, surface, acts := ollamaEnvFor(t, f, "")
	if env.st.ensureOllamaFor(context.Background(), surface, "olm", true) {
		t.Fatal("ready without starting")
	}
	if len(f.launched) != 0 || outcomes(acts()) != "start:declined" {
		t.Fatalf("launched %v, recorded %s", f.launched, outcomes(acts()))
	}
	if text := surface.text(); !strings.Contains(text, "Nothing was run.") || !strings.Contains(text, "1. OLLAMA_HOST=127.0.0.1:") {
		t.Fatalf("shown:\n%s", text)
	}
}

// "Always" starts without asking the next time, until it is forgotten.
func TestOllamaFlowAlwaysStartIsKeptAndForgotten(t *testing.T) {
	f := newAppOllama(t)
	env, surface, acts := ollamaEnvFor(t, f, "local", "always")
	if env.st.ensureOllamaFor(context.Background(), surface, "olm", true) {
		t.Fatal("ready without the model")
	}
	// The pull refused under local is recorded, as the server and Studio record it.
	if !f.env.StartAlways(f.program) || outcomes(acts()) != "start:ok,pull:refused" {
		t.Fatalf("always %v, recorded %s", f.env.StartAlways(f.program), outcomes(acts()))
	}
	// local: the pull is not offered; the steps say how to do it.
	if text := surface.text(); !strings.Contains(text, "model.ollama.assist is local") || !strings.Contains(text, "ollama pull qwen3:8b") {
		t.Fatalf("shown:\n%s", text)
	}
	// A different ollama on PATH is asked about again.
	if f.env.StartAlways("/tmp/elsewhere/ollama") {
		t.Fatal("always covered another program")
	}
	if _, err := slashOllama(context.Background(), env, []string{"forget"}); err != nil || f.env.StartAlways(f.program) {
		t.Fatalf("forget: %v, still %v", err, f.env.StartAlways(f.program))
	}
}

func TestOllamaFlowAssistOffOffersNothing(t *testing.T) {
	f := newAppOllama(t)
	env, surface, acts := ollamaEnvFor(t, f, "off", "start", ui.ChoiceYes)
	if env.st.ensureOllamaFor(context.Background(), surface, "olm", true) {
		t.Fatal("ready")
	}
	for _, d := range surface.asked {
		if d.Title == "Start Ollama?" {
			t.Fatal("assist off offered to start")
		}
	}
	if len(f.launched) != 0 || outcomes(acts()) != "start:refused" || acts()[0].Problem != "assist_off" {
		t.Fatalf("launched %v, recorded %v", f.launched, acts())
	}
	if !strings.Contains(surface.text(), "model.ollama.assist is off") {
		t.Fatalf("shown:\n%s", surface.text())
	}
}

// Without a terminal to answer at, the flow tells and asks nothing, so
// piped input meant for the session is not taken as an answer.
func TestOllamaFlowTellsWithoutATerminal(t *testing.T) {
	f := newAppOllama(t)
	env, surface, _ := ollamaEnvFor(t, f, "")
	t2, _ := ollama.Resolve("olm", "ollama", f.url+"/v1", "qwen3:8b", "")
	fl := ollamaFlow{ui: surface, a: env.st.cliOllama("cli"), tell: true}
	if fl.ensure(context.Background(), t2) || len(surface.asked) != 0 || len(f.launched) != 0 {
		t.Fatalf("asked %d, launched %v", len(surface.asked), f.launched)
	}
	if !strings.Contains(surface.text(), "abhed ollama check olm") {
		t.Fatalf("shown:\n%s", surface.text())
	}
}

// /model to a model whose Ollama is not ready asks whether to switch anyway,
// and stays without a yes.
func TestModelSwitchToAnOllamaModelThatIsNotReady(t *testing.T) {
	f := newAppOllama(t)
	env, surface, _ := ollamaEnvFor(t, f, "off")
	if err := switchModel(context.Background(), env, "olm"); err != nil {
		t.Fatal(err)
	}
	if env.st.appCfg.Model.Default == "olm" || !strings.Contains(surface.text(), "unchanged; /ollama checks it again") {
		t.Fatalf("default %s, shown:\n%s", env.st.appCfg.Model.Default, surface.text())
	}
}

// An agent's command cannot reach the assist through a nested abhed.
func TestOllamaSubcommandIsRefusedInAnAgentsCommand(t *testing.T) {
	for _, verb := range []string{"check", "install", "forget"} {
		if selfAdmin([]string{"ollama", verb}) == "" {
			t.Errorf("abhed ollama %s is not refused in an agent's command", verb)
		}
	}
	for _, verb := range []string{"status", "models"} {
		if selfAdmin([]string{"ollama", verb}) != "" {
			t.Errorf("abhed ollama %s only reads, and is refused", verb)
		}
	}
	old := inAgentCommand
	inAgentCommand = func() string { return "ABHED_IN_AGENT is set" }
	t.Cleanup(func() { inAgentCommand = old })
	f := newAppOllama(t)
	env, surface, _ := ollamaEnvFor(t, f, "", "start")
	if env.st.ensureOllamaFor(context.Background(), surface, "olm", true) || len(f.launched) != 0 {
		t.Fatalf("an agent's command started Ollama: %v", f.launched)
	}
}

// abhed ollama check asks at a terminal, records in a session of its own,
// and abhed ollama status only reads.
func TestOllamaSubcommand(t *testing.T) {
	f := newAppOllama(t)
	ws := t.TempDir()
	cfg := `{"model":{"default":"olm","providers":{"olm":{"type":"ollama","base_url":"` + f.url + `/v1","model":"qwen3:8b","context_window":8192}}}}`
	acpModelsEnv(t, cfg)
	var out, errb bytes.Buffer
	if code := ollamaCmd(ws, []string{"status"}, config.TrustAsStored, strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(out.String(), "not running") {
		t.Fatalf("status: %d %s %s", code, out.String(), errb.String())
	}
	if len(f.launched) != 0 {
		t.Fatal("status started Ollama")
	}
	// As at a terminal: 1 starts it, then 1 (yes) to the download.
	old := ollamaTerminal
	ollamaTerminal = func(io.Reader, io.Writer) bool { return true }
	t.Cleanup(func() { ollamaTerminal = old })
	out.Reset()
	if code := ollamaCmd(ws, []string{"check"}, config.TrustAsStored, strings.NewReader("1\n1\n"), &out, &errb); code != 0 {
		t.Fatalf("check: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if len(f.launched) != 1 || !strings.Contains(out.String(), "is ready at") || !strings.Contains(errb.String(), "Recorded as session s-") {
		t.Fatalf("launched %v\nOUT:%s\nERR:%s", f.launched, out.String(), errb.String())
	}
	out.Reset()
	if code := ollamaCmd(ws, []string{"models"}, config.TrustAsStored, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "gemma4:31b") {
		t.Fatalf("models: %s", out.String())
	}
}

// From a pipe, abhed ollama check and install run nothing, whatever is
// piped in: they say what is wrong and the steps.
func TestOllamaSubcommandFromAPipeRunsNothing(t *testing.T) {
	f := newAppOllama(t)
	ws := t.TempDir()
	cfg := `{"model":{"default":"olm","providers":{"olm":{"type":"ollama","base_url":"` + f.url + `/v1","model":"qwen3:8b","context_window":8192}}}}`
	acpModelsEnv(t, cfg)
	for _, verb := range []string{"check", "install"} {
		var out, errb bytes.Buffer
		if code := ollamaCmd(ws, []string{verb}, config.TrustAsStored, strings.NewReader("1\n1\n1\n2\n"), &out, &errb); code != 1 {
			t.Fatalf("%s from a pipe: %d\n%s", verb, code, out.String())
		}
		if !strings.Contains(out.String(), "ollama serve") || strings.Contains(out.String(), "Start Ollama?") {
			t.Fatalf("%s from a pipe asked:\n%s", verb, out.String())
		}
	}
	if len(f.launched) != 0 {
		t.Fatalf("a pipe started Ollama: %v", f.launched)
	}
	if ollamaTerminal(strings.NewReader(""), &bytes.Buffer{}) {
		t.Fatal("a reader and a buffer count as a terminal")
	}
}

// Studio's methods: status, a start that is recorded, a pull with progress
// notifications, and the switch reply that says the model is not ready.
func TestStudioOllama(t *testing.T) {
	f := newAppOllama(t)
	m := newScriptModel(t)
	cfg := `{"model":{"default":"m","providers":{"m":` + stubProvider(m.URL, "m-1") +
		`,"olm":{"type":"ollama","base_url":"` + f.url + `/v1","model":"qwen3:8b","context_window":8192}}}}`
	acpModelsEnv(t, cfg)
	ws := t.TempDir()
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real
	}
	cl := newStudioClient(t, ws, true)
	cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
	var res struct {
		SessionID string `json:"sessionId"`
	}
	cl.ok("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}}, &res)
	id := res.SessionID

	var sw map[string]any
	cl.ok("session/set_config_option", map[string]any{"sessionId": id, "configId": "model", "value": "olm"}, &sw)
	st, _ := meta(sw)["ollama"].(map[string]any)
	if st == nil || st["next"] != "start" || st["ready"] != false {
		t.Fatalf("the switch reply does not say the model is not ready: %v", sw)
	}
	var rep ollama.Report
	cl.ok("_abhed/ollama/status", map[string]any{"sessionId": id}, &rep)
	if rep.Provider != "olm" || !rep.CanAct || rep.Start == nil {
		t.Fatalf("status %+v", rep)
	}
	cl.ok("_abhed/ollama/start", map[string]any{"sessionId": id}, nil)
	// Studio's automatic status asks only this machine; detail, when the
	// person opens the download, asks the registry.
	var local, full ollama.Report
	cl.ok("_abhed/ollama/status", map[string]any{"sessionId": id}, &local)
	if local.Next != "pull" || local.Pull == nil || local.Pull.SizeKnown || f.regHits.Load() != 0 {
		t.Fatalf("status %+v, registry asked %d times", local, f.regHits.Load())
	}
	cl.ok("_abhed/ollama/status", map[string]any{"sessionId": id, "detail": true}, &full)
	if full.Pull == nil || !full.Pull.SizeKnown || f.regHits.Load() != 1 {
		t.Fatalf("detail %+v, registry asked %d times", full, f.regHits.Load())
	}
	from := cl.mark()
	cl.ok("_abhed/ollama/pull", map[string]any{"sessionId": id, "pullId": "p1"}, nil)
	var sawProgress bool
	for _, msg := range cl.since(from) {
		sawProgress = sawProgress || msg.Method == "_abhed/ollama/progress"
	}
	if !sawProgress {
		t.Fatal("no progress was sent")
	}
	var got []string
	evs, _ := mustRecord(t, cl).Events(id)
	for _, ev := range evs {
		if ev.Type == agent.EvOllamaStart || ev.Type == agent.EvOllamaPull {
			var a agent.OllamaAction
			_ = json.Unmarshal(ev.Payload, &a)
			got = append(got, a.Op+":"+a.Outcome+":"+a.Surface)
		}
	}
	if strings.Join(got, ",") != "start:ok:acp,pull:ok:acp" {
		t.Fatalf("recorded %v", got)
	}
	msg := cl.refused(errParams, "_abhed/ollama/start", map[string]any{"sessionId": id, "provider": "m"})
	if !strings.Contains(msg, "not served by Ollama") {
		t.Fatalf("refusal %q", msg)
	}
}

func mustRecord(t *testing.T, cl *studioClient) interface {
	Events(string) ([]agent.Event, error)
} {
	t.Helper()
	rec, err := cl.conn.record()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// The session's own check (its start, a model switch) asks nothing off
// this machine: before a pull it asks whether to look the size up, and a
// No sends no request to the registry. A Yes then shows the size and asks.
func TestOllamaAutomaticCheckMakesNoRegistryRequest(t *testing.T) {
	f := newAppOllama(t)
	f.start(t)
	env, surface, _ := ollamaEnvFor(t, f, "")
	if env.st.ensureOllamaFor(context.Background(), surface, "olm", false) {
		t.Fatal("ready without the model")
	}
	if f.regHits.Load() != 0 {
		t.Fatalf("an automatic check asked the registry %d times", f.regHits.Load())
	}
	if len(surface.asked) != 1 || !strings.Contains(surface.asked[0].Ask, "Look up its size at") {
		t.Fatalf("asked %+v", surface.asked)
	}
	env, surface, _ = ollamaEnvFor(t, f, "", "look", ui.ChoiceYes)
	if !env.st.ensureOllamaFor(context.Background(), surface, "olm", false) || f.regHits.Load() != 1 {
		t.Fatalf("after looking it up: registry asked %d times", f.regHits.Load())
	}
}
