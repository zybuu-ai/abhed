package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// `abhed version --json` prints the same block without starting a session.
func TestVersionJSON(t *testing.T) {
	acpModelsEnv(t, `{}`)
	var out bytes.Buffer
	if err := versionJSON(&out, acpBuild{Version: "1.2.3", Edition: "ce", Commit: "c0ffee"}, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m["apiLevel"] != float64(1) || m["commit"] != "c0ffee" {
		t.Fatalf("version --json: %s %v", out.String(), err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".abhed", "records")); err == nil {
		t.Fatal("version --json opened the record")
	}
}

// §3.1: reasoning streams as thought chunks, and its whole text is not sent
// again; the stop reason comes from the loop's terminal reason.
func TestStudioReasoningDeltasSentOnce(t *testing.T) {
	r := newStudioRig(t, "", `{"choices":[{"delta":{"reasoning_content":"think one","content":"answer"}}]}`)
	id := r.open()
	stop, ups := r.prompt(id, "hi")
	if stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}
	var thoughts []string
	for _, u := range ups {
		if u["sessionUpdate"] == "agent_thought_chunk" {
			thoughts = append(thoughts, u["content"].(map[string]any)["text"].(string))
		}
	}
	if strings.Join(thoughts, "") != "think one" {
		t.Fatalf("thought chunks %q: the reasoning should arrive once", thoughts)
	}
}

func TestStopReasonsFromTheTerminalReason(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		ctx          context.Context
		err          error
		stop, reason string
	}{
		{ctx, nil, "end_turn", ""},
		{ctx, &abhed.EndedError{Reason: agent.TermMaxTurns}, "max_turn_requests", "max_turns"},
		{ctx, &abhed.EndedError{Reason: agent.TermWakeLimit}, "max_turn_requests", "wake_limit"},
		{ctx, &abhed.EndedError{Reason: agent.TermUserInterrupt}, "cancelled", "user_interrupt"},
		{ctx, &abhed.EndedError{Reason: agent.TermMaxBudget}, "max_tokens", "budget"},
		{ctx, &abhed.EndedError{Reason: agent.TermPolicyDenied}, "refusal", "policy_denied"},
		// An error whose text says "turn" is no longer read as the turn limit.
		{ctx, errors.New("the turn failed"), "refusal", "error"},
		{cancelled, context.Canceled, "cancelled", "user_interrupt"},
	} {
		stop, reason := stopReason(tc.ctx, tc.err)
		if stop != tc.stop || reason != tc.reason {
			t.Errorf("%v: %s/%s, want %s/%s", tc.err, stop, reason, tc.stop, tc.reason)
		}
	}
}

// §3.3: the command list arrives after session/new; built-ins win; a
// workspace's commands are listed only when it is trusted; running one is
// recorded command.invoked with its source and hash.
func TestStudioCommands(t *testing.T) {
	r := newStudioRig(t, "", say("expanded ran"))
	home := r.home
	if err := os.MkdirAll(filepath.Join(home, ".abhed", "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "commands", "greet.md"), []byte("---\ndescription: say hi\n---\nGreet $ARGUMENTS"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A user command named like a built-in loses to it.
	if err := os.WriteFile(filepath.Join(home, ".abhed", "commands", "compact.md"), []byte("not the built-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.write(".abhed/commands/wsonly.md", "from the workspace")
	from := r.cl.mark()
	id := r.open()
	u := r.cl.waitFor(from, "available_commands_update", func(m rpcMessage) bool {
		return m.Method == "session/update" && strings.Contains(string(m.Params), "available_commands_update")
	})
	cmds := updates([]rpcMessage{u})[0]["availableCommands"].([]any)
	sources := map[string]string{}
	for _, c := range cmds {
		c := c.(map[string]any)
		sources[c["name"].(string)] = meta(c)["source"].(string)
	}
	if sources["compact"] != "builtin" || sources["greet"] != "user" || sources["tasks"] != "builtin" {
		t.Fatalf("commands: %v", sources)
	}
	if _, listed := sources["wsonly"]; listed {
		t.Fatal("an untrusted workspace's command was listed")
	}
	stop, _ := r.prompt(id, "/greet the team")
	if stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}
	got, actors := r.recorded(id, agent.EvCommandInvoked)
	if len(got) != 1 || got[0]["name"] != "/greet" || got[0]["source"] != "user" || got[0]["sha256"] == "" || actors[0] != agent.ActorUser {
		t.Fatalf("command.invoked: %v %v", got, actors)
	}
	msgs, _ := r.recorded(id, agent.EvUserMessage)
	if len(msgs) != 1 || msgs[0]["text"] != "Greet the team" {
		t.Fatalf("the command was not expanded: %v", msgs)
	}
	// A built-in runs here, answered as text.
	_, ups := r.prompt(id, "/tasks")
	if !strings.Contains(textOf(ups), "No background tasks") {
		t.Fatalf("/tasks said %q", textOf(ups))
	}
}

// textOf joins the agent message chunks among updates.
func textOf(ups []map[string]any) string {
	var b strings.Builder
	for _, u := range ups {
		if u["sessionUpdate"] == "agent_message_chunk" {
			b.WriteString(u["content"].(map[string]any)["text"].(string))
		}
	}
	return b.String()
}

// §3.4: usage carries the token counts; no cost is invented.
func TestStudioUsageMeta(t *testing.T) {
	r := newStudioRig(t, "", say("ok"))
	id := r.open()
	_, ups := r.prompt(id, "hi")
	for _, u := range ups {
		if u["sessionUpdate"] != "usage_update" {
			continue
		}
		m := meta(u)
		if m["tokensIn"] == nil || m["sessionTotalIn"] == nil || u["cost"] != nil {
			t.Fatalf("usage_update: %v", u)
		}
		return
	}
	t.Fatalf("no usage_update in %v", ups)
}

// §1.1, §5.6: the legacy _meta key is still read on input; the reply's
// trust report is under zybuu.ai/abhed only.
func TestStudioMetaKeys(t *testing.T) {
	r := newStudioRig(t, "")
	m := r.cl.call("session/new", map[string]any{"cwd": r.ws, "mcpServers": []any{}, "_meta": map[string]any{"abhed": map[string]any{"trust": "untrusted"}}})
	if m.Error != nil {
		t.Fatal(m.Error.Message)
	}
	var res map[string]any
	_ = json.Unmarshal(m.Result, &res)
	if meta(res)["workspaceTrust"] == nil || res["_meta"].(map[string]any)["abhed"] != nil {
		t.Fatalf("result meta: %v", res["_meta"])
	}
}

// §2.1: a field the engine does not know is refused, never ignored, since a
// later one could widen what a session does.
func TestRuleUnknownMetaFieldsAreRefused(t *testing.T) {
	r := newStudioRig(t, "")
	for _, key := range []string{acpMetaKey, "abhed"} {
		r.cl.refused(errParams, "session/new", map[string]any{"cwd": r.ws, "mcpServers": []any{},
			"_meta": map[string]any{key: map[string]any{"trust": "untrusted", "allow": []string{"bash(*)"}}}})
	}
	for _, method := range []string{"_abhed/trust/grant", "_abhed/policy/set", "_abhed/tool/run", "_abhed/mcp/add"} {
		r.cl.refused(errNoMethod, method, map[string]any{"cwd": r.ws})
	}
	// MCP servers a client names are not started; the reply says which.
	var res map[string]any
	r.cl.ok("session/new", map[string]any{"cwd": r.ws, "mcpServers": []any{map[string]any{"name": "evil", "command": "/bin/sh"}}}, &res)
	if refused := meta(res)["mcpServersRefused"]; refused == nil || refused.([]any)[0] != "evil" {
		t.Fatalf("mcpServersRefused: %v", meta(res))
	}
}
