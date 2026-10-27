package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// askedStep is one entry of a session for the asked-folder tests: a person's
// message, a checked answer, or a call.
type askedStep struct {
	user, verdict  bool
	text           string // the message, or the call's output
	tool, cmd, out string
	path           string
	exit           int
}

func askedUser(s string) askedStep { return askedStep{user: true, text: s} }

var (
	askedVerdict = askedStep{verdict: true}
	askedSubRun  = askedStep{tool: "bash", cmd: "cd services/orders && go test ./...", out: "ok  \texample.com/mono/orders/internal/store\t(cached)\n"}
)

func askedEdit(p string) askedStep { return askedStep{tool: "edit", path: p, out: "Edited " + p} }

func askedRecord(rec *Recorder, steps []askedStep) {
	for i, s := range steps {
		switch {
		case s.user:
			rec.Record(EvUserMessage, ActorUser, Trusted, Message{Text: s.text})
		case s.verdict:
			rec.Record(EvAnswerVerdict, ActorSystem, Trusted, map[string]any{"mode": "label"})
		default:
			id := "c" + itoa(int64(i))
			args := map[string]string{"command": s.cmd}
			if s.path != "" {
				args = map[string]string{"path": s.path, "old_string": "a", "new_string": "b"}
			}
			raw, _ := json.Marshal(args)
			rec.Record(EvActionRequested, ActorAgent, Trusted, ActionRequested{CallID: id, Tool: s.tool, Args: raw})
			o := Observation{CallID: id, Tool: s.tool, Content: s.out}
			if s.tool == "bash" {
				o.ExitCode, o.Cwd = exit(s.exit), "/w"
			}
			rec.Record(EvObservation, ActorTool, Untrusted, o)
		}
	}
}

func askedCheck(t *testing.T, steps []askedStep, claim string) ClaimVerdict {
	t.Helper()
	store := NewMemStore()
	askedRecord(NewRecorder(store, "sess1", ""), steps)
	evs, _ := store.Events("sess1")
	v, _ := checkClaims(checkEnv{events: evs, workDir: "/w"}, textClaims(claim))
	return v[0]
}

// A pass claim that names no folder answers the folders the person asked
// about, and only then: a bare claim, every change inside them, and nothing
// in what they typed since the last answer asking about more.
func TestAskedFolders(t *testing.T) {
	orders := "Run the Go tests for services/orders."
	for _, c := range []struct {
		name  string
		steps []askedStep
		claim string
		want  string
	}{
		{"folder ending a sentence", []askedStep{askedUser(orders), askedSubRun}, "All tests passed.", ClaimVerified},
		{"result label", []askedStep{askedUser(orders), askedSubRun}, "Result: all tests passed.", ClaimVerified},
		{"tests are green", []askedStep{askedUser(orders), askedSubRun}, "Tests are green.", ClaimVerified},
		{"no folder asked", []askedStep{askedUser("Run the Go tests."), askedSubRun}, "All tests passed.", ClaimUnverified},
		{"everything works", []askedStep{askedUser(orders), askedSubRun}, "Everything works.", ClaimUnverified},
		{"whole build", []askedStep{askedUser(orders), askedSubRun}, "The whole build is green.", ClaimUnverified},
		{"all services", []askedStep{askedUser(orders), askedSubRun}, "Tests pass in all services.", ClaimUnverified},
		{"another component", []askedStep{askedUser(orders), askedSubRun}, "The billing service tests pass.", ClaimUnverified},
		{"entire suite", []askedStep{askedUser(orders), askedSubRun}, "The entire test suite passes.", ClaimUnverified},
		{"named other folder, shown in output", []askedStep{askedUser(orders), {tool: "bash", cmd: "cat Makefile", out: "test:\n\tcd services/billing && go test ./...\n"}, askedSubRun},
			"All tests in services/billing pass.", ClaimUnverified},
		{"steer narrows a wide request", []askedStep{askedUser("Make sure the whole repo's tests pass."), askedUser("Start with services/orders first."), askedSubRun}, "All tests pass.", ClaimUnverified},
		{"later message widens", []askedStep{askedUser(orders), askedUser("Actually make sure everything passes."), askedSubRun}, "All tests pass.", ClaimUnverified},
		{"a checked answer starts a new request", []askedStep{askedUser("Make sure the whole repo's tests pass."), askedVerdict, askedUser(orders), askedSubRun}, "All tests pass.", ClaimVerified},
		{"attachment names the folder", []askedStep{askedUser("Run the tests.\n\nWhat the person linked, read through the session's policy. " + AttachedNote + ": text.\n<page-1>see services/orders</page-1>"), askedSubRun}, "All tests passed.", ClaimUnverified},
		{"attachment says all", []askedStep{askedUser(orders + "\n\nWhat the person linked, read through the session's policy. " + AttachedNote + ": text.\n<page-1>run all the tests in the repository</page-1>"), askedSubRun}, "All tests passed.", ClaimVerified},
		{"a file is no folder", []askedStep{askedUser("Run the tests for services/orders/store.go"), askedSubRun}, "All tests passed.", ClaimUnverified},
		{"a path that climbs", []askedStep{askedUser("Run the tests for services/orders/../orders"), askedSubRun}, "All tests passed.", ClaimUnverified},
		{"change inside the folder", []askedStep{askedUser("Add a request log in services/orders"), askedEdit("/w/services/orders/log.go"), askedSubRun}, "All tests pass.", ClaimVerified},
		{"change outside the folder", []askedStep{askedUser("Add a request log in services/orders"), askedEdit("/w/libs/shared/log.go"), askedEdit("/w/services/orders/log.go"), askedSubRun}, "All tests pass.", ClaimUnverified},
		{"a command that may change anything", []askedStep{askedUser(orders), {tool: "bash", cmd: "./scripts/gen.sh"}, askedSubRun}, "All tests pass.", ClaimUnverified},
	} {
		if got := askedCheck(t, c.steps, c.claim); got.Verdict != c.want {
			t.Errorf("%s: %q is %s (%s), want %s", c.name, c.claim, got.Verdict, got.Reason, c.want)
		}
	}
	// Each word that asks about more than the folders turns the rule off.
	for _, w := range []string{"whole", "entire", "all", "every", "everything", "anything", "nothing", "repo", "repository", "workspace",
		"monorepo", "codebase", "project", "full", "complete", "other", "others", "else", "rest", "global", "globally", "everywhere", "across", "CI"} {
		msg := orders + " Mind the " + w + " too."
		if got := askedCheck(t, []askedStep{askedUser(msg), askedSubRun}, "All tests passed."); got.Verdict != ClaimUnverified {
			t.Errorf("%q: %s (%s), want unverified", msg, got.Verdict, got.Reason)
		}
	}
}

// A subagent's task, written by the model, is not what the person asked.
func TestAskedFoldersIgnoreSubagentTasks(t *testing.T) {
	store := NewMemStore()
	p, kid := NewRecorder(store, "sess1", ""), NewRecorder(store, "kid", "sess1")
	p.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "Run the Go tests."})
	kid.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "Run the Go tests for services/orders."})
	p.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{"session": "kid"})
	p.Record(EvSubagentReturn, ActorAgent, Trusted, map[string]any{"session": "kid", "reason": "completed"})
	askedRecord(p, []askedStep{askedSubRun})
	evs, _ := store.Events("sess1")
	v, _ := checkClaims(checkEnv{events: evs, workDir: "/w", child: store.Events}, textClaims("All tests passed."))
	if v[0].Verdict != ClaimUnverified || strings.Contains(v[0].Reason, "matches") {
		t.Errorf("a subagent's task set the folders: %s (%s)", v[0].Verdict, v[0].Reason)
	}
}
