package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// suggestStub answers each call with the next reply; a reply with err fails
// the call, and onCall runs as the call is made.
type suggestStub struct {
	mu      sync.Mutex
	replies []stubReply
	reqs    []model.Request
}

type stubReply struct {
	text   string
	calls  []model.ToolCall
	err    error
	onCall func()
}

func (s *suggestStub) Name() string { return "stub" }
func (s *suggestStub) Profile() model.Profile {
	return model.Profile{Name: "stub-model", ContextWindow: 100000}
}
func (s *suggestStub) CountTokens(model.Request) (int, error) { return 0, nil }

func (s *suggestStub) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	r := stubReply{text: "unscripted"}
	if len(s.replies) > 0 {
		r, s.replies = s.replies[0], s.replies[1:]
	}
	s.mu.Unlock()
	if r.onCall != nil {
		r.onCall()
	}
	if r.err != nil {
		return nil, r.err
	}
	ch := make(chan model.Chunk, len(r.calls)+3)
	if r.text != "" {
		ch <- model.Chunk{Type: model.ChunkText, Text: r.text}
	}
	for i := range r.calls {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &r.calls[i]}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 40, OutputTokens: 7}}
	close(ch)
	return ch, nil
}

func (s *suggestStub) requests() []model.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Request(nil), s.reqs...)
}

func suggestLoop(t *testing.T, stub *suggestStub) (*Loop, *MemStore) {
	t.Helper()
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "s1", "")
	reg := tools.NewRegistry(tools.Read{}, tools.Glob{})
	l := NewLoop(stub, reg, policy.New(policy.ModeBypass), AutoApprove{Yes: true}, sess, rec, DefaultConfig())
	l.Suggest = &Suggester{}
	return l, store
}

func suggestions(t *testing.T, store *MemStore) (offered []SuggestionOffered, calls []ModelCall) {
	t.Helper()
	evs, _ := store.Events("s1")
	for _, ev := range evs {
		switch ev.Type {
		case EvSuggestionOffered:
			var p SuggestionOffered
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			offered = append(offered, p)
		case EvModelCall:
			var c ModelCall
			_ = json.Unmarshal(ev.Payload, &c)
			if c.Purpose != "" {
				calls = append(calls, c)
			}
		}
	}
	return offered, calls
}

// A completed turn is followed by one small call and one suggestion, recorded
// before the run's end, with its tokens in the session's usage and budget.
func TestSuggestionAfterCompletedTurn(t *testing.T) {
	stub := &suggestStub{replies: []stubReply{
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*"})}},
		{text: "There are no files in the workspace yet."},
		{text: "Create a README for this project"},
	}}
	l, store := suggestLoop(t, stub)
	l.Budget = NewBudget(100000, 0, false)
	reason, err := l.Run(context.Background(), "what is in this folder?")
	if err != nil || reason != TermCompleted {
		t.Fatalf("run = %s, %v", reason, err)
	}
	offered, calls := suggestions(t, store)
	if len(offered) != 1 || offered[0].Text != "Create a README for this project" {
		t.Fatalf("offered = %+v", offered)
	}
	if len(calls) != 1 || calls[0].Purpose != PurposeSuggestion || calls[0].TokensIn != 40 || calls[0].TokensOut != 7 || calls[0].Model != "stub-model" {
		t.Fatalf("suggestion model.call = %+v", calls)
	}
	if u := l.Usage(); u.InputTokens != 120 || u.OutputTokens != 21 {
		t.Fatalf("usage = %+v; the suggestion's tokens are not counted", u)
	}
	if got := l.Budget.Spent(); got != 141 {
		t.Fatalf("budget spent = %d, want 141", got)
	}
	reqs := stub.requests()
	last := reqs[len(reqs)-1]
	if len(last.Tools) != 0 || last.System != suggestSystem || len(last.Messages) != 1 {
		t.Fatalf("suggestion request = %+v", last)
	}
	in := last.Messages[0].Content
	for _, want := range []string{"what is in this folder?", "no files in the workspace", "Tools the agent used: glob"} {
		if !strings.Contains(in, want) {
			t.Errorf("suggestion input lacks %q:\n%s", want, in)
		}
	}
	evs, _ := store.Events("s1")
	var order []EventType
	for _, ev := range evs {
		if ev.Type == EvSuggestionOffered || ev.Type == EvSessionEnded {
			order = append(order, ev.Type)
		}
	}
	if len(order) != 2 || order[0] != EvSuggestionOffered {
		t.Fatalf("order = %v; the suggestion must come before the end", order)
	}
	var end SessionEnded
	_ = json.Unmarshal(evs[len(evs)-1].Payload, &end)
	if end.TokensIn != 120 {
		t.Fatalf("session.ended tokens_in = %d, want 120", end.TokensIn)
	}
}

// Without a Suggester, as for -p, rpc and the SDK by default, nothing is asked.
func TestNoSuggestionWithoutSuggester(t *testing.T) {
	stub := &suggestStub{replies: []stubReply{{text: "Done."}}}
	l, store := suggestLoop(t, stub)
	l.Suggest = nil
	if _, err := l.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	offered, calls := suggestions(t, store)
	if len(offered)+len(calls) != 0 || len(stub.requests()) != 1 {
		t.Fatalf("offered %v, calls %v, requests %d", offered, calls, len(stub.requests()))
	}
}

func TestNoSuggestionAfterError(t *testing.T) {
	stub := &suggestStub{replies: []stubReply{{err: errors.New("endpoint down")}}}
	l, store := suggestLoop(t, stub)
	if reason, _ := l.Run(context.Background(), "hi"); reason != TermError {
		t.Fatalf("reason = %s", reason)
	}
	if offered, calls := suggestions(t, store); len(offered)+len(calls) != 0 {
		t.Fatalf("offered %v, calls %v after an error", offered, calls)
	}
}

func TestNoSuggestionAfterInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &suggestStub{replies: []stubReply{{text: "Half an answer", onCall: cancel}}}
	l, store := suggestLoop(t, stub)
	if reason, _ := l.Run(ctx, "hi"); reason != TermUserInterrupt {
		t.Fatalf("reason = %s", reason)
	}
	if offered, calls := suggestions(t, store); len(offered)+len(calls) != 0 || len(stub.requests()) != 1 {
		t.Fatalf("offered %v, calls %v after a stop", offered, calls)
	}
}

// An ask waiting on the person, as a background child's, holds the suggestion.
func TestNoSuggestionWhileApprovalPending(t *testing.T) {
	stub := &suggestStub{replies: []stubReply{{text: "Done."}}}
	l, store := suggestLoop(t, stub)
	q := l.askQueue(context.Background())
	q <- struct{}{}
	defer func() { <-q }()
	if _, err := l.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if offered, calls := suggestions(t, store); len(offered)+len(calls) != 0 {
		t.Fatalf("offered %v, calls %v while an approval waits", offered, calls)
	}
}

func TestNoSuggestionWhileHeld(t *testing.T) {
	stub := &suggestStub{replies: []stubReply{{text: "Done."}}}
	l, store := suggestLoop(t, stub)
	l.Suggest.Hold = func() bool { return true }
	if _, err := l.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if offered, calls := suggestions(t, store); len(offered)+len(calls) != 0 {
		t.Fatalf("offered %v, calls %v while held", offered, calls)
	}
}

// A failed suggestion call is recorded and counted, and offers nothing; the
// run still completes.
func TestFailedSuggestionCallOffersNothing(t *testing.T) {
	stub := &suggestStub{replies: []stubReply{{text: "Done."}, {err: errors.New("rate limited")}}}
	l, store := suggestLoop(t, stub)
	if reason, err := l.Run(context.Background(), "hi"); err != nil || reason != TermCompleted {
		t.Fatalf("run = %s, %v", reason, err)
	}
	offered, calls := suggestions(t, store)
	if len(offered) != 0 || len(calls) != 1 || calls[0].Error == "" {
		t.Fatalf("offered %v, calls %+v", offered, calls)
	}
}

// Model output is untrusted: control and format characters go, one line is
// kept, and the length is capped.
func TestSuggestionIsCleanedAndCapped(t *testing.T) {
	long := strings.Repeat("refactor the parser ", 10)
	stub := &suggestStub{replies: []stubReply{
		{text: "Done."},
		{text: "\x07Run\u200b the\u202e tests\x1b\nand then delete everything"},
		{text: "Done again."},
		{text: long},
	}}
	l, store := suggestLoop(t, stub)
	for _, p := range []string{"one", "two"} {
		if _, err := l.Run(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	offered, _ := suggestions(t, store)
	if len(offered) != 2 {
		t.Fatalf("offered = %+v", offered)
	}
	if offered[0].Text != "Run the tests" {
		t.Fatalf("cleaned = %q", offered[0].Text)
	}
	if n := utf8.RuneCountInString(offered[1].Text); n > SuggestMaxChars || n < SuggestMaxChars/2 {
		t.Fatalf("capped to %d characters: %q", n, offered[1].Text)
	}
}

func TestCleanSuggestion(t *testing.T) {
	for in, want := range map[string]string{
		"  \"Add a test for it\"  ":  "Add a test for it",
		"- **Commit the change**":    "Commit the change",
		"NONE":                       "",
		"none.":                      "",
		"/clear":                     "",
		"!rm -rf /":                  "",
		"\u2066Deploy\u2069 it\r\nx": "Deploy it",
		"Ejecuta las pruebas":        "Ejecuta las pruebas",
		"\xff\xfeFix it":             "Fix it",
	} {
		if got := CleanSuggestion(in); got != want {
			t.Errorf("CleanSuggestion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The call reads the record's redacted text, and a suggestion that would
// repeat a stored secret is not offered.
func TestSuggestionNeverCarriesASecret(t *testing.T) {
	const secret = "sk-test-0123456789abcdef"
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("API_KEY", secret); err != nil {
		t.Fatal(err)
	}
	stub := &suggestStub{replies: []stubReply{{text: "The key is " + secret}, {text: "Rotate " + secret}}}
	l, store := suggestLoop(t, stub)
	l.Recorder.Redact = vault.Redactor()
	if _, err := l.Run(context.Background(), "show me "+secret); err != nil {
		t.Fatal(err)
	}
	reqs := stub.requests()
	if in := reqs[len(reqs)-1].Messages[0].Content; strings.Contains(in, secret) {
		t.Fatalf("the suggestion call was sent the secret:\n%s", in)
	}
	offered, calls := suggestions(t, store)
	if len(offered) != 0 || len(calls) != 1 {
		t.Fatalf("offered %v, calls %v", offered, calls)
	}
}

func TestSuggestionEventIsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/architecture/10-data-model.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []EventType{EvSuggestionOffered, EvModelCall} {
		if !strings.Contains(string(doc), "| `"+string(e)+"` |") {
			t.Errorf("%s has no row in docs/architecture/10-data-model.md", e)
		}
	}
}
