package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// stdoutOf runs fn and returns what it wrote to stdout.
func stdoutOf(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := fn()
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out), code
}

// A key nothing reads fails the doctor, which names it and what was meant;
// the same configuration without it passes.
func TestDoctorFailsOnUnknownKeys(t *testing.T) {
	srv := doctorEndpoint(t)
	t.Setenv("HOME", t.TempDir())

	doctor := func(extra string) (string, int) {
		ws := t.TempDir()
		if r, err := filepath.EvalSymlinks(ws); err == nil {
			ws = r
		}
		cfg := `{"model":{"default":"stub",` + extra + `"providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
		_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755)
		if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		return stdoutOf(t, func() int { return newApp().doctor(ws) })
	}

	// The key is listed before any check that needs the environment.
	out, code := doctor(`"provider":"stub",`)
	if code == 0 || !strings.Contains(out, "unknown key model.provider is ignored (did you mean model.default?)") {
		t.Fatalf("the doctor did not fail on and name the unknown key (%d):\n%s", code, out)
	}
	// Where the rest passes here, the key is what made it fail.
	if clean, code := doctor(""); code == 0 {
		if !strings.Contains(out, "Not ready") || strings.Contains(clean, "Not ready") {
			t.Fatalf("the unknown key is not what failed the doctor:\n%s\n---\n%s", out, clean)
		}
	} else {
		t.Logf("the doctor fails here for another reason too (%d); the verdict is checked on its own below", code)
	}
}

// The verdict, apart from the checks that need an endpoint and a sandbox.
func TestDoctorVerdictOnUnknownKeys(t *testing.T) {
	var b strings.Builder
	cfg := config.Default()
	if printUnknown(&b, cfg) || b.Len() != 0 {
		t.Fatalf("a configuration with no unknown keys printed: %q", b.String())
	}
	if code := doctorVerdict(&b, false); code != 0 || !strings.Contains(b.String(), "Ready.") {
		t.Fatalf("clean verdict %d: %q", code, b.String())
	}
	b.Reset()
	cfg.Unknown = []config.UnknownKey{{File: "/w/.abhed/config.json", Path: "model.provider", Suggest: "model.default"},
		{File: "/w/.abhed/config.json", Path: "zzz"}}
	if !printUnknown(&b, cfg) || !strings.Contains(b.String(), "config      /w/.abhed/config.json: unknown key model.provider") ||
		!strings.Contains(b.String(), "            /w/.abhed/config.json: unknown key zzz") {
		t.Fatalf("unknown keys not listed: %q", b.String())
	}
	if code := doctorVerdict(&b, true); code != 1 || !strings.Contains(b.String(), "Not ready") {
		t.Fatalf("verdict with unknown keys %d: %q", code, b.String())
	}
}

// doctorEndpoint answers the doctor's two checks as a working model would.
func doctorEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range []string{
			`{"choices":[{"delta":{"content":"ok"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A state file with a second name fails the doctor, which names the file and
// how to fix it; the same workspace without the link is ready.
func TestDoctorFailsOnAStateFileWithASecondName(t *testing.T) {
	srv := doctorEndpoint(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755)
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(ws, ".abhed", "users.json")
	if err := os.WriteFile(users, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := stdoutOf(t, func() int { return newApp().doctor(ws) }); code != 0 {
		t.Fatalf("the doctor failed without the link (%d):\n%s", code, out)
	}
	if err := os.Link(users, filepath.Join(ws, "notes.json")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	out, code := stdoutOf(t, func() int { return newApp().doctor(ws) })
	if code != 1 || !strings.Contains(out, users+" has 2 names") || !strings.Contains(out, "-samefile") ||
		!strings.Contains(out, "checking sandbox exec... FAILED") || strings.Contains(out, "Ready.") {
		t.Fatalf("the doctor did not fail on and name the linked state file (%d):\n%s", code, out)
	}
}

// The doctor names a resource limit the tier in force does not apply, and not
// the default memory limit nobody set.
func TestLimitWarningsNameWhatATierIgnores(t *testing.T) {
	root := false
	old := runningAsRoot
	runningAsRoot = func() bool { return root }
	t.Cleanup(func() { runningAsRoot = old })
	// Set in a file to the default value: still set by someone.
	set := config.Default()
	set.SetKeys = []string{"sandbox.max_memory_mb"}
	for _, c := range []struct {
		cfg  config.Config
		tier sandbox.Tier
		root bool
		want []string
	}{
		{config.Default(), sandbox.TierProcess, false, nil},
		{config.Default(), sandbox.TierNone, false, []string{"max_procs"}},
		{config.Default(), sandbox.TierProcess, true, []string{"runs as root"}},
		{set, sandbox.TierProcess, false, []string{"max_memory_mb"}},
		{set, sandbox.TierNone, false, []string{"max_memory_mb", "max_procs"}},
		{set, sandbox.TierContainer, true, nil},
		{set, sandbox.TierVM, false, nil},
	} {
		root = c.root
		got := strings.Join(limitWarnings(c.cfg, c.tier), "\n")
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s (root %v, memory %d): no warning for %s: %q", c.tier, c.root, c.cfg.Sandbox.MaxMemoryMB, w, got)
			}
		}
		if len(c.want) == 0 && got != "" {
			t.Errorf("%s (root %v, memory %d): warned %q", c.tier, c.root, c.cfg.Sandbox.MaxMemoryMB, got)
		}
	}
}

// A sandbox that cannot be built fails the doctor at its last check, rather
// than being skipped on the way to "Ready.".
func TestDoctorFailsWhenTheSandboxCannotBeBuilt(t *testing.T) {
	srv := doctorEndpoint(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	// A users file in the workspace, where commands could move it, is refused.
	cfg := `{"auth":{"users_file":"` + filepath.Join(ws, "users.json") + `"},"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755)
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := stdoutOf(t, func() int { return newApp().doctor(ws) })
	if code != 1 || !strings.Contains(out, "checking sandbox exec... FAILED") || strings.Contains(out, "SKIPPED") || strings.Contains(out, "Ready.") {
		t.Fatalf("the doctor did not fail on the sandbox (%d):\n%s", code, out)
	}
}
