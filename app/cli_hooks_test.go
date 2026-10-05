package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
)

// gatekeeper is an extension script that refuses messages saying
// "forbidden" and tries to grant every permission request.
const gatekeeper = `#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"event":"user_prompt_submit"'*forbidden*) echo '{"block":true,"reason":"that topic is off limits"}' ;;
    *'"event":"permission_request"'*) echo '{"allow":true,"reason":"I approve"}' ;;
    *) echo '{}' ;;
  esac
done
`

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// Each hook that fires is recorded in the open conversation as hook.fired,
// and a refused message is said to the person.
func TestHookFiredIsRecorded(t *testing.T) {
	env, surface, events := recordedEnv(t, config.Default(), "default")
	host := extension.NewHost(nil)
	if errs := host.Load(context.Background(), []extension.Config{{Name: "gate", Command: "bash",
		Args: []string{writeScript(t, gatekeeper)}, Events: []extension.Event{extension.EvUserPromptSubmit, extension.EvPermissionRequest}}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Cleanup(host.Close)
	env.st.hooks = host
	env.st.attachHooks(env.st.loop)
	hooks := env.st.loop.Hooks
	if why := hooks.PromptSubmitted(context.Background(), "s1", "a forbidden thing"); why == "" {
		t.Fatal("not refused")
	}
	if why := hooks.PermissionRequested(context.Background(), env.pol, "s1", "bash", []byte(`{"command":"make"}`), "asks"); why != "" {
		t.Fatalf("a permission request was refused: %q", why)
	}
	got := eventsOf[agent.HookFired](t, events(), agent.EvHookFired)
	if len(got) != 2 || got[0].Verdict != "block" || got[0].Event != "user_prompt_submit" || got[1].Verdict != "annotate" {
		t.Fatalf("recorded %+v", got)
	}
	if !strings.Contains(surface.text(), "your message was not sent: that topic is off limits") {
		t.Fatalf("shown:\n%s", surface.text())
	}
}

// /hooks lists each extension with its layer, events, matcher and status,
// and says hooks never approve.
func TestHooksPanel(t *testing.T) {
	cfg := config.Default()
	cfg.Extensions = []config.ExtensionConfig{
		{Name: "gate", Command: "bash", Events: []string{"user_prompt_submit"}},
		{Name: "git-guard", Command: "bash", Events: []string{"tool_call"}, Match: []string{"bash(git *)"}, Async: true},
	}
	env, surface, _ := recordedEnv(t, cfg, "default")
	if _, err := slashHooks(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}
	out := surface.text()
	for _, want := range []string{"gate | config | user_prompt_submit |  | not started",
		"git-guard | config | tool_call (async) | bash(git *) | not started", "never approve one"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// Under hooks.managed_only, /hooks says a non-managed extension takes no hooks.
func TestHooksPanelManagedOnly(t *testing.T) {
	cfg := config.Default()
	cfg.Hooks.ManagedOnly = true
	cfg.Extensions = []config.ExtensionConfig{{Name: "gate", Command: "bash", Events: []string{"user_prompt_submit"}}}
	env, surface, _ := recordedEnv(t, cfg, "default")
	if _, err := slashHooks(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}
	out := surface.text()
	for _, want := range []string{"off: only the managed configuration's extensions take hooks", "limits hooks to its own extensions"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// Through the CLI: a user_prompt_submit hook refuses a message, the person is
// told, the model never sees it, and the record says the hook fired.
func TestCLIPromptHookVeto(t *testing.T) {
	script := writeScript(t, gatekeeper)
	c := startCLIConfig(t, func(w io.Writer, n int, _ string) { textReply("fine")(w, n) }, func(url string) string {
		return `{"extensions":[{"name":"gate","command":"bash","args":[` + jsonQuote(script) + `],"events":["user_prompt_submit"]}],` +
			`"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	})
	c.command("tell me the forbidden thing", "your message was not sent: that topic is off limits")
	// The refused message's turn may print usage of its own, so the wait is
	// for the model's answer to the next one, not for a count of turns.
	c.command("hello", "fine")
	if n := c.requests(); n != 1 {
		t.Fatalf("the model was sent %d requests; the refused message must not be one", n)
	}
	evs := c.export()
	fired := eventsOf[agent.HookFired](t, evs, agent.EvHookFired)
	if len(fired) != 1 || fired[0].Extension != "gate" || fired[0].Verdict != "block" {
		t.Fatalf("recorded %+v", fired)
	}
	for _, m := range eventsOf[agent.Message](t, evs, agent.EvUserMessage) {
		if strings.Contains(m.Text, "forbidden") {
			t.Fatal("the refused message was recorded as sent")
		}
	}
}

func jsonQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// A prompt hook that has stopped fails open, and the person is told the
// message went unscreened.
func TestDeadPromptHookIsSaid(t *testing.T) {
	env, surface, _ := recordedEnv(t, config.Default(), "default")
	host := extension.NewHost(nil)
	if errs := host.Load(context.Background(), []extension.Config{{Name: "dlp", Command: "bash",
		Args: []string{writeScript(t, "#!/bin/bash\nread -r line\nexit 1\n")}, Events: []extension.Event{extension.EvUserPromptSubmit}}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Cleanup(host.Close)
	env.st.hooks = host
	env.st.attachHooks(env.st.loop)
	if why := env.st.loop.Hooks.PromptSubmitted(context.Background(), "s1", "hello"); why != "" {
		t.Fatalf("refused: %q", why)
	}
	if !strings.Contains(surface.text(), "not screened: the user_prompt_submit hook dlp is not running") {
		t.Fatalf("shown:\n%s", surface.text())
	}
}
