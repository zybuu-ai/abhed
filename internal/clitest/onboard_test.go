//go:build unix

package clitest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Startup to the prompt, with a podman on PATH that takes three seconds to
// answer: the container probe is off the start-up path.
func TestBudgetStartupWithSlowPodman(t *testing.T) {
	first, warm := startupRuns(t, 5, func() Opts { return Opts{Podman: "sleep 3; exit 1"} })
	// Every run has a fresh HOME, so the first is the plan's cold start.
	t.Logf("startup with a slow podman: first %v, best of the rest %v", first, warm)
	assertStartup(t, "startup, cold", first, Budgets.StartupColdCPU, Budgets.StartupCold)
	assertStartup(t, "startup, warm", warm, Budgets.StartupWarmCPU, Budgets.StartupWarm)
}

// startupCost is how long a run took to its prompt: by the wall clock, and in
// the binary's own CPU time at that moment.
type startupCost struct{ wall, cpu time.Duration }

func (c startupCost) String() string {
	return fmt.Sprintf("%v wall, %v CPU", c.wall.Round(time.Millisecond), c.cpu.Round(time.Millisecond))
}

// startupRuns starts n runs to the prompt and returns the first, and the
// best of the rest by CPU and by wall clock, each taken on its own.
func startupRuns(t *testing.T, n int, opts func() Opts) (first, warm startupCost) {
	t.Helper()
	for i := 0; i < n; i++ {
		h := StartRun(t, opts())
		d := h.WaitOutput(PromptGlyph)
		cpu, err := h.CPUTime()
		if err != nil {
			t.Fatalf("CPU time to the prompt: %v", err)
		}
		h.Exit(0)
		c := startupCost{wall: d, cpu: cpu}
		switch {
		case i == 0:
			first = c
		case warm.wall == 0:
			warm = c
		default:
			warm.wall, warm.cpu = min(warm.wall, c.wall), min(warm.cpu, c.cpu)
		}
	}
	return first, warm
}

// assertStartup holds a start to its CPU budget, and its wall clock to the
// loose ceiling, which a start that waits on the slow engine (three seconds)
// breaks. The tight wall-clock budget is only reported: the load average
// lags, so it cannot say when the machine, such as a whole-module test run
// beside this one, is what made a start slow.
func assertStartup(t *testing.T, what string, got startupCost, cpu, wall time.Duration) {
	t.Helper()
	AssertWithin(t, what+" (CPU)", got.cpu, cpu)
	AssertWithin(t, what+" (wall, ceiling)", got.wall, Budgets.StartupWallCeiling)
	if got.wall > raceSlack(wall) {
		load, _ := Loaded()
		t.Logf("%s: %v by the wall clock, over the %v an unloaded machine takes (load average %.1f, %d CPUs)",
			what, got.wall.Round(time.Millisecond), raceSlack(wall), load, runtime.NumCPU())
	}
}

// brokenBwrap puts a bubblewrap that cannot make namespaces, as Ubuntu's
// AppArmor default leaves it for an unprivileged user, and a docker whose
// script is docker, first on PATH.
func brokenBwrap(o Opts, docker string) Opts {
	o.Setup = func(home, ws string) {
		bin := filepath.Join(home, "bin")
		stubs := map[string]string{
			"bwrap":  "echo 'bwrap: setting up uid map: Permission denied' >&2; exit 1",
			"docker": docker,
		}
		for name, body := range stubs {
			if err := os.MkdirAll(bin, 0o700); err != nil {
				panic(err)
			}
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
				panic(err)
			}
		}
	}
	o.Env = append(o.Env, "PATH={{HOME}}/bin:{{HOME}}/../bin:/usr/bin:/bin:/usr/sbin:/sbin")
	return o
}

// Where bwrap is installed but cannot run, a session that requires no tier
// still reaches the prompt without waiting for a container engine.
func TestBudgetStartupWithBrokenBwrap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process tier is bubblewrap only on Linux")
	}
	_, warm := startupRuns(t, 4, func() Opts { return brokenBwrap(Opts{Podman: "sleep 3; exit 1"}, "sleep 3; exit 1") })
	t.Logf("startup with a broken bwrap and a slow engine: best of 3 %v", warm)
	assertStartup(t, "startup, broken bwrap", warm, Budgets.StartupWarmCPU, Budgets.StartupWarm)
}

// Requiring the process tier where bwrap cannot run still refuses to start,
// naming bwrap's reason, before any command could run.
func TestBrokenBwrapRefusesARequiredTier(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process tier is bubblewrap only on Linux")
	}
	t.Parallel()
	cfg := strings.Replace(DefaultUserConfig, `"min_tier":"none"`, `"min_tier":"process"`, 1)
	h := StartRun(t, brokenBwrap(Opts{UserConfig: cfg, Piped: true, Args: []string{"-p", "hi"}}, "exit 1"))
	code := h.Wait(15 * time.Second)
	if code == 0 || !strings.Contains(h.Stderr(), "Permission denied") {
		t.Fatalf("exit %d:\n%s", code, h.Stderr())
	}
	h2 := StartRun(t, brokenBwrap(Opts{UserConfig: cfg}, "exit 1"))
	h2.WaitOutput("no sandbox backend meets")
}

// A task typed before the prompt is drawn is not lost, and its Enter, which
// the cooked terminal turned into a line feed, still submits it.
func TestKeysTypedDuringStartup(t *testing.T) {
	t.Parallel()
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
// tools, asks once about auto memory (1 No, 2 yes) and writes only the
// person's own configuration, which the session then uses.
func TestFirstRunWithOllama(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{NoConfig: true, Env: []string{"OLLAMA_HOST={{MODEL_URL}}"}, Cols: 120,
		Script: "tool get_time {\"zone\":\"UTC\"}\n\ntext \"ready\""})
	h.WaitText("1) Ollama: stub-model")
	h.Type("\r")
	h.WaitOutput("it can.")
	h.WaitText("memory notes")
	h.Type("1\r")
	h.WaitText("Write this to")
	h.Type("2\r")
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
	h.WaitOutput("looks like a key")
	h.Type("hf_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789\r")
	h.WaitScreen(func(Screen) bool { return strings.Count(Strip(h.Output()), "looks like a key") == 2 }, DefaultTimeout)
	h.Type("MY_KEY\r")
	h.WaitText("Model name [stub-model]")
	h.Type("\r")
	h.WaitOutput("it can.")
	h.WaitText("memory notes")
	h.Type("2\r")
	h.WaitText("Write this to")
	h.Type("2\r")
	h.WaitOutput("Type a task")
	h.Exit(0)
	data, _ := os.ReadFile(filepath.Join(h.Home(), ".abhed", "config.json"))
	s := string(data)
	if !strings.Contains(s, `"api_key_env": "MY_KEY"`) || strings.Contains(s, "sk-live") || strings.Contains(s, "hf_") || strings.Contains(s, CanaryPrefix) || !strings.Contains(s, `"auto": true`) {
		t.Fatalf("config:\n%s", s)
	}
	reqs := h.Requests()
	if len(reqs) == 0 {
		t.Fatal("no probe reached the endpoint")
	}
}
