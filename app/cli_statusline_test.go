package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func TestSanitizeStatus(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                "plain text",
		"\x1b[1;32mgreen\x1b[0m":    "\x1b[1;32mgreen\x1b[0m\x1b[0m",
		"\x1b]0;pwned\x07after":     "after",
		"\x1b]52;c;ZXZpbA==\x1b\\x": "x",
		"a\x1b[2Jb\x1b[10;1Hc":      "abc",
		"bell\x07 and\rcr":          "bell andcr",
		"tab\there":                 "tab here",
		"\x1b7save":                 "save",
		strings.Repeat("x", 300):    strings.Repeat("x", 200),
		"unicode ▲ ok":              "unicode ▲ ok",
	} {
		if got := sanitizeStatus(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

func testSandbox(t *testing.T, ws string) sandbox.Sandbox {
	t.Helper()
	cfg := config.Default()
	cfg.Sandbox.MinTier = "none"
	sb, err := statuslineSandbox(cfg, ws)
	if err != nil {
		t.Skipf("no process sandbox here: %v", err)
	}
	return sb
}

func TestRunStatusline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	sb := testSandbox(t, ws)
	m := ui.StatusModel{Provider: "stub", Mode: "plan"}
	got, err := runStatusline(context.Background(), sb, ws, `grep -o '"mode":"[a-z]*"'; printf '\033]0;title\007'`, m)
	if err != nil || got != `"mode":"plan"` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := runStatusline(context.Background(), sb, ws, "sleep 2", m); err == nil || !strings.Contains(err.Error(), "300 ms") {
		t.Fatalf("slow command: %v", err)
	}
	if got, err := runStatusline(context.Background(), sb, ws, "", m); got != "" || err != nil {
		t.Fatalf("no command: %q %v", got, err)
	}
}

// fakeProcess records the policy the statusline asked for.
type fakeProcess struct {
	sandbox.Sandbox
	p  sandbox.Policy
	ok bool
}

func (f *fakeProcess) Available() (bool, string) { return f.ok, "not here" }

// The statusline runs under the process tier with the network off, whatever
// the session allows, and not at all where that tier is missing.
func TestStatuslineSandboxIsProcessWithNoNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	var got *fakeProcess
	old := processSandbox
	t.Cleanup(func() { processSandbox = old })
	processSandbox = func(p sandbox.Policy) sandbox.Sandbox { got = &fakeProcess{p: p, ok: true}; return got }
	cfg := config.Default()
	cfg.Sandbox.AllowNetwork = true
	cfg.Sandbox.MinTier = "none"
	if _, err := statuslineSandbox(cfg, ws); err != nil {
		t.Fatal(err)
	}
	if got.p.AllowNetwork || got.p.MinTier != sandbox.TierProcess {
		t.Fatalf("policy %+v", got.p)
	}
	processSandbox = func(p sandbox.Policy) sandbox.Sandbox { return &fakeProcess{p: p} }
	if sb, err := statuslineSandbox(cfg, ws); err == nil || sb != nil {
		t.Fatal("ran with no process sandbox")
	}
	// Refused once, said once, and nothing is shown.
	st := &cliState{appCfg: cfg, workspace: ws}
	st.appCfg.Statusline.Command = "echo hi"
	first := st.statusLine(context.Background(), "default")
	if !strings.Contains(first, "process sandbox") || st.statusLine(context.Background(), "default") != "" {
		t.Fatalf("%q", first)
	}
}

// With the session's network on, the statusline still reaches nothing.
func TestStatuslineHasNoNetwork(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit <- struct{}{} }))
	defer srv.Close()
	cfg := config.Default()
	cfg.Sandbox.MinTier = "none"
	cfg.Sandbox.AllowNetwork = true
	sb, err := statuslineSandbox(cfg, ws)
	if err != nil {
		t.Skipf("no process sandbox here: %v", err)
	}
	_, _ = runStatusline(context.Background(), sb, ws, "curl -s -m 0.2 "+srv.URL+" >/dev/null; echo done", ui.StatusModel{})
	select {
	case <-hit:
		t.Fatal("the statusline reached the network")
	case <-time.After(100 * time.Millisecond):
	}
}
