package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
)

// trustWorkspace makes a home whose own config names a stub model, and a
// workspace whose config would switch to a model that does not exist: an
// untrusted run works, and a trusted one fails naming it.
func trustWorkspace(t *testing.T, file string) (home, ws string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		srv.URL + `","model":"m","context_window":8192}}}}`
	for dir, body := range map[string]string{home: user, ws: file} {
		if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv(config.TrustEnv, "")
	return home, ws
}

const widening = `{"model":{"default":"nope"},"permissions":{"mode":"bypass","allow":["bash(*)"],"deny":["bash(curl*)"]}}`

// TestTrustMainHelper runs Main with the arguments the test below gives it.
func TestTrustMainHelper(t *testing.T) {
	raw := os.Getenv("ABHED_TRUST_MAIN_ARGS")
	if raw == "" {
		t.Skip("run by the workspace-trust tests")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	os.Exit(Main(args))
}

func mainHelper(args []string, env ...string) *exec.Cmd {
	raw, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestTrustMainHelper$")
	cmd.Env = append(os.Environ(), append(env, "ABHED_TRUST_MAIN_ARGS="+string(raw))...)
	return cmd
}

// A headless run never asks: it reads nothing from stdin, warns naming what
// it ignored, and records no decision. The flag and the variable trust it.
func TestHeadlessRunNeverPromptsAndTheFlagTrusts(t *testing.T) {
	_, ws := trustWorkspace(t, widening)
	for _, c := range []struct {
		name  string
		args  []string
		env   []string
		code  int
		wants []string
	}{
		{"untrusted", []string{"-C", ws, "-p", "hi"}, nil, 0,
			[]string{"is not trusted; ignored model.default, permissions.allow, permissions.mode", "abhed trust"}},
		{"flag", []string{"-C", ws, "-trust-workspace", "-p", "hi"}, nil, 1, []string{`model "nope" is not defined`}},
		{"environment", []string{"-C", ws, "-p", "hi"}, []string{config.TrustEnv + "=1"}, 1, []string{`model "nope" is not defined`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := mainHelper(c.args, c.env...)
			cmd.Stdin = strings.NewReader("t\n")
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			err := cmd.Run()
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			}
			if code != c.code {
				t.Fatalf("exit %d, want %d:\n%s", code, c.code, out.String())
			}
			for _, w := range c.wants {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			if strings.Contains(out.String(), "Trust this file?") {
				t.Fatalf("a headless run prompted:\n%s", out.String())
			}
			if recs, _ := config.TrustRecords(); len(recs) != 0 {
				t.Fatalf("a headless run recorded a decision: %v", recs)
			}
		})
	}
}

// The prompt lists what the file would set, shows the file on request, and
// trusts only on an explicit answer.
func TestAskTrustAnswers(t *testing.T) {
	_, ws := trustWorkspace(t, widening)
	cfg, err := config.LoadWith(ws, config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in    string
		grant bool
		err   bool
		shows []string
	}{
		{"t\n", true, false, []string{"permissions.mode", `"bypass"`, "permissions.allow", "model.default", "Applied already", "permissions.deny"}},
		{"d\n", false, false, []string{"Not trusted"}},
		{"v\nt\n", true, false, []string{`"nope"`, "--- " + cfg.Workspace.File}},
		{"what\n\n", false, true, []string{"Trust this file?"}},
		{"", false, true, nil},
	} {
		var out bytes.Buffer
		grant, err := askTrust(strings.NewReader(c.in), &out, cfg.Workspace)
		if grant != c.grant || (err != nil) != c.err {
			t.Errorf("%q: grant %v err %v", c.in, grant, err)
		}
		for _, s := range c.shows {
			if !strings.Contains(out.String(), s) {
				t.Errorf("%q: output lacks %q:\n%s", c.in, s, out.String())
			}
		}
	}
}

// doctor names each setting an untrusted file had ignored, and says so no
// longer once it is trusted.
func TestDoctorNamesIgnoredSettings(t *testing.T) {
	_, ws := trustWorkspace(t, `{"permissions":{"mode":"bypass","allow":["bash(*)"],"deny":["bash(curl*)"]}}`)
	out, _ := stdoutOf(t, func() int { return newApp().doctor(ws) })
	for _, w := range []string{"trust       NOT TRUSTED — " + filepath.Join(ws, ".abhed", "config.json"),
		`⚠ ignored permissions.mode "bypass"`, `⚠ ignored permissions.allow ["bash(*)"]`, "abhed trust grant"} {
		if !strings.Contains(out, w) {
			t.Errorf("doctor lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "mode        bypass") {
		t.Fatalf("doctor reports the untrusted mode as in force:\n%s", out)
	}
	st, _ := config.InspectWorkspace(ws)
	if err := config.GrantTrust(ws, st.SHA256); err != nil {
		t.Fatal(err)
	}
	out, _ = stdoutOf(t, func() int { return newApp().doctor(ws) })
	if !strings.Contains(out, "trust       trusted — ") || strings.Contains(out, "ignored") || !strings.Contains(out, "mode        bypass") {
		t.Fatalf("doctor after a grant:\n%s", out)
	}
}

// abhed trust shows, grants, lists and revokes.
func TestTrustCommand(t *testing.T) {
	_, ws := trustWorkspace(t, widening)
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if code := trustCmd(ws, args, &out); code != 0 {
			t.Fatalf("trust %v exited %d:\n%s", args, code, out.String())
		}
		return out.String()
	}
	if out := run(); !strings.Contains(out, "trust       NOT TRUSTED") || !strings.Contains(out, "permissions.mode") {
		t.Fatalf("show:\n%s", out)
	}
	st, _ := config.InspectWorkspace(ws)
	if code := trustCmd(ws, []string{"grant", "-sha256", strings.Repeat("0", 64)}, io.Discard); code != 1 {
		t.Fatalf("a grant for content not on disk exited %d", code)
	}
	if st2, _ := config.InspectWorkspace(ws); st2.Trusted {
		t.Fatal("a grant for other content trusted the file")
	}
	if out := run("grant", "-sha256", st.SHA256); !strings.Contains(out, "Trusted ") {
		t.Fatalf("grant:\n%s", out)
	}
	if out := run("show", ws); !strings.Contains(out, "trust       trusted") {
		t.Fatalf("show after grant:\n%s", out)
	}
	if out := run("list"); !strings.Contains(out, "trusted") || !strings.Contains(out, ws) {
		t.Fatalf("list:\n%s", out)
	}
	if out := run("revoke"); !strings.Contains(out, "untrusted again") {
		t.Fatalf("revoke:\n%s", out)
	}
	if st, _ := config.InspectWorkspace(ws); st.Trusted {
		t.Fatal("still trusted after a revoke")
	}
	if code := trustCmd(ws, []string{"bless"}, io.Discard); code != 2 {
		t.Fatalf("an unknown action exits %d", code)
	}
}

// abhed init trusts the file it wrote.
func TestInitTrustsWhatItWrote(t *testing.T) {
	_, ws := trustWorkspace(t, `{}`)
	if err := os.Remove(filepath.Join(ws, ".abhed", "config.json")); err != nil {
		t.Fatal(err)
	}
	if _, code := stdoutOf(t, func() int { return Main([]string{"-C", ws, "init"}) }); code != 0 {
		t.Fatalf("init exited %d", code)
	}
	if st, _ := config.InspectWorkspace(ws); !st.Trusted || st.Reason != "stored" {
		t.Fatalf("the starter file is not trusted: %+v", st)
	}
}

// abhed rpc says on ready whether the workspace file applied.
func TestRPCReadyReportsWorkspaceTrust(t *testing.T) {
	_, ws := trustWorkspace(t, widening)
	out := runRPC(t, ws, `{"id":"1","method":"start"}`)
	var ready rpcResponse
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"type":"ready"`) {
			_ = json.Unmarshal([]byte(line), &ready)
		}
	}
	st := ready.WorkspaceTrust
	if st == nil || st.Trusted || st.Reason != "new" || len(st.Ignored) == 0 {
		t.Fatalf("ready does not report an untrusted file: %s", out)
	}
}

// ACP reports the workspace file's trust on session/new, lets the editor
// refuse it, and never lets the wire grant it.
func TestACPReportsWorkspaceTrust(t *testing.T) {
	_, ws := trustWorkspace(t, `{"permissions":{"mode":"accept-edits","allow":["bash(*)"],"deny":["bash(curl*)"]}}`)
	cl := newACPClient(t, nil)
	trustOf := func(m rpcMessage) config.WorkspaceTrust {
		t.Helper()
		if m.Error != nil {
			t.Fatalf("session/new: %s", m.Error.Message)
		}
		var res struct {
			Meta struct {
				Abhed struct {
					WorkspaceTrust config.WorkspaceTrust `json:"workspaceTrust"`
				} `json:"zybuu.ai/abhed"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(m.Result, &res)
		return res.Meta.Abhed.WorkspaceTrust
	}
	st := trustOf(cl.request(1, "session/new", map[string]any{"cwd": ws, "mcpServers": []any{}}))
	if st.Trusted || st.File == "" || len(st.Ignored) == 0 || len(st.Applied) == 0 {
		t.Fatalf("untrusted report: %+v", st)
	}
	if m := cl.request(2, "session/new", map[string]any{"cwd": ws, "_meta": map[string]any{"abhed": map[string]any{"trust": "trusted"}}}); m.Error == nil {
		t.Fatal("the wire granted trust")
	}
	if err := config.GrantTrust(ws, st.SHA256); err != nil {
		t.Fatal(err)
	}
	if st = trustOf(cl.request(3, "session/new", map[string]any{"cwd": ws})); !st.Trusted || st.Reason != "stored" {
		t.Fatalf("after a grant: %+v", st)
	}
	if st = trustOf(cl.request(4, "session/new", map[string]any{"cwd": ws, "_meta": map[string]any{"abhed": map[string]any{"trust": "untrusted"}}})); st.Trusted || st.Reason != "refused" {
		t.Fatalf("the editor's refusal: %+v", st)
	}
}

// serve, user and migrate refuse to run without the auth, storage or server
// settings an untrusted file made: without them a console would be open.
func TestDeploymentCommandsRefuseAnUntrustedDeployment(t *testing.T) {
	_, ws := trustWorkspace(t, `{"auth":{"mode":"local","require_group":"eng"},"storage":{"driver":"postgres","dsn":"postgres://app:pw@127.0.0.1:1/abhed"}}`)
	for _, args := range [][]string{
		{"-C", ws, "serve", "-addr", "127.0.0.1:0"},
		{"-C", ws, "user", "list"},
		{"-C", ws, "migrate"},
	} {
		cmd := mainHelper(args)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(out.String(), "abhed trust grant") || !strings.Contains(out.String(), "auth.mode") {
				t.Errorf("%v: %v\n%s", args[2:], err, out.String())
			}
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill() // the helper this test started
			t.Fatalf("%v started instead of refusing:\n%s", args[2:], out.String())
		}
	}
	// Trusted, a file with auth is taken.
	t.Setenv(config.TrustEnv, "1")
	cfgFile := `{"auth":{"mode":"local"}}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfgFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runUser(t, ws, "list"); code != 0 {
		t.Fatalf("user refused a trusted file: %d %s", code, out)
	}
}

// -trust-workspace is taken after the subcommand too.
func TestTrustFlagAfterTheSubcommand(t *testing.T) {
	_, ws := trustWorkspace(t, `{"permissions":{"mode":"accept-edits"}}`)
	out, _ := stdoutOf(t, func() int { return Main([]string{"-C", ws, "doctor", "-trust-workspace"}) })
	if !strings.Contains(out, "trust       trusted for this run (-trust-workspace)") || !strings.Contains(out, "mode        accept-edits") {
		t.Fatalf("the trailing flag was not taken:\n%s", out)
	}
}

// The flag is taken only where it is a flag: first after the subcommand, or
// among serve's, eval's and resolve's own flags; never as a value or after --.
func TestLeadingTrustFlag(t *testing.T) {
	for _, c := range []struct {
		args  []string
		trust bool
		left  []string
	}{
		{[]string{"doctor", "-trust-workspace"}, true, []string{"doctor"}},
		{[]string{"acp", "--trust-workspace"}, true, []string{"acp"}},
		{[]string{"user", "add", "-trust-workspace"}, false, []string{"user", "add", "-trust-workspace"}},
		{[]string{"rpc", "--", "-trust-workspace"}, false, []string{"rpc", "--", "-trust-workspace"}},
		{[]string{"secret", "-trust-workspace"}, false, []string{"secret", "-trust-workspace"}},
		{[]string{"trust", "-trust-workspace"}, false, []string{"trust", "-trust-workspace"}},
		{[]string{"audit-export", "-trust-workspace"}, false, []string{"audit-export", "-trust-workspace"}},
	} {
		var got bool
		left := leadingTrustFlag(append([]string(nil), c.args...), &got)
		if got != c.trust || strings.Join(left, " ") != strings.Join(c.left, " ") {
			t.Errorf("%v: trust %v left %v", c.args, got, left)
		}
	}
}

// Every registered subcommand says whether it loads the workspace
// configuration, and the ones that never read it do not take the flag.
func TestSubcommandsDeclareTrust(t *testing.T) {
	never := map[string]bool{"init": true, "trust": true, "providers": true, "secret": true, "version": true}
	for _, c := range subcommands {
		if c.trust == never[c.name] {
			t.Errorf("%s: trust %v; a subcommand that loads the workspace configuration takes the flag, one that does not never does", c.name, c.trust)
		}
	}
}

// A headless run that fails says, where it fails, that the workspace file's
// model settings were not used.
func TestFailedRunNamesIgnoredModelSettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, `{"error":{"message":"no such model"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	home, ws := trustWorkspace(t, `{"model":{"default":"gpu","providers":{"gpu":{"type":"openai-compatible","base_url":"http://gpu.internal/v1","model":"big","context_window":8192}}}}`)
	user := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := mainHelper([]string{"-C", ws, "-p", "hi"})
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err == nil {
		t.Fatalf("the run against a failing model succeeded:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "note: the workspace configuration's model settings (model.default, model.providers.gpu) were ignored") ||
		!strings.Contains(out.String(), srv.URL) {
		t.Fatalf("the failure does not name the ignored model settings:\n%s", out.String())
	}
}
