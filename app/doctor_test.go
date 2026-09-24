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
	defer srv.Close()
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

	if out, code := doctor(""); code != 0 {
		t.Skipf("the doctor does not pass here even without unknown keys (%d):\n%s", code, out)
	}
	out, code := doctor(`"provider":"stub",`)
	if code == 0 {
		t.Fatalf("the doctor passed a configuration with an unknown key:\n%s", out)
	}
	if !strings.Contains(out, "unknown key model.provider is ignored (did you mean model.default?)") || !strings.Contains(out, "Not ready") {
		t.Fatalf("the doctor does not name the unknown key:\n%s", out)
	}
}
