package app

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// reviewRig is customRig over a git repository with one committed file
// changed in the working tree.
func reviewRig(t *testing.T) (*cliState, *agent.MemStore, *scriptSurface) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	st, store, sf := customRig(t)
	ws := st.sess.Root
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	write(t, filepath.Join(ws, "calc.go"), "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	write(t, filepath.Join(ws, "calc.go"), "package calc\n\nfunc Add(a, b int) int { return a - b } // REVIEW-MARK\n")
	return st, store, sf
}

// /review reads the diff as a recorded call, sends the built-in prompt with
// it in plan mode, and puts the earlier mode back when the turn ends.
func TestReviewSendsTheDiffInPlanMode(t *testing.T) {
	for _, name := range []string{"/review", "/security-review"} {
		st, store, _ := reviewRig(t)
		typeLine(t, st, name)
		turn := st.takeTurn()
		if turn == nil {
			t.Fatalf("%s sent no turn", name)
		}
		if !strings.Contains(turn.msg.Text, "REVIEW-MARK") || !strings.Contains(turn.msg.Text, "plan mode") {
			t.Fatalf("%s turn lacks the diff or the prompt: %q", name, turn.msg.Text)
		}
		if st.loop.Policy.Mode != policy.ModePlan {
			t.Fatalf("%s turn runs in %s, not plan", name, st.loop.Policy.Mode)
		}
		if len(storeEventsOf(t, store, agent.EvObservation)) != 1 {
			t.Fatalf("%s: the diff was not read as a recorded call", name)
		}
		ev := storeEventsOf(t, store, agent.EvCommandInvoked)
		var p agent.CommandInvoked
		if len(ev) != 1 || json.Unmarshal(ev[0].Payload, &p) != nil || p.Name != name || p.Source != sourceBuiltin || len(p.SHA256) != 64 {
			t.Fatalf("%s command.invoked: %+v", name, p)
		}
		turn.done()
		if st.loop.Policy.Mode != policy.ModeDefault {
			t.Fatalf("%s left the session in %s", name, st.loop.Policy.Mode)
		}
		if len(storeEventsOf(t, store, agent.EvModeChanged)) != 2 {
			t.Fatalf("%s: the mode changes were not recorded", name)
		}
	}
}

// In plan mode the shell refuses git diff, so /review says how to run it and sends nothing.
func TestReviewRefusedInPlanMode(t *testing.T) {
	st, store, _ := reviewRig(t)
	st.loop.Policy.Mode = policy.ModePlan
	typeLine(t, st, "/review")
	if st.takeTurn() != nil || len(storeEventsOf(t, store, agent.EvCommandInvoked)) != 0 {
		t.Fatal("/review ran in plan mode")
	}
}

// A deny rule on the command that reads the diff holds for /review.
func TestReviewHonoursDenyRules(t *testing.T) {
	st, store, _ := reviewRig(t)
	if err := st.loop.Policy.AddDeny("bash(git *)"); err != nil {
		t.Fatal(err)
	}
	typeLine(t, st, "/review")
	if st.takeTurn() != nil || len(storeEventsOf(t, store, agent.EvActionDenied)) != 1 {
		t.Fatal("/review read the diff past a deny rule")
	}
	if st.loop.Policy.Mode != policy.ModeDefault {
		t.Fatalf("a refused review changed the mode to %s", st.loop.Policy.Mode)
	}
}

// A base that is not a plain revision never reaches the shell.
func TestReviewRefusesAShellBase(t *testing.T) {
	st, store, _ := reviewRig(t)
	for _, base := range []string{"HEAD;touch${IFS}x", "$(id)", "-o/tmp/x", "`id`", "a|b"} {
		typeLine(t, st, "/review "+base)
		if st.takeTurn() != nil || len(storeEventsOf(t, store, agent.EvObservation)) != 0 {
			t.Fatalf("base %q ran", base)
		}
	}
	typeLine(t, st, "/review HEAD")
	if st.takeTurn() == nil {
		t.Fatal("a plain base was refused")
	}
}

// A custom command named review does not replace the built-in, and the clash is reported.
func TestReviewBuiltinBeatsACustomCommand(t *testing.T) {
	st, store, sf := reviewRig(t)
	userCommand(t, "review.md", "HIJACKED-REVIEW")
	typeLine(t, st, "/review")
	turn := st.takeTurn()
	if turn == nil || strings.Contains(turn.msg.Text, "HIJACKED-REVIEW") {
		t.Fatalf("the custom command ran in place of /review: %+v", turn)
	}
	if !strings.Contains(sf.shown(), "the built-in /review has that name") {
		t.Fatalf("the clash was not reported: %q", sf.shown())
	}
	var p agent.CommandInvoked
	ev := storeEventsOf(t, store, agent.EvCommandInvoked)
	if len(ev) != 1 || json.Unmarshal(ev[0].Payload, &p) != nil || p.Source != sourceBuiltin {
		t.Fatalf("command.invoked: %+v", p)
	}
}

// With nothing changed, no turn is sent and the mode does not move.
func TestReviewWithNoChanges(t *testing.T) {
	st, _, sf := reviewRig(t)
	write(t, filepath.Join(st.sess.Root, "calc.go"), "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	typeLine(t, st, "/review")
	if st.takeTurn() != nil || st.loop.Policy.Mode != policy.ModeDefault || !strings.Contains(sf.shown(), "no changes") {
		t.Fatalf("an empty review: %q", sf.shown())
	}
}
