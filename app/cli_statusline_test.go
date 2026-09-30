package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
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
	sb, _, _, err := statuslineSandbox(cfg, ws, nil)
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
	if _, _, _, err := statuslineSandbox(cfg, ws, nil); err != nil {
		t.Fatal(err)
	}
	if got.p.AllowNetwork || got.p.MinTier != sandbox.TierProcess {
		t.Fatalf("policy %+v", got.p)
	}
	processSandbox = func(p sandbox.Policy) sandbox.Sandbox { return &fakeProcess{p: p} }
	if sb, _, _, err := statuslineSandbox(cfg, ws, nil); err == nil || sb != nil {
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
	sb, _, _, err := statuslineSandbox(cfg, ws, nil)
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

// homeOutsideTemp is a HOME for one test outside every temp and cache
// area, which a statusline script may not live in: a folder beside the test.
func homeOutsideTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".statusline-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	// A checkout in a temp area has no place a script is allowed.
	if under(abs, sandbox.WritableAreas()) {
		t.Skipf("the checkout is in a writable area (%s), where a statusline script is refused", abs)
	}
	t.Setenv("HOME", abs)
	return abs
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { // #nosec G306 -- the test's script
		t.Fatal(err)
	}
}

// A script in the home directory runs as the statusline, pinned: swapped
// afterwards for a link to a hidden script or to the configuration, it is
// not run, and nothing of either is shown.
func TestStatuslineScriptPinned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := homeOutsideTemp(t)
	state := filepath.Join(home, ".abhed")
	writeScript(t, filepath.Join(home, "bin", "status.sh"), "#!/bin/sh\ncat \"$HOME/.abhed/secrets.json\" 2>/dev/null; echo from-home\n")
	writeScript(t, filepath.Join(state, "hidden.sh"), "#!/bin/sh\necho hidden-ran\n")
	if err := os.WriteFile(filepath.Join(state, "secrets.json"), []byte(`{"K":"do-not-read"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.MinTier = "none"
	cfg.Statusline.Command = "~/bin/status.sh"
	sb, _, _, err := statuslineSandbox(cfg, ws, nil)
	// Only a missing sandbox skips; a refusal of the script is a failure.
	if errors.Is(err, errNoProcessSandbox) {
		t.Skipf("no process sandbox here: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if probe, err := runStatusline(context.Background(), sb, ws, "echo probe", ui.StatusModel{}); err != nil || probe != "probe" {
		t.Skipf("the process sandbox cannot run a command here: %v", err)
	}
	// The first run of a new script pays for the system's checks of it,
	// and a loaded machine is slow; the limit is not what this tests.
	old := statuslineTimeout
	statuslineTimeout = 10 * time.Second
	t.Cleanup(func() { statuslineTimeout = old })
	st := &cliState{appCfg: cfg, workspace: ws}
	if got := st.statusLine(context.Background(), "default"); got != "from-home" {
		t.Fatalf("%q", got)
	}
	for _, target := range []string{filepath.Join(state, "hidden.sh"), filepath.Join(state, "secrets.json")} {
		link := filepath.Join(home, "bin", "status.sh")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		// Said on every redraw, with what to do about it.
		for range 2 {
			got := st.statusLine(context.Background(), "default")
			if strings.Contains(got, "hidden-ran") || strings.Contains(got, "do-not-read") || strings.Contains(got, "from-home") ||
				!strings.Contains(got, "start a new session") {
				t.Fatalf("after a swap to %s: %q", target, got)
			}
		}
	}
}

// A script in a folder the agent's tools may write, added by -add-dir or
// granted to the session as a skill's folder, is refused, and the refusal
// is said on every redraw.
func TestStatuslineScriptRefusedInGrantedFolders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := homeOutsideTemp(t)
	ws := t.TempDir()
	added := filepath.Join(home, "bin2")
	writeScript(t, filepath.Join(added, "s.sh"), "#!/bin/sh\necho ran\n")
	cfg := config.Default()
	cfg.Sandbox.MinTier = "none"
	cfg.AdditionalDirs = []string{added}
	cfg.Statusline.Command = filepath.Join(added, "s.sh")
	if _, _, _, err := statuslineSandbox(cfg, ws, nil); err == nil || !strings.Contains(err.Error(), "granted to its tools") {
		t.Fatalf("an -add-dir script was taken: %v", err)
	}
	skill := filepath.Join(home, "skills-elsewhere", "tidy")
	writeScript(t, filepath.Join(skill, "s.sh"), "#!/bin/sh\necho ran\n")
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddRoot(skill); err != nil {
		t.Fatal(err)
	}
	cfg.AdditionalDirs = nil
	cfg.Statusline.Command = filepath.Join(skill, "s.sh")
	st := &cliState{appCfg: cfg, workspace: ws, sess: sess}
	for range 3 {
		if got := st.statusLine(context.Background(), "default"); !strings.Contains(got, "granted to its tools") {
			t.Fatalf("%q", got)
		}
	}
}

// A script is refused where the agent could change it or where Abhed keeps
// its state, by where it is named and where it resolves.
func TestStatuslineScriptRefusedInStateAndWritableAreas(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := homeOutsideTemp(t)
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	users := filepath.Join(home, "etc-abhed")
	for _, p := range []string{
		filepath.Join(home, ".abhed", "s.sh"), filepath.Join(home, ".abhed", "skills", "x", "s.sh"),
		filepath.Join(ws, ".abhed", "s.sh"), filepath.Join(users, "s.sh"), filepath.Join(ws, "s.sh"),
		filepath.Join(home, ".cache", "s.sh"),
	} {
		writeScript(t, p, "#!/bin/sh\n")
		if pin, _, err := statuslineScript(p+" --flag", ws, []string{users}, nil); err == nil || pin.Info != nil {
			t.Errorf("%s was taken", p)
		}
	}
	tmp, err := os.CreateTemp("", "statusline-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	t.Cleanup(func() { _ = os.Remove(tmp.Name()) })
	if err := os.Chmod(tmp.Name(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := statuslineScript(tmp.Name(), ws, nil, nil); err == nil {
		t.Errorf("a temp script %s was taken", tmp.Name())
	}
	// A link from a fine place into the state resolves there, and is refused.
	writeScript(t, filepath.Join(home, ".abhed", "inner.sh"), "#!/bin/sh\n")
	link := filepath.Join(home, "bin", "link.sh")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".abhed", "inner.sh"), link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := statuslineScript(link, ws, nil, nil); err == nil {
		t.Error("a link into the state was taken")
	}
	ok := filepath.Join(home, "bin", "ok.sh")
	writeScript(t, ok, "#!/bin/sh\n")
	pin, rest, err := statuslineScript("~/bin/ok.sh --flag x", ws, []string{users}, nil)
	if err != nil || pin.Path != ok || rest != " --flag x" {
		t.Fatalf("%+v %q %v", pin, rest, err)
	}
	for _, c := range []string{"echo hi", "status.sh", "~/bin/none.sh"} {
		if pin, _, err := statuslineScript(c, ws, nil, nil); err != nil || pin.Info != nil {
			t.Errorf("%q: %+v %v", c, pin, err)
		}
	}
}
