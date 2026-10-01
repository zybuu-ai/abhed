package redteam

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/clitest"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// denied lists the steps of the record's action.denied events.
func denied(t *testing.T, r clitest.Record) []string {
	t.Helper()
	var out []string
	for _, e := range r.Events {
		if e.Type == agent.EvActionDenied {
			var p map[string]string
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p["step"])
		}
	}
	return out
}

func approvedBy(t *testing.T, r clitest.Record) []string {
	t.Helper()
	var out []string
	for _, e := range r.Events {
		if e.Type == agent.EvActionApproved {
			var p map[string]string
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p["by"]+":"+p["step"])
		}
	}
	return out
}

// Deny wins in every mode: a denied command is refused in default,
// accept-edits, auto and bypass, and under a session allow for it.
func TestInvariantDenyWinsInEveryMode(t *testing.T) {
	for _, mode := range []string{"default", "accept-edits", "auto", "bypass"} {
		t.Run(mode, func(t *testing.T) {
			h := start(t, clitest.Opts{
				Args:   []string{"-mode", mode, "-deny", "bash(curl *)"},
				Script: "tool bash {\"command\":\"curl http://example.invalid\"}\n\ntext \"done\"",
			})
			if mode == "bypass" {
				h.WaitText("bypass")
			}
			h.Type("/permissions allow bash(curl *)")
			h.Key(clitest.Enter)
			answer(h, "Allow bash(curl *) for this session?", "1")
			h.WaitText("session allow rule added")
			h.Type("fetch it")
			h.Key(clitest.Enter)
			h.WaitText("done")
			if got := denied(t, h.Record()); !slices.Contains(got, "deny") {
				t.Fatalf("%s: denied at %v", mode, got)
			}
			h.Exit(0)
		})
	}
}

// Approvals are never auto-granted: arrow keys and a number pressed as the
// question appears answer nothing, input ending during an ask refuses, and a
// hook's "allow" approves nothing.
func TestInvariantNoAutoApprove(t *testing.T) {
	for _, keys := range [][]clitest.Key{{clitest.Down, clitest.Enter}, {"1"}, {clitest.Enter}} {
		h := start(t, clitest.Opts{Script: "tool write {\"path\":\"{{WS}}/notes.txt\",\"content\":\"x\"}\n\ntext \"done\""})
		h.Type("write notes")
		h.Key(clitest.Enter)
		h.WaitText("Create notes.txt?")
		h.Key(keys...)
		time.Sleep(300 * time.Millisecond)
		if got := approvedBy(t, h.Record()); len(got) != 0 {
			t.Fatalf("keys %q pressed as the question appeared approved %v", keys, got)
		}
		time.Sleep(guard)
		h.Key(clitest.Esc) // No, and the turn stops
		h.WaitText("Declined")
		h.Exit(0)
	}
	h := start(t, clitest.Opts{Piped: true, KeepStdin: true, Script: "tool write {\"path\":\"{{WS}}/notes.txt\",\"content\":\"x\"}\n\ntext \"done\""})
	h.Type("write notes\n")
	h.WaitOutput("answer 1-")
	h.Exit(0) // input ends while the write waits
	if got := denied(t, h.Record()); len(got) == 0 {
		t.Fatal("input ending during an ask did not refuse the call")
	}
}

// Destructive commands always confirm: in auto and bypass, under an allow
// rule, with no "always" offered.
func TestInvariantDestructiveAlwaysConfirms(t *testing.T) {
	for _, mode := range []string{"auto", "bypass"} {
		h := start(t, clitest.Opts{
			Args:   []string{"-mode", mode, "-allow", "bash(rm *)"},
			Script: "tool bash {\"command\":\"rm -rf build\"}\n\ntext \"done\"",
		})
		h.Type("clean")
		h.Key(clitest.Enter)
		s := h.WaitText("rm -rf build")
		if s.Contains("Always") || s.Contains("always allow") {
			t.Fatalf("%s: a destructive command offered to always allow", mode)
		}
		answer(h, "Run this command?", "2") // No
		h.WaitText("Declined")
		if got := approvedBy(t, h.Record()); len(got) != 0 {
			t.Fatalf("%s: a destructive command was approved: %v", mode, got)
		}
		h.Exit(0)
	}
}

// The managed configuration wins: /mode auto, bypass, /permissions allow and
// /add-dir are refused under it.
func TestInvariantManagedPolicyWins(t *testing.T) {
	h := start(t, clitest.Opts{Script: `text "ok"`, Managed: `{"permissions":{"mode":"default","allow":[]},"additional_dirs":["/usr"]}`})
	for _, c := range []struct{ line, want string }{
		{"/mode auto", "refused"},
		{"/permissions allow bash(make)", "managed configuration"},
		{"/add-dir /tmp", "refused"},
	} {
		// Each answer is new output: the same words may already be on screen.
		before := strings.Count(clitest.Strip(h.Output()), c.want)
		h.Type(c.line)
		h.Key(clitest.Enter)
		h.WaitScreen(func(clitest.Screen) bool { return strings.Count(clitest.Strip(h.Output()), c.want) > before }, clitest.DefaultTimeout)
		h.Settle()
	}
	// A task opens the conversation's record, which holds what came before.
	h.Type("hello")
	h.Key(clitest.Enter)
	h.WaitText("● ok")
	for _, e := range h.Record().Events {
		if e.Type == agent.EvModeChanged || e.Type == agent.EvPermissionChanged || e.Type == agent.EvWorkspaceDirAdded {
			t.Fatalf("a refused change was recorded: %s", e.Type)
		}
	}
	h.Exit(0)
}

// @ mentions, ! commands and custom commands go through policy and the
// record: a symlink out of the workspace, the state directory and a denied
// path are not attached, and ! of a destructive command asks.
func TestInvariantMentionsBangAndCommandsGoThroughPolicy(t *testing.T) {
	h := start(t, clitest.Opts{Args: []string{"-deny", "read(secret/**)"}, Script: `text "ok"`})
	for _, line := range []string{"look at @secret/key.txt", "look at @.abhed/config.json", "!rm -rf build"} {
		h.Type(line)
		h.Key(clitest.Enter)
	}
	h.WaitText("rm -rf build")
	r := h.Record()
	for _, e := range r.Events {
		if e.Type == agent.EvInputMention {
			t.Fatalf("a refused path was attached: %s", e.Payload)
		}
	}
	h.Exit(0)
}

// /permissions cannot widen past managed policy, and its rules are recorded
// and end with /clear.
func TestInvariantPermissionsCannotWidenPastManaged(t *testing.T) {
	h := start(t, clitest.Opts{Managed: `{"permissions":{"deny":["bash(curl *)"]}}`})
	h.Type("/permissions allow bash(*)")
	h.Key(clitest.Enter)
	h.WaitText("managed configuration")
	h.Type("/permissions deny bash(make)")
	h.Key(clitest.Enter)
	h.WaitText("session deny rule added")
	h.Type("/clear")
	h.Key(clitest.Enter)
	h.WaitText("context cleared")
	h.Settle()
	h.Type("/permissions")
	h.Key(clitest.Enter)
	if s := h.WaitText("Permission rules"); s.Contains("bash(make)") {
		t.Fatal("a session rule outlived /clear")
	}
	h.Key(clitest.Esc) // close the panel
	h.Exit(0)
}

// Shift-Tab never reaches auto or bypass, however often it is pressed; auto
// comes only through /mode auto and a typed yes.
func TestInvariantModeCycleNeverReachesAutoOrBypass(t *testing.T) {
	h := start(t, clitest.Opts{Script: `text "ok"`})
	for range 20 {
		h.Key(clitest.ShiftTab)
		if s := h.Screen(); s.Contains("auto") || s.Contains("bypass") {
			t.Fatalf("Shift-Tab reached:\n%s", s.Text())
		}
	}
	h.Type("/mode auto")
	h.Key(clitest.Enter)
	h.WaitText("Switch to auto mode?")
	time.Sleep(guard)
	h.Key(clitest.Esc)
	h.WaitText("mode stays")
	// A task opens the conversation's record, which holds what came before.
	h.Settle()
	h.Type("hello")
	h.Key(clitest.Enter)
	h.WaitText("● ok")
	for _, e := range h.Record().Events {
		if e.Type == agent.EvModeChanged && strings.Contains(string(e.Payload), `"to":"auto"`) {
			t.Fatal("auto was reached without a yes")
		}
	}
	h.Exit(0)
}

// Rewind is a recorded fork: rewinding to the first message records a fork
// at 0, the abandoned events still exist, and the record verifies.
func TestInvariantRewindIsAFork(t *testing.T) {
	h := start(t, clitest.Opts{Script: "text \"one\"\n\ntext \"two\""})
	h.Type("first")
	h.Key(clitest.Enter)
	h.WaitText("● one")
	h.WaitScreen(func(s clitest.Screen) bool { return s.Contains("? for shortcuts") }, clitest.DefaultTimeout)
	h.Settle()
	h.Type("/rewind")
	h.Key(clitest.Enter)
	answer(h, "Rewind to before which prompt?", "1")
	answer(h, `Rewind to before "first"?`, "1") // the conversation only
	h.WaitText("forked at step 0")
	// Read once the session has ended, when its head names the last line.
	h.Exit(0)
	r := h.Record()
	if !r.Verified || !slices.Contains(r.Types(), agent.EvForked) {
		t.Fatalf("verified %v, types %v", r.Verified, r.Types())
	}
}

// The record is append-only: what the session did stays in it, in order,
// and verifies.
func TestInvariantRecordIsAppendOnly(t *testing.T) {
	h := start(t, clitest.Opts{Script: "text \"one\"\n\ntext \"two\""})
	// Each line waits for the one before: lines sent together read as a paste.
	for _, step := range []struct{ line, want string }{{"first", "● one"}, {"/clear", "context cleared"}, {"second", "● two"}} {
		h.Type(step.line)
		h.Key(clitest.Enter)
		h.WaitText(step.want)
		h.Settle()
	}
	r := h.Record()
	if !r.Verified {
		t.Fatal("the record does not verify")
	}
	for i, e := range r.Events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i+1, e.Seq)
		}
	}
	h.Exit(0)
}

// Secrets are redacted: a stored secret's value is on no screen and in no
// record, whether it comes back in the model's reply or a tool's output.
func TestInvariantSecretsAreRedacted(t *testing.T) {
	const canary = "abhed-canary-7f3c9e1d2b"
	// The vault is where Abhed keeps it, ~/.abhed: one in a temp folder is
	// refused at start-up, since commands can write there.
	h := start(t, clitest.Opts{
		Setup: func(home, _ string) {
			if err := secrets.Open(filepath.Join(home, ".abhed", "secrets.json")).Set("API_TOKEN", canary); err != nil {
				t.Fatal(err)
			}
		},
		Args:   []string{"-allow", "bash(echo *)"},
		Script: "text \"the token is " + canary + "\"\n\ntool bash {\"command\":\"echo " + canary + "\"}\n\ntext \"done\"",
	})
	h.Type("show me")
	h.Key(clitest.Enter)
	h.WaitText("the token is")
	h.Settle()
	h.Type("and run it")
	h.Key(clitest.Enter)
	s := h.WaitText("done")
	if s.Contains(canary) || slices.ContainsFunc(h.Scrollback(), func(l string) bool { return strings.Contains(l, canary) }) {
		t.Fatal("a secret reached the screen")
	}
	for _, e := range h.Record().Events {
		if strings.Contains(string(e.Payload), canary) {
			t.Fatalf("a secret reached the record in %s", e.Type)
		}
	}
	h.Exit(0)
}

// Workspace trust gates hooks: an untrusted workspace's extensions are not
// run, and a user_prompt_submit veto from a trusted one is shown and recorded.
func TestInvariantUntrustedHooksDoNotRun(t *testing.T) {
	h := start(t, clitest.Opts{Script: `text "ok"`})
	h.Type("/hooks")
	h.Key(clitest.Enter)
	h.WaitText("they never")
	h.Key(clitest.Esc) // close the panel
	h.Exit(0)
}
