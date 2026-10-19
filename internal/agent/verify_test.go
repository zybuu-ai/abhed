package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// rec builds a session record by hand, for the checker's unit tests.
type rec struct {
	t     *testing.T
	store *MemStore
	r     *Recorder
}

func newRec(t *testing.T) *rec {
	s := NewMemStore()
	return &rec{t: t, store: s, r: NewRecorder(s, "sess1", "")}
}

// call records a call and its result, and returns the result's seq.
func (b *rec) call(id, tool string, args any, out string, exit *int) int64 {
	b.t.Helper()
	raw, _ := json.Marshal(args)
	if _, err := b.r.Record(EvActionRequested, ActorAgent, Trusted, ActionRequested{CallID: id, Tool: tool, Args: raw}); err != nil {
		b.t.Fatal(err)
	}
	ev, err := b.r.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: id, Tool: tool, Content: out, ExitCode: exit, IsError: exit != nil && *exit != 0})
	if err != nil {
		b.t.Fatal(err)
	}
	return ev.Seq
}

func (b *rec) events() []Event {
	evs, _ := b.store.Events("sess1")
	return evs
}

func exit(n int) *int { return &n }

func checkText(t *testing.T, b *rec, dir, text string) []ClaimVerdict {
	t.Helper()
	env := checkEnv{events: b.events(), workDir: dir, readFile: os.ReadFile}
	v, _ := checkClaims(env, textClaims(text))
	return v
}

func checkCited(t *testing.T, b *rec, dir string, claims ...Claim) []ClaimVerdict {
	t.Helper()
	env := checkEnv{events: b.events(), workDir: dir, readFile: os.ReadFile}
	v, _ := checkClaims(env, claims)
	return v
}

func wantVerdict(t *testing.T, v ClaimVerdict, want string) {
	t.Helper()
	if v.Verdict != want {
		t.Fatalf("%q: want %s, got %s (%s)", v.Text, want, v.Verdict, v.Reason)
	}
}

// A weak model reports a test count it never saw. The output counted 12
// tests; the claim of 15 is contradicted, and the conflict is shown.
func TestInventedNumberIsContradicted(t *testing.T) {
	b := newRec(t)
	b.call("c1", "bash", map[string]string{"command": "go test ./..."}, "ok  \texample.com/pkg\t0.21s\n12 tests passed\n", exit(0))
	for _, text := range []string{"All 15 tests pass.", "I ran the suite and 15 tests passed."} {
		v := checkText(t, b, "", text)
		if len(v) != 1 {
			t.Fatalf("want one claim, got %+v", v)
		}
		wantVerdict(t, v[0], ClaimContradicted)
		if v[0].Conflict == nil || !strings.Contains(v[0].Conflict.Excerpt, "12 tests") {
			t.Fatalf("the conflicting evidence is not shown: %+v", v[0].Conflict)
		}
	}
	// Cited, the same invention fares no better.
	v := checkCited(t, b, "", Claim{Text: "15 tests passed", Kind: KindObserved, Evidence: []string{"c1"}})
	wantVerdict(t, v[0], ClaimContradicted)
	// And the true figure is verified, tied to the result.
	v = checkText(t, b, "", "All 12 tests pass.")
	wantVerdict(t, v[0], ClaimVerified)
	if len(v[0].Evidence) == 0 {
		t.Fatal("a verified claim names its evidence")
	}
}

// A number that appears nowhere in what was observed is unverified, not verified.
func TestNumberNotObservedIsUnverified(t *testing.T) {
	b := newRec(t)
	b.call("c1", "read", map[string]string{"path": "/w/app.yaml"}, "name: api\nreplicas: 3\n", nil)
	v := checkText(t, b, "", "The api deployment runs with a memory limit of 512 MB.")
	wantVerdict(t, v[0], ClaimUnverified)
	v = checkText(t, b, "", "The config sets replicas: 4.")
	wantVerdict(t, v[0], ClaimContradicted)
	v = checkText(t, b, "", "The config sets 3 replicas.")
	wantVerdict(t, v[0], ClaimVerified)
}

// Uncited, a bare number found somewhere is not evidence, and a claim whose
// pieces sit in different results is not verified by any one of them.
func TestUncitedNeedsOneResultThatShowsIt(t *testing.T) {
	b := newRec(t)
	b.call("c1", "bash", map[string]string{"command": "ls"}, "a.txt\nb.txt\n3\n", exit(0))
	b.call("c2", "read", map[string]string{"path": "/w/hosts"}, "db.internal.example.com\n", nil)
	b.call("c3", "read", map[string]string{"path": "/w/ports"}, "5432\n", nil)
	v := checkText(t, b, "", "The cluster has 3 nodes.")[0]
	wantVerdict(t, v, ClaimUnverified)
	v = checkText(t, b, "", "The database is db.internal.example.com on port 5432.")[0]
	wantVerdict(t, v, ClaimUnverified)
	if !strings.Contains(v.Reason, "no one result shows it") {
		t.Fatalf("reason: %s", v.Reason)
	}
	// Cited, the two results together are what the agent says shows it.
	v = checkCited(t, b, "", Claim{Text: "The database is db.internal.example.com on port 5432.", Evidence: []string{"c2", "c3"}})[0]
	wantVerdict(t, v, ClaimVerified)
}

// "Tests pass" needs a command with exit 0 after the last change.
func TestSuccessNeedsExitZeroAfterLastEdit(t *testing.T) {
	b := newRec(t)
	b.call("c1", "bash", map[string]string{"command": "go test ./..."}, "ok", exit(0))
	wantVerdict(t, checkText(t, b, "", "The tests pass now.")[0], ClaimVerified)

	b.call("c2", "edit", map[string]string{"path": "/w/a.go", "old_string": "a", "new_string": "b"}, "Edited /w/a.go", nil)
	v := checkText(t, b, "", "The tests pass now.")[0]
	wantVerdict(t, v, ClaimUnverified)
	if !strings.Contains(v.Reason, "after the last change") {
		t.Fatalf("reason: %s", v.Reason)
	}
	// Cited, the earlier pass is still not current.
	wantVerdict(t, checkCited(t, b, "", Claim{Text: "tests pass", Evidence: []string{"c1"}})[0], ClaimUnverified)

	b.call("c3", "bash", map[string]string{"command": "go test ./..."}, "--- FAIL: TestA\nFAIL", exit(1))
	v = checkText(t, b, "", "The tests pass now.")[0]
	wantVerdict(t, v, ClaimContradicted)
	if v.Conflict == nil || v.Conflict.Seq == 0 {
		t.Fatalf("the failing run is the conflict: %+v", v.Conflict)
	}
	// A claim that they fail is not a success claim.
	if f := extractFacts("The tests do not pass."); f.success {
		t.Fatal("a negated claim was read as success")
	}
	if f := extractFacts("Build finished with no errors."); !f.success {
		t.Fatal("'no errors' is a success claim")
	}
}

// A command that only reads does not count as a change; one that writes does.
func TestInspectionCommands(t *testing.T) {
	for _, c := range []string{"go test ./...", "git status && git diff", "ls -la | wc -l", "cat a.txt 2>/dev/null",
		"grep -rn foo .", "npm test", "python -m pytest -q", "cd sub && make test"} {
		if !inspectionCommand(c) {
			t.Errorf("%q reads only", c)
		}
	}
	for _, c := range []string{"rm -rf x", "echo hi > a.txt", "sed -i s/a/b/ f", "git commit -m x", "go mod tidy",
		"find . -delete", "curl -o x https://h", "npm install", "cat $(whoami)", "tee out.txt"} {
		if inspectionCommand(c) {
			t.Errorf("%q may write", c)
		}
	}
}

// A cited event must exist, be a tool result, and still be current.
func TestCitations(t *testing.T) {
	b := newRec(t)
	b.r.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "what port?"})
	seq := b.call("c1", "read", map[string]string{"path": "/w/a.conf"}, "listen 8080\n", nil)
	wantVerdict(t, checkCited(t, b, "", Claim{Text: "it listens on 8080", Evidence: []string{"c1"}})[0], ClaimVerified)
	wantVerdict(t, checkCited(t, b, "", Claim{Text: "it listens on 8080", Evidence: []string{"#" + itoa(seq)}})[0], ClaimVerified)

	v := checkCited(t, b, "", Claim{Text: "it listens on 8080", Evidence: []string{"call_made_up"}})[0]
	wantVerdict(t, v, ClaimUnverified)
	if !strings.Contains(v.Reason, "not in this session's record") {
		t.Fatalf("reason: %s", v.Reason)
	}
	v = checkCited(t, b, "", Claim{Text: "it listens on 8080", Evidence: []string{"#1"}})[0]
	wantVerdict(t, v, ClaimUnverified)
	if !strings.Contains(v.Reason, "user.message") {
		t.Fatalf("reason: %s", v.Reason)
	}
	// A quote the output does not hold is not evidence.
	v = checkCited(t, b, "", Claim{Text: "it listens on 8080", Evidence: []string{"c1"}, Quote: "listen 0.0.0.0:8080"})[0]
	wantVerdict(t, v, ClaimUnverified)
	// The file changed after it was read: the read no longer shows the present.
	b.call("c2", "write", map[string]string{"path": "/w/a.conf", "content": "listen 9090\n"}, "Wrote /w/a.conf", nil)
	v = checkCited(t, b, "", Claim{Text: "it listens on 8080", Evidence: []string{"c1"}})[0]
	wantVerdict(t, v, ClaimUnverified)
	if !strings.Contains(v.Reason, "predates a later change") {
		t.Fatalf("reason: %s", v.Reason)
	}
	// An edit of another file leaves a read current.
	b2 := newRec(t)
	b2.call("c1", "read", map[string]string{"path": "/w/a.conf"}, "listen 8080\n", nil)
	b2.call("c2", "edit", map[string]string{"path": "/w/b.conf"}, "Edited", nil)
	wantVerdict(t, checkCited(t, b2, "", Claim{Text: "it listens on 8080", Evidence: []string{"c1"}})[0], ClaimVerified)
}

// file:line cites are checked against the file as it is now.
func TestFileLineCites(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Serve() {}\n"), 0o644)
	b := newRec(t)
	wantVerdict(t, checkText(t, b, dir, "The handler is `func Serve` at main.go:3.")[0], ClaimVerified)
	wantVerdict(t, checkText(t, b, dir, "The handler is `func Listen` at main.go:3.")[0], ClaimUnverified)
	v := checkText(t, b, dir, "The bug is at main.go:40.")[0]
	wantVerdict(t, v, ClaimContradicted)
	wantVerdict(t, checkText(t, b, dir, "The handler is `func Serve` at missing.go:3.")[0], ClaimContradicted)
}

// Saying a file was changed, or a command run, when the record holds no such
// call is contradicted.
func TestActionsAgainstTheRecord(t *testing.T) {
	b := newRec(t)
	b.call("c1", "edit", map[string]string{"path": "/w/config.yaml"}, "Edited /w/config.yaml", nil)
	wantVerdict(t, checkText(t, b, "", "I updated config.yaml to raise the limit.")[0], ClaimVerified)
	wantVerdict(t, checkText(t, b, "", "I also updated deploy.yaml to match.")[0], ClaimContradicted)
	wantVerdict(t, checkText(t, b, "", "I ran `make lint` first.")[0], ClaimContradicted)
	b.call("c2", "bash", map[string]string{"command": "make lint"}, "", exit(0))
	wantVerdict(t, checkText(t, b, "", "I ran `make lint` first.")[0], ClaimVerified)
}

// General knowledge and sentences with nothing checkable are unverified, and
// are not load-bearing; a "knowledge" claim about the session's work is
// checked as an observation.
func TestKnowledgeAndStatements(t *testing.T) {
	b := newRec(t)
	v := checkText(t, b, "", "Go has goroutines and channels for concurrency.")[0]
	wantVerdict(t, v, ClaimUnverified)
	if v.LoadBearing || v.Kind != KindStatement {
		t.Fatalf("a plain statement is not load-bearing: %+v", v)
	}
	v = checkCited(t, b, "", Claim{Text: "TLS 1.3 removed RSA key exchange", Kind: KindKnowledge})[0]
	wantVerdict(t, v, ClaimUnverified)
	if v.LoadBearing {
		t.Fatal("knowledge is not load-bearing")
	}
	v = checkCited(t, b, "", Claim{Text: "all tests pass", Kind: KindKnowledge})[0]
	if !v.LoadBearing || v.Verdict == ClaimVerified {
		t.Fatalf("a claim about the session's tests is checked whatever its kind: %+v", v)
	}
}

func TestSplitClaims(t *testing.T) {
	text := "Here is what I found:\n\n- The service listens on port 8080.\n- 3 replicas are configured. They use v1.2.8.\n\n```\nnot a claim 42\n```\nRun `go test ./...` to confirm. I'll update the docs next.\nWould you like me to change it?\nLet me know if you need more."
	got := splitClaims(text)
	want := []string{"The service listens on port 8080.", "3 replicas are configured.", "They use v1.2.8."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
	f := extractFacts("Deployed to api.internal.example.com at 10.0.0.5:22 on 2026-10-19 from deploy/app.yaml.")
	for _, lit := range []string{"api.internal.example.com", "10.0.0.5:22", "2026-10-19", "deploy/app.yaml"} {
		if !contains(f.literals, lit) {
			t.Errorf("%q not taken from the claim: %q", lit, f.literals)
		}
	}
	if len(f.numbers) != 0 {
		t.Errorf("the digits of a host, a date or a path are not numbers: %q", f.numbers)
	}
	if g := extractFacts("It handles 1,204 requests and pass/fail cases."); !contains(g.numbers, "1204") || len(g.paths) != 0 {
		t.Errorf("numbers %q paths %q", g.numbers, g.paths)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// verifyHarness is the loop harness with a verifier installed.
func verifyHarness(t *testing.T, mode VerifyMode, tool bool, turns []scriptedTurn) (*Loop, *MemStore, *scriptedAdapter, string) {
	t.Helper()
	l, store, dir := harness(t, turns, policy.ModeBypass, true)
	(&Verifier{Mode: mode, AnswerTool: tool}).Install(l)
	return l, store, l.Adapter.(*scriptedAdapter), dir
}

func answerVerdicts(store *MemStore) []AnswerVerdict {
	evs, _ := store.Events("sess1")
	var out []AnswerVerdict
	for _, e := range evs {
		if e.Type == EvAnswerVerdict {
			var v AnswerVerdict
			_ = json.Unmarshal(e.Payload, &v)
			out = append(out, v)
		}
	}
	return out
}

// Label mode checks the answer and labels it, and ends the run as it would
// have. It spends a model call only on a contradiction: the one revision.
func TestLabelMode(t *testing.T) {
	l, store, ad, _ := verifyHarness(t, VerifyLabel, false, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "printf '12 tests passed\\n'"})}},
		{text: "The suite ran 12 tests. The cluster has 9 nodes. Go is a compiled language."},
	})
	reason, err := l.Run(context.Background(), "run the tests")
	if err != nil || reason != TermCompleted || len(ad.gotRequests) != 2 {
		t.Fatalf("an answer with nothing contradicted costs no call: %s %v %d", reason, err, len(ad.gotRequests))
	}
	vs := answerVerdicts(store)
	if len(vs) != 1 || vs[0].Verified != 1 || vs[0].Unverified != 2 || vs[0].Revision != "" {
		t.Fatalf("verdict: %+v", vs)
	}
	if got := vs[0].Footer(); got != "1 verified · 2 unverified (1 general knowledge) · 0 contradicted" {
		t.Fatalf("footer: %s", got)
	}

	// A contradiction gets the one revision; still contradicted, the run
	// completes, with the claim labelled.
	l, store, ad, _ = verifyHarness(t, VerifyLabel, false, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "printf '12 tests passed\\n'"})}},
		{text: "All 15 tests pass."},
		{text: "Yes: all 15 tests pass."},
	})
	if reason, _ := l.Run(context.Background(), "run the tests"); reason != TermCompleted || len(ad.gotRequests) != 3 {
		t.Fatalf("label mode ends completed after one revision: %s %d", reason, len(ad.gotRequests))
	}
	vs = answerVerdicts(store)
	if len(vs) != 2 || vs[0].Revision == "" || vs[1].Revision != "" || vs[1].Contradicted != 1 {
		t.Fatalf("verdicts: %+v", vs)
	}
	if l.LastVerdict() == nil || l.LastVerdict().Contradicted != 1 {
		t.Fatal("the loop keeps its last verdict")
	}
}

// Strict mode gives one recorded revision turn; a corrected answer completes.
func TestStrictRevisesOnceThenCompletes(t *testing.T) {
	l, store, ad, _ := verifyHarness(t, VerifyStrict, false, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "printf '12 tests passed\\n'"})}},
		{text: "All 15 tests pass."},
		{text: "12 tests passed."},
	})
	reason, _ := l.Run(context.Background(), "run the tests")
	if reason != TermCompleted {
		t.Fatalf("want completed after the revision, got %s", reason)
	}
	vs := answerVerdicts(store)
	if len(vs) != 2 || vs[0].Revision == "" || vs[1].Revision != "" || vs[1].Outcome != ClaimVerified {
		t.Fatalf("verdicts: %+v", vs)
	}
	last := ad.gotRequests[len(ad.gotRequests)-1].Messages
	if note := last[len(last)-1]; note.Role != model.RoleUser || !strings.Contains(note.Content, "15 tests") {
		t.Fatalf("the model was not told what failed: %+v", note)
	}
	// The note is in the record, so a rebuilt conversation has it too.
	msgs, err := Fork(func() []Event { e, _ := store.Events("sess1"); return e }(), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		found = found || (m.Role == model.RoleUser && strings.Contains(m.Content, "This is your one revision"))
	}
	if !found {
		t.Fatal("a rebuilt conversation lost the revision note")
	}
}

// Strict mode ends unverified, with its own exit code, when the revision
// does not fix it; there is only one revision.
func TestStrictEndsUnverified(t *testing.T) {
	l, store, ad, _ := verifyHarness(t, VerifyStrict, false, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "printf '12 tests passed\\n'"})}},
		{text: "All 15 tests pass."},
		{text: "Yes, all 15 tests pass."},
	})
	reason, _ := l.Run(context.Background(), "run the tests")
	if reason != TermUnverified || reason.ExitCode() != 8 {
		t.Fatalf("want unverified (exit 8), got %s (%d)", reason, reason.ExitCode())
	}
	if len(ad.gotRequests) != 3 {
		t.Fatalf("one revision only: %d calls", len(ad.gotRequests))
	}
	evs, _ := store.Events("sess1")
	end, _ := LastEnd(evs)
	if end.Reason != TermUnverified {
		t.Fatalf("recorded end: %s", end.Reason)
	}
	if vs := answerVerdicts(store); len(vs) != 2 || vs[1].Outcome != ClaimUnverified {
		t.Fatalf("verdicts: %+v", vs)
	}
}

// Through the answer tool, the agent's citations are checked, its text is
// recorded as its message, and the run ends without another turn.
func TestAnswerTool(t *testing.T) {
	l, store, ad, _ := verifyHarness(t, VerifyLabel, true, []scriptedTurn{
		{calls: []model.ToolCall{{ID: "t1", Name: "bash", Args: json.RawMessage(`{"command":"printf 'replicas: 3\\n'"}`)}}},
		{calls: []model.ToolCall{{ID: "a1", Name: "answer", Args: json.RawMessage(
			`{"text":"There are 3 replicas, and 7 pods.","claims":[{"text":"There are 3 replicas","kind":"observed","evidence":["t1"],"quote":"replicas: 3"},{"text":"there are 7 pods","evidence":"t1"}]}`)}}},
	})
	if !strings.Contains(l.Config.SystemPrompt, "`answer`") {
		t.Fatal("the prompt does not say how to answer")
	}
	reason, err := l.Run(context.Background(), "how many replicas?")
	if err != nil || reason != TermCompleted || len(ad.gotRequests) != 2 {
		t.Fatalf("%s %v %d", reason, err, len(ad.gotRequests))
	}
	vs := answerVerdicts(store)
	if len(vs) != 1 || vs[0].Source != "answer_tool" || vs[0].Verified != 1 || vs[0].Unverified != 1 {
		t.Fatalf("verdict: %+v", vs)
	}
	msgs := l.Messages()
	if last := msgs[len(msgs)-1]; last.Role != model.RoleAssistant || last.Content != "There are 3 replicas, and 7 pods." {
		t.Fatalf("the answer's text is the agent's last message: %+v", last)
	}
	evs, _ := store.Events("sess1")
	if !hasEvent(evs, EvAgentMessage) {
		t.Fatal("the answer's text is not recorded as the agent's message")
	}
}

// The contract's required command runs after the last change, by the
// harness; a failing one ends the run unverified after one revision.
func TestContractRequire(t *testing.T) {
	l, store, _, dir := verifyHarness(t, VerifyOff, false, nil)
	l.Adapter.(*scriptedAdapter).turns = []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": filepath.Join(dir, "new.txt"), "content": "x\n"})}},
		{text: "Done."},
		{text: "Done, really."},
	}
	l.Verify = &Verifier{Mode: VerifyOff, Contract: &Contract{Require: []string{"test -f ok.flag"}}}
	reason, _ := l.Run(context.Background(), "make the change")
	if reason != TermUnverified {
		t.Fatalf("a failing required command ends unverified, got %s", reason)
	}
	vs := answerVerdicts(store)
	if len(vs) != 2 || vs[0].Contract == nil || vs[0].Contract.OK || !strings.Contains(vs[0].Revision, "test -f ok.flag") {
		t.Fatalf("verdicts: %+v", vs)
	}
	evs, _ := store.Events("sess1")
	ran := 0
	for _, e := range evs {
		if e.Type == EvActionRequested && strings.Contains(string(e.Payload), ContractVia) {
			ran++
			if e.Actor != ActorSystem {
				t.Fatal("the harness runs the contract, not the agent")
			}
		}
	}
	// Run after the change and again for the revision, nothing having
	// passed in between.
	if ran != 2 {
		t.Fatalf("the required command ran %d times, want 2", ran)
	}

	// Once it passes, the run completes, and a pass with no change since is reused.
	os.WriteFile(filepath.Join(dir, "ok.flag"), nil, 0o644)
	l.Adapter.(*scriptedAdapter).turns = append(l.Adapter.(*scriptedAdapter).turns, scriptedTurn{text: "Done."}, scriptedTurn{text: "Still done."})
	if reason, _ := l.Run(context.Background(), "check"); reason != TermCompleted {
		t.Fatalf("a passing contract completes, got %s", reason)
	}
	if reason, _ := l.Run(context.Background(), "check again"); reason != TermCompleted {
		t.Fatalf("got %s", reason)
	}
	vs = answerVerdicts(store)
	if r := vs[len(vs)-1].Contract.Require[0]; !r.OK || !r.Reused {
		t.Fatalf("a pass with no change since is reused: %+v", r)
	}
}

// Changing a protected file marks the run unverified.
func TestContractProtect(t *testing.T) {
	l, store, _, dir := verifyHarness(t, VerifyOff, false, nil)
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "a_test.go"), []byte("package pkg\n"), 0o644)
	ad := l.Adapter.(*scriptedAdapter)
	ad.turns = []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "echo '// skip' >> pkg/a_test.go"})}},
		{text: "Fixed."}, {text: "Fixed."},
	}
	l.Verify = &Verifier{Mode: VerifyLabel, Contract: &Contract{Protect: []string{"*_test.go"}}}
	reason, _ := l.Run(context.Background(), "fix it")
	if reason != TermUnverified {
		t.Fatalf("got %s", reason)
	}
	vs := answerVerdicts(store)
	if c := vs[len(vs)-1].Contract; c == nil || len(c.Protected) != 1 || c.Protected[0] != "pkg/a_test.go" {
		t.Fatalf("contract: %+v", c)
	}
	for g, p := range map[string]string{"*_test.go": "a/b/c_test.go", ".github/**": ".github/workflows/ci.yml", "tests/": "tests/x/y.py", "/Makefile": "Makefile"} {
		if !globRegexp(g).MatchString(p) {
			t.Errorf("%s should match %s", g, p)
		}
	}
	if globRegexp("/Makefile").MatchString("sub/Makefile") || globRegexp("src/*.go").MatchString("src/a/b.go") {
		t.Error("an anchored glob matched below its folder")
	}
}

// The critic can only take a verdict down, and fails closed.
func TestCriticOnlyDowngrades(t *testing.T) {
	run := func(reply string, fail bool) AnswerVerdict {
		l, store, _, _ := verifyHarness(t, VerifyLabel, false, []scriptedTurn{
			{calls: []model.ToolCall{call("bash", map[string]string{"command": "printf 'replicas: 3\\n'"})}},
			{text: "There are 3 replicas. There are 9 nodes."},
		})
		critic := &scriptedAdapter{turns: []scriptedTurn{{text: reply}}}
		if fail {
			critic.turns = nil
			l.Verify.Critic = &Critic{Adapter: errAdapter{}}
		} else {
			l.Verify.Critic = &Critic{Adapter: critic}
		}
		l.Run(context.Background(), "how many?")
		return answerVerdicts(store)[0] // a contradiction is followed by a revision's verdict
	}
	// It says both are supported: the unverified one stays unverified.
	v := run(`{"verdicts":[{"n":1,"verdict":"supported"},{"n":2,"verdict":"supported"}]}`, false)
	if v.Verified != 1 || v.Unverified != 1 || v.Critic != CriticChecked {
		t.Fatalf("the critic raised a verdict: %+v", v)
	}
	v = run(`{"verdicts":[{"n":1,"verdict":"contradicted","why":"the output says 4"}]}`, false)
	if v.Verified != 0 || v.Contradicted != 1 {
		t.Fatalf("the critic's downgrade was not applied: %+v", v)
	}
	v = run("I think they are fine.", false)
	if v.Verified != 0 || v.Critic != CriticUnavailable {
		t.Fatalf("an unreadable critic leaves nothing verified: %+v", v)
	}
	v = run("", true)
	if v.Verified != 0 || v.Critic != CriticUnavailable {
		t.Fatalf("a failed critic leaves nothing verified: %+v", v)
	}
}

type errAdapter struct{}

func (errAdapter) Name() string                           { return "err" }
func (errAdapter) Profile() model.Profile                 { return model.Profile{} }
func (errAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (errAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	return nil, os.ErrDeadlineExceeded
}

func TestVerifyModes(t *testing.T) {
	for in, want := range map[string]VerifyMode{"": VerifyOff, "OFF": VerifyOff, "label": VerifyLabel, " strict ": VerifyStrict} {
		if got, err := ParseVerifyMode(in); err != nil || got != want {
			t.Errorf("%q: %s %v", in, got, err)
		}
	}
	if _, err := ParseVerifyMode("on"); err == nil {
		t.Error("an unknown mode is refused")
	}
	if VerifyLabel.Tighter(VerifyStrict) != VerifyStrict || VerifyStrict.Tighter(VerifyOff) != VerifyStrict || !VerifyOff.Looser(VerifyLabel) {
		t.Error("ordering")
	}
}

// A required command policy would not let run unasked is not run, and the
// contract fails closed: a destructive one still needs a person, a deny rule
// still binds, and plan mode runs nothing.
func TestContractRespectsPolicy(t *testing.T) {
	for name, set := range map[string]func(l *Loop){
		"destructive": func(l *Loop) { l.Verify.Contract.Require = []string{"rm -rf build"} },
		"deny rule": func(l *Loop) {
			r, err := policy.ParseRule("bash(make*)")
			if err != nil {
				t.Fatal(err)
			}
			l.Policy.Deny = append(l.Policy.Deny, r)
		},
		"plan mode": func(l *Loop) { l.Policy.Mode = policy.ModePlan },
	} {
		t.Run(name, func(t *testing.T) {
			l, store, _, _ := verifyHarness(t, VerifyOff, false, []scriptedTurn{{text: "Done."}, {text: "Done."}})
			l.Verify = &Verifier{Mode: VerifyOff, Contract: &Contract{Require: []string{"make check"}}}
			set(l)
			if reason, _ := l.Run(context.Background(), "go"); reason != TermUnverified {
				t.Fatalf("got %s", reason)
			}
			evs, _ := store.Events("sess1")
			for _, e := range evs {
				if e.Type == EvActionRequested && strings.Contains(string(e.Payload), ContractVia) {
					t.Fatalf("the harness ran a command policy did not let it run unasked: %s", e.Payload)
				}
			}
			vs := answerVerdicts(store)
			if r := vs[len(vs)-1].Contract.Require[0]; r.OK || r.Reason == "" {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

// A structured run delivers its result, not an answer: the answer tool is put
// away for it and back after.
func TestStructuredRunPutsTheAnswerToolAway(t *testing.T) {
	l, _, ad, _ := verifyHarness(t, VerifyStrict, true, []scriptedTurn{
		{calls: []model.ToolCall{{ID: "r1", Name: ResultToolName, Args: json.RawMessage(`{"n":1}`)}}},
	})
	got, reason, err := RunStructured(context.Background(), l, l.Tools, "count", json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`))
	if err != nil || reason != TermCompleted || string(got) != `{"n":1}` {
		t.Fatalf("%s %s %v", got, reason, err)
	}
	for _, d := range ad.gotRequests[0].Tools {
		if d.Name == AnswerToolName {
			t.Fatal("the answer tool was offered to a structured run")
		}
	}
	if _, ok := l.Tools.Get(AnswerToolName); !ok {
		t.Fatal("the answer tool did not come back")
	}
}

// A subagent's work is checked in its own record: its edit verifies "I
// updated", its test run verifies the count, and a parent's change after its
// return makes its output out of date.
func TestClaimsAboutASubagentsWork(t *testing.T) {
	store := NewMemStore()
	p, kid := NewRecorder(store, "sess1", ""), NewRecorder(store, "kid", "sess1")
	kid.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{"session": "kid"})
	for _, c := range []struct {
		id, tool, out string
		args          any
		exit          *int
	}{
		{"k1", "edit", "Edited /w/config.yaml", map[string]string{"path": "/w/config.yaml"}, nil},
		{"k2", "bash", "7 tests passed", map[string]string{"command": "go test ./..."}, exit(0)},
	} {
		raw, _ := json.Marshal(c.args)
		kid.Record(EvActionRequested, ActorAgent, Trusted, ActionRequested{CallID: c.id, Tool: c.tool, Args: raw})
		kid.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: c.id, Tool: c.tool, Content: c.out, ExitCode: c.exit})
	}
	p.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{"session": "kid"})
	p.Record(EvSubagentReturn, ActorAgent, Trusted, map[string]any{"session": "kid", "reason": "completed"})
	evs, _ := store.Events("sess1")
	check := func(child bool, text string) ClaimVerdict {
		env := checkEnv{events: evs}
		if child {
			env.child = store.Events
		}
		v, _ := checkClaims(env, textClaims(text))
		return v[0]
	}
	wantVerdict(t, check(true, "I updated config.yaml for the new limit."), ClaimVerified)
	wantVerdict(t, check(true, "The suite passes: 7 tests passed."), ClaimVerified)
	wantVerdict(t, check(true, "The suite passes: 9 tests passed."), ClaimContradicted)
	wantVerdict(t, check(false, "I updated config.yaml for the new limit."), ClaimUnverified)
	// The parent changes a file after the subagent returned: its test run is old.
	raw, _ := json.Marshal(map[string]string{"path": "/w/main.go"})
	p.Record(EvActionRequested, ActorAgent, Trusted, ActionRequested{CallID: "p1", Tool: "edit", Args: raw})
	p.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: "p1", Tool: "edit", Content: "Edited"})
	evs, _ = store.Events("sess1")
	wantVerdict(t, check(true, "The suite passes: 7 tests passed."), ClaimUnverified)
}

// A runbook-driven run's answer is checked like any other, a remote command
// it ran included; the contract's command, the operator's and not the
// plan's, runs after the last step without consuming one or being recorded
// as a plan step.
func TestVerifyUnderARunbook(t *testing.T) {
	r := runbookHarness(t, policy.ModeDefault, []tools.RunbookStep{sshStep("web1", "systemctl reload nginx")}, []scriptedTurn{
		{calls: []model.ToolCall{sshCall("a", "web1", "systemctl reload nginx")}},
		{text: "I ran `systemctl reload nginx` on web1."},
	})
	r.l.Verify = &Verifier{Mode: VerifyLabel, Contract: &Contract{Require: []string{"test -d ."}}}
	reason, err := r.l.Run(context.Background(), "go")
	if err != nil || reason != TermCompleted {
		t.Fatalf("%s %v", reason, err)
	}
	if len(r.appr.reasons) != 0 {
		t.Fatalf("asked %q: neither the step nor the contract should ask", r.appr.reasons)
	}
	evs, _ := r.store.Events("sess1")
	steps := 0
	for _, e := range evs {
		if e.Type == EvPlanStep {
			steps++
		}
	}
	if _, done, ok := r.l.BoundRunbook(); !ok || !done[0] || steps != 1 {
		t.Fatalf("runbook bound %v done %v, plan.step %d", ok, done, steps)
	}
	vs := answerVerdicts(r.store)
	if len(vs) != 1 || vs[0].Verified != 1 || vs[0].Contract == nil || !vs[0].Contract.OK || vs[0].Outcome != ClaimVerified {
		t.Fatalf("verdict %+v", vs)
	}
}
