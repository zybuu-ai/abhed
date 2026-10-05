package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/internal/webfetch"
)

// permEnv is recordedEnv with a session overlay on the engine.
func permEnv(t *testing.T, cfg config.Config, answers ...string) (*cmdEnv, *scriptedSurface, func() []agent.Event) {
	t.Helper()
	env, surface, events := recordedEnv(t, cfg, policy.ModeDefault, answers...)
	env.pol.Session = &policy.Overlay{}
	env.st.overlay = env.pol.Session
	return env, surface, events
}

func permissionChanges(t *testing.T, evs []agent.Event) []string {
	var out []string
	for _, p := range eventsOf[agent.PermissionChanged](t, evs, agent.EvPermissionChanged) {
		if p.Scope != "session" || p.By != agent.ByUser {
			t.Fatalf("permission change %+v", p)
		}
		out = append(out, p.Op+" "+p.List+" "+p.Rule)
	}
	return out
}

// A session allow is asked about once, a broad one twice; each added rule
// is recorded, takes effect, and ends when the conversation does.
func TestSessionAllowIsConfirmedRecordedAndCleared(t *testing.T) {
	env, surface, events := permEnv(t, config.Default(), ui.ChoiceYes)
	if _, err := slashPermissions(context.Background(), env, []string{"allow", "bash(go", "test*)"}); err != nil {
		t.Fatal(err)
	}
	if len(surface.asked) != 1 || surface.asked[0].Default != ui.ChoiceNo {
		t.Fatalf("asked %+v", surface.asked)
	}
	if got := env.pol.Evaluate("bash", true, cmd("go test ./...")); got.Decision != policy.Allow {
		t.Fatalf("the session rule does not apply: %+v", got)
	}
	env.st.fresh()
	if got := env.pol.Evaluate("bash", true, cmd("go test ./...")); got.Decision != policy.Ask {
		t.Fatalf("the session rule outlived /clear: %+v", got)
	}
	if got := permissionChanges(t, events()); !slices.Equal(got, []string{"add allow bash(go test*)"}) {
		t.Fatalf("recorded %v", got)
	}

	// A broad rule: one yes is not enough.
	env, surface, events = permEnv(t, config.Default(), ui.ChoiceYes)
	if _, err := slashPermissions(context.Background(), env, []string{"allow", "bash(*)"}); err != nil {
		t.Fatal(err)
	}
	if len(surface.asked) != 2 || len(events()) != 0 {
		t.Fatalf("a broad rule was added after %d questions, %d events", len(surface.asked), len(events()))
	}
	if got := env.pol.Evaluate("bash", true, cmd("make")); got.Decision != policy.Ask {
		t.Fatalf("an unconfirmed broad rule applies: %+v", got)
	}
	env, _, events = permEnv(t, config.Default(), ui.ChoiceYes, ui.ChoiceYes)
	if _, err := slashPermissions(context.Background(), env, []string{"allow", "bash"}); err != nil {
		t.Fatal(err)
	}
	if got := permissionChanges(t, events()); !slices.Equal(got, []string{"add allow bash"}) {
		t.Fatalf("recorded %v", got)
	}
}

// No answer, or a no, adds nothing.
func TestSessionAllowUnansweredAddsNothing(t *testing.T) {
	for _, answers := range [][]string{nil, {ui.ChoiceNo}, {""}} {
		env, _, events := permEnv(t, config.Default(), answers...)
		if _, err := slashPermissions(context.Background(), env, []string{"allow", "bash(make)"}); err != nil {
			t.Fatal(err)
		}
		if _, _, allow := env.pol.Session.SessionRules(); len(allow) != 0 || len(events()) != 0 {
			t.Fatalf("answers %q added %v", answers, allow)
		}
	}
}

// Under a managed configuration that sets the permissions, a session allow
// is refused before anyone is asked; deny and ask rules, which only tighten,
// are still added.
func TestSessionAllowIsRefusedUnderManagedPermissions(t *testing.T) {
	cfg := config.Default()
	cfg.Managed, cfg.ManagedKeys = true, []string{"permissions.deny"}
	env, surface, events := permEnv(t, cfg, ui.ChoiceYes, ui.ChoiceYes)
	_, err := slashPermissions(context.Background(), env, []string{"allow", "bash(make)"})
	if err == nil || !strings.Contains(err.Error(), "managed configuration") || len(surface.asked) != 0 {
		t.Fatalf("err %v, asked %d", err, len(surface.asked))
	}
	if _, err := slashPermissions(context.Background(), env, []string{"deny", "bash(make)"}); err != nil {
		t.Fatal(err)
	}
	if got := env.pol.Evaluate("bash", true, cmd("make")); got.Decision != policy.Deny {
		t.Fatalf("session deny under a managed file: %+v", got)
	}
	if got := permissionChanges(t, events()); !slices.Equal(got, []string{"add deny bash(make)"}) {
		t.Fatalf("recorded %v", got)
	}
}

// Removing a session rule is recorded; a configured rule cannot be removed here.
func TestSessionRuleRemoval(t *testing.T) {
	cfg := config.Default()
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "bash(sudo *)")
	env, _, events := permEnv(t, cfg)
	if _, err := slashPermissions(context.Background(), env, []string{"ask", "bash(make)"}); err != nil {
		t.Fatal(err)
	}
	if _, err := slashPermissions(context.Background(), env, []string{"remove", "bash(make)"}); err != nil {
		t.Fatal(err)
	}
	if _, err := slashPermissions(context.Background(), env, []string{"remove", "bash(sudo", "*)"}); err == nil {
		t.Fatal("a configured rule was removed as a session one")
	}
	if got := permissionChanges(t, events()); !slices.Equal(got, []string{"add ask bash(make)", "remove ask bash(make)"}) {
		t.Fatalf("recorded %v", got)
	}
}

// The listing names each rule's layer, managed rules as locked, and the
// allow rules the managed permissions left out.
func TestPermissionsListsRulesByLayer(t *testing.T) {
	managedConfig(t, `{"permissions":{"deny":["bash(curl *)"]}}`)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"permissions":{"ask":["bash(go build*)"],"allow":["bash(go run*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWith(t.TempDir(), config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	env, surface, _ := permEnv(t, cfg)
	if _, err := slashPermissions(context.Background(), env, []string{"deny", "bash(make)"}); err != nil {
		t.Fatal(err)
	}
	if _, err := slashPermissions(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}
	out := surface.text()
	for _, want := range []string{"managed (locked) | deny | bash(curl *)", "user | ask | bash(go build*)", "session | deny | bash(make)",
		"allow rules left out because the managed configuration sets the permissions: bash(go run*) (" + filepath.Join(home, ".abhed", "config.json") + ")"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// The user's allow rule is left out under the managed permissions.
	if strings.Contains(out, "| allow | bash(go run*)") {
		t.Fatalf("a dropped allow rule is listed as in force:\n%s", out)
	}
}

// explain says what policy would decide, at which step and by which rule,
// without asking a hook.
func TestPermissionsExplain(t *testing.T) {
	env, surface, _ := permEnv(t, config.Default())
	asked := false
	env.pol.Hooks = []policy.Hook{func(string, json.RawMessage) *policy.Result { asked = true; return nil }}
	if err := env.pol.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	if _, err := slashPermissions(context.Background(), env, []string{"explain", "bash", "curl", "http://x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := slashPermissions(context.Background(), env, []string{"explain", "bash", "rm", "-rf", "build"}); err != nil {
		t.Fatal(err)
	}
	if out := surface.text(); !strings.Contains(out, "deny · step deny · bash(curl *)") || !strings.Contains(out, "ask · step destructive · no rule") {
		t.Fatalf("explain said:\n%s", out)
	}
	if asked {
		t.Fatal("explain asked a hook about a call that is not being made")
	}
}

// A call the tool itself refuses is shown as refused, whatever policy says.
func TestPermissionsExplainShowsToolRefusals(t *testing.T) {
	env, surface, _ := permEnv(t, config.Default())
	env.pol.Mode = policy.ModeBypass
	kept := filepath.Join(env.sess.Root, "kept.txt")
	env.sess.Guard = func(p string, _ []string) error {
		if p == kept {
			return errors.New("kept.txt is kept from the agent")
		}
		return nil
	}
	for _, args := range [][]string{{"explain", "write", filepath.Join(t.TempDir(), "x")}, {"explain", "edit", kept}, {"explain", "write", filepath.Join(env.sess.Root, "fine.txt")}} {
		if _, err := slashPermissions(context.Background(), env, args); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(surface.text()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "refused · by the write tool") || !strings.Contains(lines[0], "policy alone: allow") ||
		!strings.Contains(lines[1], "refused · by the edit tool · kept.txt is kept from the agent") || !strings.HasPrefix(lines[2], "allow · step mode") {
		t.Fatalf("explain said:\n%s", surface.text())
	}
}

// Through the CLI: a session rule is recorded in the conversation, and
// /clear ends it, so the next conversation's listing has none.
func TestCLIPermissionsAcrossClear(t *testing.T) {
	c := startCLI(t)
	c.command("/permissions allow bash(go test*)", "answer 1-2")
	c.command("1", "session allow rule added")
	c.task("hello")
	if got := permissionChanges(t, c.export()); !slices.Equal(got, []string{"add allow bash(go test*)"}) {
		t.Fatalf("recorded %v", got)
	}
	c.command("/clear", "context cleared")
	c.command("/permissions", "Deny rules win in every mode")
	out := c.out.String()
	if last := out[strings.LastIndex(out, "Permission rules"):]; strings.Contains(last, "bash(go test*)") {
		t.Fatalf("the session rule outlived /clear:\n%s", last)
	}
}

func cmd(c string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": c})
	return b
}

// A rule added before any conversation held it, then cleared, leaves no
// record in the next conversation: it never applied there.
func TestClearedRuleIsNotRecordedInTheNextConversation(t *testing.T) {
	env, _, events := permEnv(t, config.Default())
	loop := env.st.loop
	env.st.loop = nil
	if _, err := slashPermissions(context.Background(), env, []string{"deny", "bash(make)"}); err != nil {
		t.Fatal(err)
	}
	if err := env.modes.Set(context.Background(), policy.ModePlan, agent.ViaSlash); err != nil {
		t.Fatal(err)
	}
	env.st.fresh()
	env.st.loop = loop
	env.st.flushPending()
	if got := permissionChanges(t, events()); len(got) != 0 {
		t.Fatalf("a cleared rule was recorded: %v", got)
	}
	if got := modeChanges(t, events()); !slices.Equal(got, []string{"default>plan/carried"}) {
		t.Fatalf("the mode, which /clear keeps, was not recorded once: %v", got)
	}
}

// A host web_fetch's own list refuses is shown as refused, not as the allow
// policy alone would give.
func TestPermissionsExplainShowsWebFetchHostRefusal(t *testing.T) {
	env, surface, _ := permEnv(t, config.Default())
	env.pol.Mode = policy.ModeBypass
	env.st.loop.Tools = tools.NewRegistry(&webfetch.Tool{AllowedHosts: []string{"docs.example.com"}})
	for _, u := range []string{"https://evil.example.net/x", "https://docs.example.com/x"} {
		if _, err := slashPermissions(context.Background(), env, []string{"explain", "web_fetch", u}); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(surface.text()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "refused · by the web_fetch tool") || !strings.HasPrefix(lines[1], "allow") {
		t.Fatalf("explain said:\n%s", surface.text())
	}
}

// A relative path is read from the session's folder, as the file tools read
// it, and the line names the path it explained; it used to be refused as not
// absolute.
func TestPermissionsExplainResolvesARelativePath(t *testing.T) {
	env, surface, _ := permEnv(t, config.Default())
	env.pol.Mode = policy.ModeBypass
	if _, err := slashPermissions(context.Background(), env, []string{"explain", "write", "sub/fine.txt"}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(env.sess.Cwd, "sub", "fine.txt") + ": allow · step mode"
	if out := strings.TrimSpace(surface.text()); !strings.HasPrefix(out, want) {
		t.Fatalf("explain said:\n%s\nwant prefix %s", out, want)
	}
}
