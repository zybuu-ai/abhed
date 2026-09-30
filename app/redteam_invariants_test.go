package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/server"
)

// The invariant red-team checks that run today, over the piped CLI and the
// command paths. Each invariant also has a pseudo-terminal test in
// internal/clitest/redteam, which runs once that harness is built; the list
// there names both.

// startWith is startCLIConfig with a stub model and extra configuration
// merged at the top level of the workspace's file.
func startWith(t *testing.T, reply func(w io.Writer, n int, body string), extra string) *cliSession {
	t.Helper()
	return startCLIConfig(t, reply, func(url string) string {
		return `{` + extra + `"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
			url + `","model":"m","context_window":8192}}}}`
	})
}

// callThenText streams one tool call as the first reply and text after it.
func callThenText(name, args string) func(w io.Writer, n int, _ string) {
	return func(w io.Writer, n int, _ string) {
		if n == 1 {
			sseCall(w, n, name, args)
			return
		}
		textReply("done")(w, n)
	}
}

func deniedAt(t *testing.T, evs []agent.Event) []string {
	var out []string
	for _, p := range eventsOf[map[string]string](t, evs, agent.EvActionDenied) {
		out = append(out, p["step"])
	}
	return out
}

// Deny wins in every mode, bypass included, and over a session allow for
// the same command that the person confirmed.
func TestRedTeamDenyWinsInEveryMode(t *testing.T) {
	for _, mode := range []string{"default", "accept-edits", "auto", "bypass"} {
		t.Run(mode, func(t *testing.T) {
			c := startWith(t, callThenText("bash", `{"command":"curl http://example.invalid"}`),
				`"permissions":{"mode":"`+mode+`","deny":["bash(curl *)"]},`)
			c.command("/permissions allow bash(curl *)", "answer 1-2")
			c.command("yes", "session allow rule added")
			c.task("fetch it")
			if got := deniedAt(t, c.export()); !slices.Equal(got, []string{"deny"}) {
				t.Fatalf("denied at %v", got)
			}
		})
	}
}

// A policy hook's "allow" is no opinion: it approves nothing that would ask
// and lifts nothing that is denied.
func TestRedTeamHookAllowApprovesNothing(t *testing.T) {
	e := policy.New(policy.ModeDefault)
	if err := e.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	e.Hooks = []policy.Hook{func(string, json.RawMessage) *policy.Result {
		return &policy.Result{Decision: policy.Allow, Reason: "a hook says yes"}
	}}
	if got := e.Evaluate("bash", true, cmd("curl x")); got.Decision != policy.Deny {
		t.Fatalf("a hook's allow lifted a deny: %+v", got)
	}
	if got := e.Evaluate("bash", true, cmd("make")); got.Decision != policy.Ask {
		t.Fatalf("a hook's allow approved a call that asks: %+v", got)
	}
}

// Approvals are never auto-granted: input ending while a call waits refuses
// it, and nothing is written.
func TestRedTeamInputEndingRefuses(t *testing.T) {
	var ws atomic.Value
	c := startCLIWith(t, func(w io.Writer, n int, _ string) {
		if n == 1 {
			sseCall(w, n, "write", `{"path":`+strconv.Quote(filepath.Join(ws.Load().(string), "notes.txt"))+`,"content":"x"}`)
			return
		}
		textReply("done")(w, n)
	})
	ws.Store(c.ws)
	fmt.Fprintln(c.stdin, "write notes")
	c.waitFor(func(out string) bool { return strings.Contains(out, "[a]ccept") }, "the write to be asked about")
	_ = c.stdin.Close()
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("the CLI did not end cleanly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.ws, "notes.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a write was made with no answer: %v", err)
	}
}

// Destructive commands always confirm: in bypass mode, under an allow rule
// for them, with no "always" offered, and a no refuses.
func TestRedTeamDestructiveAlwaysConfirms(t *testing.T) {
	c := startWith(t, callThenText("bash", `{"command":"rm -rf build"}`),
		`"permissions":{"mode":"bypass","allow":["bash(rm *)"]},`)
	fmt.Fprintln(c.stdin, "clean")
	c.waitFor(func(out string) bool { return strings.Contains(out, "[a]ccept") }, "the command to be asked about")
	if strings.Contains(c.out.String(), "[A]lways") {
		t.Fatal("a destructive command offered to always allow")
	}
	c.command("r", " in / ")
	if got := deniedAt(t, c.export()); !slices.Equal(got, []string{"destructive"}) {
		t.Fatalf("denied at %v", got)
	}
}

// The managed configuration wins over every command that could widen the
// session: /mode auto, the bypass flag, /permissions allow and /add-dir are
// refused, before anyone is asked, and nothing is recorded.
func TestRedTeamManagedPolicyWins(t *testing.T) {
	t.Setenv("HOME", realDir(t))
	cfg := config.Default()
	cfg.Managed = true
	cfg.ManagedKeys = []string{"additional_dirs", "permissions.allow", "permissions.mode"}
	cfg.Permissions.Mode = "default"
	env, surface, events := permEnv(t, cfg, "yes", "yes", "yes", accessReadWrite)
	ctx := context.Background()
	if _, err := slashMode(ctx, env, []string{"auto"}); err == nil {
		t.Error("/mode auto under a managed mode")
	}
	if err := env.modes.Set(ctx, policy.ModeBypass, agent.ViaFlag); err == nil {
		t.Error("bypass under a managed file")
	}
	if _, err := slashPermissions(ctx, env, []string{"allow", "bash(make)"}); err == nil {
		t.Error("/permissions allow under managed rules")
	}
	if _, err := slashAddDir(ctx, env, []string{realDir(t)}); err == nil {
		t.Error("/add-dir under a managed list")
	}
	if len(surface.asked) != 0 || len(events()) != 0 || env.pol.Mode != policy.ModeDefault {
		t.Fatalf("asked %d, recorded %d, mode %s", len(surface.asked), len(events()), env.pol.Mode)
	}
	// Any managed file refuses bypass, whatever it sets.
	other := config.Default()
	other.Managed, other.ManagedKeys = true, []string{"sandbox.min_tier"}
	env, _, _ = permEnv(t, other)
	if err := env.modes.Set(ctx, policy.ModeBypass, agent.ViaFlag); err == nil || env.pol.Mode == policy.ModeBypass {
		t.Fatalf("bypass under a managed file that does not set the mode: %v", err)
	}
}

// What is typed goes to the model as text: a leading ! runs nothing and an
// @ attaches nothing, until they are built to go through policy.
func TestRedTeamTypedInputRunsAndAttachesNothing(t *testing.T) {
	c := startCLI(t)
	victim := filepath.Join(c.ws, "build")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := c.task("!rm -rf build and look at @.abhed/config.json")
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("a ! line ran a command")
	}
	if strings.Contains(body, "openai-compatible") {
		t.Fatal("an @ mention attached the state directory's config")
	}
}

// The record is append-only: the stores the CLI and the server write to
// offer no way to change or remove an event, and a session's events keep a
// contiguous sequence across /clear.
func TestRedTeamRecordIsAppendOnly(t *testing.T) {
	for _, iface := range []reflect.Type{reflect.TypeFor[agent.Store](), reflect.TypeFor[server.EventStore]()} {
		for i := range iface.NumMethod() {
			name := iface.Method(i).Name
			for _, verb := range []string{"Delete", "Update", "Remove", "Truncate", "Replace", "Rewrite", "Set"} {
				if strings.HasPrefix(name, verb) {
					t.Errorf("%s offers %s", iface, name)
				}
			}
		}
	}
	c := startCLI(t)
	c.task("one")
	c.command("/clear", "context cleared")
	c.task("two")
	checkRecord(t, c.export(), 1)
}

// Secrets are redacted: a stored secret's value that comes back in the
// model's reply or in a command's output reaches neither the screen nor the
// record.
func TestRedTeamSecretsAreRedacted(t *testing.T) {
	const canary = "abhed-canary-7f3c9e1d2b"
	home := realDir(t)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Open(filepath.Join(home, ".abhed", "secrets.json")).Set("API_TOKEN", canary); err != nil {
		t.Fatal(err)
	}
	c := startCLIHome(t, home, func(w io.Writer, n int, _ string) {
		switch n {
		case 1:
			sseCall(w, n, "bash", `{"command":"echo `+canary+`"}`)
		default:
			textReply("the token is "+canary)(w, n)
		}
	}, `"permissions":{"allow":["bash(echo *)"]},`)
	c.task("show me")
	for _, e := range c.export() {
		if strings.Contains(string(e.Payload), canary) {
			t.Errorf("a secret reached the record in %s", e.Type)
		}
	}
	if strings.Contains(c.out.String(), canary) {
		t.Fatal("a secret reached the screen")
	}
}

// startCLIHome is startCLIConfig with home as the CLI's home directory, for a
// test that puts something there first, such as the secrets store.
func startCLIHome(t *testing.T, home string, reply func(w io.Writer, n int, body string), extra string) *cliSession {
	t.Helper()
	c := &cliSession{t: t, out: &syncBuffer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(body))
		n := len(c.bodies)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		reply(w, n, string(body))
	}))
	t.Cleanup(srv.Close)
	c.ws = realDir(t)
	cfg := `{` + extra + `"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(c.ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c.cmd = exec.Command(os.Args[0], "-test.run=^TestConversationHelper$")
	// ABHED_TRUST_MAIN_ARGS keeps the helper's TestMain from giving it a home
	// of its own; the helper still runs TestConversationHelper.
	c.cmd.Env = append(os.Environ(), "ABHED_CONV_WS="+c.ws, "HOME="+home, "USERPROFILE="+home,
		"ABHED_TRUST_WORKSPACE=1", "ABHED_TRUST_MAIN_ARGS=[]", secrets.EnvFile+"=")
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.stdin = stdin
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.stdin.Close()
		done := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = c.cmd.Process.Kill() // the helper this test started
			<-done
		}
	})
	return c
}
