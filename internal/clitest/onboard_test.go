//go:build unix

package clitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Startup to the prompt, with a podman on PATH that takes three seconds to
// answer: the container probe is off the start-up path.
func TestBudgetStartupWithSlowPodman(t *testing.T) {
	var first, warm time.Duration
	for i := 0; i < 3; i++ {
		h := StartRun(t, Opts{Podman: "sleep 3; exit 1"})
		d := h.WaitOutput(PromptGlyph)
		if i == 0 {
			first = d
		} else if warm == 0 || d < warm {
			warm = d
		}
		h.Exit(0)
	}
	// Every run has a fresh HOME, so the first is the plan's cold start.
	t.Logf("startup with a slow podman: first %v, best of the rest %v", first, warm)
	AssertWithin(t, "startup, cold", first, Budgets.StartupCold)
	AssertWithin(t, "startup, warm", warm, Budgets.StartupWarm)
}

// A task typed before the prompt is drawn is not lost. The text survives;
// the Enter arrives through the cooked terminal as a line feed, which the
// line editor reads as Ctrl-J, so the task waits for another Enter. Track A
// (A1) owns the editor.
func TestKeysTypedDuringStartup(t *testing.T) {
	t.Parallel()
	Pending(t, "A1", "an Enter typed before raw mode arrives as Ctrl-J")
	h := StartRun(t, Opts{Script: `text "got it"`, Podman: "sleep 1; exit 1"})
	h.Type("early task\r")
	h.WaitOutput("got it")
	if !strings.Contains(lastRequestText(t, h), "early task") {
		t.Fatal("the early keys did not reach the model")
	}
	h.Exit(0)
}

// An endpoint that is down is named in plain words at start-up, and a task
// fails at once with the same advice, not a dial error after the retries.
func TestEndpointDownIsFriendly(t *testing.T) {
	t.Parallel()
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"local","providers":{"local":` +
		`{"type":"openai-compatible","base_url":"http://127.0.0.1:9/v1","model":"m","context_window":8192}}}}`
	h := StartRun(t, Opts{UserConfig: cfg, Cols: 120})
	h.WaitOutput("Nothing is answering at http://127.0.0.1:9/v1")
	h.WaitOutput(PromptGlyph)
	sent := time.Now()
	h.Type("hi\r")
	h.WaitOutput("error:")
	if d := time.Since(sent); d > TimeBudget(time.Second) {
		t.Errorf("the task took %v to fail", d)
	}
	out := Strip(h.Output())
	for _, bad := range []string{"dial tcp", "connect: connection refused", "Post \""} {
		if strings.Contains(out, bad) {
			t.Errorf("a raw Go error reached the screen: %q", bad)
		}
	}
	if !strings.Contains(out, "abhed doctor") {
		t.Errorf("no next step named:\n%s", out)
	}
	h.Exit(0)

	p := StartRun(t, Opts{UserConfig: cfg, Piped: true, Args: []string{"-p", "hi"}})
	if code := p.Wait(10 * time.Second); code != 1 || !strings.Contains(p.Stderr(), "Nothing is answering") {
		t.Fatalf("exit %d:\n%s", code, p.Stderr())
	}
}

// With no configuration, a first run finds Ollama, checks the model can call
// tools, asks once about auto memory (No by default) and writes only the
// person's own configuration, which the session then uses.
func TestFirstRunWithOllama(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{NoConfig: true, Env: []string{"OLLAMA_HOST={{MODEL_URL}}"}, Cols: 120,
		Script: "tool get_time {\"zone\":\"UTC\"}\n\ntext \"ready\""})
	h.WaitText("1) Ollama: stub-model")
	h.Type("\r")
	h.WaitOutput("it can.")
	h.WaitText("memory notes")
	h.Type("\r")
	h.WaitText("Write this to")
	h.Type("\r")
	h.WaitOutput("Wrote ")
	h.WaitOutput("Type a task")
	h.Type("hello\r")
	h.WaitText("ready")
	h.Exit(0)
	var cfg struct {
		Model struct {
			Default   string
			Providers map[string]map[string]any
		}
		Memory struct{ Auto bool }
	}
	data, err := os.ReadFile(filepath.Join(h.Home(), ".abhed", "config.json"))
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		t.Fatalf("%v %s", err, data)
	}
	if cfg.Model.Default != "ollama" || cfg.Model.Providers["ollama"]["model"] != "stub-model" || cfg.Memory.Auto {
		t.Fatalf("config %s", data)
	}
	if st, _ := os.Stat(filepath.Join(h.Home(), ".abhed", "config.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if _, err := os.Stat(filepath.Join(h.Workspace(), ".abhed")); err == nil {
		t.Fatal("the first run wrote into the workspace")
	}
}

// An endpoint by URL takes the NAME of the variable with its key: a pasted
// key is refused and never written.
func TestFirstRunEndpointNeverStoresAKey(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{NoConfig: true, Cols: 120, Env: []string{"MY_KEY=abhed-canary-secret"},
		Script: "tool get_time {\"zone\":\"UTC\"}"})
	h.WaitText("Choose [e]")
	h.Type("e\r")
	h.WaitText("Base URL")
	h.Type(h.Stub().URL() + "\r")
	h.WaitText("Name of the environment variable")
	h.Type("sk-live-0123456789abcdef\r")
	h.WaitOutput("That is not a variable name")
	h.Type("MY_KEY\r")
	h.WaitText("Model name [stub-model]")
	h.Type("\r")
	h.WaitOutput("it can.")
	h.WaitText("memory notes")
	h.Type("y\r")
	h.WaitText("Write this to")
	h.Type("\r")
	h.WaitOutput("Type a task")
	h.Exit(0)
	data, _ := os.ReadFile(filepath.Join(h.Home(), ".abhed", "config.json"))
	s := string(data)
	if !strings.Contains(s, `"api_key_env": "MY_KEY"`) || strings.Contains(s, "sk-live") || strings.Contains(s, CanaryPrefix) || !strings.Contains(s, `"auto": true`) {
		t.Fatalf("config:\n%s", s)
	}
	reqs := h.Requests()
	if len(reqs) == 0 {
		t.Fatal("no probe reached the endpoint")
	}
}
