package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// offloadLoop builds a loop whose history holds n tool results of size chars
// each, recorded the way invoke records them, under a window of the given size.
func offloadLoop(t *testing.T, n, size, window int) (*Loop, *MemStore) {
	t.Helper()
	store := NewMemStore()
	l := &Loop{
		Adapter:   &summarizerAdapter{window: window},
		Tools:     tools.NewRegistry(),
		Recorder:  NewRecorder(store, "s-off", ""),
		Offloader: NewOffloader(0.5),
	}
	l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: "find the bug"})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		body := fmt.Sprintf("RESULT-%d-START ", i) + strings.Repeat("x", size) + fmt.Sprintf(" RESULT-%d-END", i)
		l.messages = append(l.messages,
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: id, Name: "read", Args: json.RawMessage(fmt.Sprintf(`{"path":"f%d.go"}`, i))}}},
			model.Message{Role: model.RoleTool, ToolCallID: id, Content: body})
		if _, err := l.Recorder.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: id, Tool: "read", Content: body}); err != nil {
			t.Fatal(err)
		}
	}
	return l, store
}

func toolContents(l *Loop) []string {
	var out []string
	for _, m := range l.messages {
		if m.Role == model.RoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

// Below the threshold the window is left exactly as it is: rewriting history
// costs the prefix cache, and there is no reason to pay that early.
func TestOffloadLeavesAComfortableWindowAlone(t *testing.T) {
	l, store := offloadLoop(t, 8, 4000, 1_000_000)
	before := toolContents(l)
	l.offloadIfNeeded()
	for i, c := range toolContents(l) {
		if c != before[i] {
			t.Fatalf("result %d was rewritten under a nearly empty window", i)
		}
	}
	for _, e := range mustEvents(t, store) {
		if e.Type == EvContextOffloaded {
			t.Fatal("an offload was recorded that did not happen")
		}
	}
}

func TestOffloadStubsOldResultsAndKeepsRecentOnes(t *testing.T) {
	l, store := offloadLoop(t, 10, 4000, 12_000) // ~10k tokens used of 12k
	usedBefore, _ := l.contextSize()
	l.offloadIfNeeded()
	got := toolContents(l)

	for i := 0; i < 6; i++ {
		if !strings.HasPrefix(got[i], offloadMark) {
			t.Fatalf("old result %d was not offloaded", i)
		}
		if !strings.Contains(got[i], fmt.Sprintf(`"c%d"`, i)) || !strings.Contains(got[i], fmt.Sprintf("f%d.go", i)) {
			t.Fatalf("stub %d does not say what it held or how to get it back:\n%s", i, got[i])
		}
	}
	for i := 6; i < 10; i++ {
		if strings.HasPrefix(got[i], offloadMark) {
			t.Fatalf("recent result %d was offloaded; the last %d are the working state", i, l.Offloader.KeepRecent)
		}
	}
	usedAfter, _ := l.contextSize()
	if usedAfter >= usedBefore/2 {
		t.Fatalf("offloading freed too little: %d -> %d tokens", usedBefore, usedAfter)
	}

	var info Offloaded
	for _, e := range mustEvents(t, store) {
		if e.Type == EvContextOffloaded {
			_ = json.Unmarshal(e.Payload, &info)
		}
	}
	if info.Results != 6 || info.BeforeTokens != usedBefore || info.AfterTokens != usedAfter || len(info.CallIDs) != 6 {
		t.Fatalf("the offload was not recorded accurately: %+v", info)
	}
}

// A second pass finds nothing to do: a stub is never offloaded again, and
// small results are not worth a stub that costs as much as they do.
func TestOffloadIsIdempotentAndSkipsSmallResults(t *testing.T) {
	l, _ := offloadLoop(t, 10, 4000, 12_000)
	l.offloadIfNeeded()
	first := toolContents(l)
	l.Offloader.Threshold = 0.0001 // force a second pass to be considered
	l.offloadIfNeeded()
	for i, c := range toolContents(l) {
		if c != first[i] {
			t.Fatalf("result %d changed on a second pass", i)
		}
	}

	small, _ := offloadLoop(t, 10, 200, 600)
	small.offloadIfNeeded()
	for i, c := range toolContents(small) {
		if strings.HasPrefix(c, offloadMark) {
			t.Fatalf("a %d-char result (%d) was replaced by a stub of similar size", len(c), i)
		}
	}
}

// The point of the whole design: what left the window comes back whole.
func TestRecallReturnsAnOffloadedResultInFull(t *testing.T) {
	l, store := offloadLoop(t, 10, 4000, 12_000)
	l.offloadIfNeeded()
	if !strings.HasPrefix(toolContents(l)[2], offloadMark) {
		t.Fatal("setup: result 2 should have been offloaded")
	}

	r := Recall{Store: store, SessionID: "s-off"}
	got := r.Run(context.Background(), nil, json.RawMessage(`{"call_id":"c2"}`))
	if got.IsError {
		t.Fatalf("recall failed: %s", got.Content)
	}
	if !strings.Contains(got.Content, "RESULT-2-START") || !strings.Contains(got.Content, "RESULT-2-END") {
		t.Fatalf("recall did not return the whole result:\n%.200s", got.Content)
	}
}

func TestRecallPagesALongResult(t *testing.T) {
	store := NewMemStore()
	rec := NewRecorder(store, "s-page", "")
	body := "HEAD " + strings.Repeat("अभेद ", 3000) + " TAIL"
	if _, err := rec.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: "big", Tool: "read", Content: body}); err != nil {
		t.Fatal(err)
	}
	r := Recall{Store: store, SessionID: "s-page", MaxChars: 5000}

	first := r.Run(context.Background(), nil, json.RawMessage(`{"call_id":"big"}`))
	if !first.Truncated || !strings.Contains(first.Content, "Continue with offset 5000") {
		t.Fatalf("a long result was not paged:\n%.300s", first.Content[len(first.Content)-120:])
	}
	// An offset inside a multi-byte character must not produce broken text.
	for _, off := range []int{5000, 5001, 5002} {
		page := r.Run(context.Background(), nil, json.RawMessage(fmt.Sprintf(`{"call_id":"big","offset":%d}`, off)))
		if page.IsError || strings.ContainsRune(page.Content, '\uFFFD') {
			t.Fatalf("offset %d split a character", off)
		}
	}
	last := r.Run(context.Background(), nil, json.RawMessage(fmt.Sprintf(`{"call_id":"big","offset":%d}`, len(body)-20)))
	if !strings.Contains(last.Content, "TAIL") || last.Truncated {
		t.Fatalf("the final page is wrong: %q", last.Content)
	}
}

func TestRecallSearchesMessagesAndResults(t *testing.T) {
	store := NewMemStore()
	rec := NewRecorder(store, "s-find", "")
	_, _ = rec.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "The flaky one is TestRetryBackoff."})
	_, _ = rec.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: "k1", Tool: "bash", Content: "--- FAIL: TestRetryBackoff (0.31s)\n  want 3s got 1s"})
	_, _ = rec.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: "k2", Tool: "read", Content: "unrelated"})

	r := Recall{Store: store, SessionID: "s-find"}
	got := r.Run(context.Background(), nil, json.RawMessage(`{"query":"testretrybackoff"}`)).Content
	for _, want := range []string{"2 match(es)", "#1 user", "call_id k1", "want 3s got 1s"} {
		if !strings.Contains(got, want) {
			t.Errorf("search result is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "k2") {
		t.Error("search returned a result that does not match")
	}
	if miss := r.Run(context.Background(), nil, json.RawMessage(`{"query":"nothing like this"}`)); miss.IsError {
		t.Error("finding nothing is an answer, not an error")
	}
}

// The session is fixed when the tool is built. Nothing the model passes can
// point it at another session's record.
func TestRecallCannotReachAnotherSession(t *testing.T) {
	store := NewMemStore()
	_, _ = NewRecorder(store, "s-theirs", "").Record(EvObservation, ActorTool, Untrusted,
		Observation{CallID: "secret", Tool: "read", Content: "their credentials"})
	_, _ = NewRecorder(store, "s-mine", "").Record(EvUserMessage, ActorUser, Trusted, Message{Text: "hello"})

	r := Recall{Store: store, SessionID: "s-mine"}
	for _, args := range []string{
		`{"call_id":"secret"}`,
		`{"query":"credentials"}`,
		`{"call_id":"secret","session_id":"s-theirs"}`,
		`{"query":"credentials","session":"s-theirs"}`,
	} {
		if got := r.Run(context.Background(), nil, json.RawMessage(args)); strings.Contains(got.Content, "their credentials") {
			t.Fatalf("recall read another session with %s", args)
		}
	}
}

func mustEvents(t *testing.T, s *MemStore) []Event {
	t.Helper()
	var all []Event
	for _, id := range []string{"s-off"} {
		evs, err := s.Events(id)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	return all
}

// A server shares one registry across every session. recall is bound to one
// session's record, so it must never land on the shared copy.
func TestRecallIsPerLoopNotOnTheSharedRegistry(t *testing.T) {
	shared := tools.NewRegistry()
	store := NewMemStore()
	a := NewLoop(&summarizerAdapter{window: 1000}, shared, nil, nil, nil, NewRecorder(store, "s-a", ""), DefaultConfig())
	b := NewLoop(&summarizerAdapter{window: 1000}, shared, nil, nil, nil, NewRecorder(store, "s-b", ""), DefaultConfig())

	if _, leaked := shared.Get("recall"); leaked {
		t.Fatal("recall was added to the shared registry: the next session would read this one's record")
	}
	for name, l := range map[string]*Loop{"s-a": a, "s-b": b} {
		tool, ok := l.Tools.Get("recall")
		if !ok {
			t.Fatalf("loop %s has no recall tool", name)
		}
		if got := tool.(Recall).SessionID; got != name {
			t.Fatalf("loop %s got a recall bound to %s", name, got)
		}
	}
	if a.Offloader == nil || a.Offloader.Threshold != 0.60 {
		t.Fatalf("default offloader = %+v, want threshold 0.60", a.Offloader)
	}
	cfg := DefaultConfig()
	cfg.OffloadAt = 0
	if off := NewLoop(&summarizerAdapter{window: 1000}, shared, nil, nil, nil, NewRecorder(store, "s-c", ""), cfg); off.Offloader != nil {
		t.Fatal("offload_at 0 must turn offloading off")
	}
}

// sizedAdapter is the scripted model with a real window and a token count,
// so the loop's own threshold check is what triggers the offload.
type sizedAdapter struct {
	*scriptedAdapter
	window int
}

func (s *sizedAdapter) Profile() model.Profile { return model.Profile{ContextWindow: s.window} }
func (s *sizedAdapter) CountTokens(req model.Request) (int, error) {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content)
	}
	return n / 4, nil
}

// End to end through Run: the agent reads seven large files under a small
// window, the early results leave the window, and the agent then recalls the
// first one and gets it back whole — without a compaction ever firing.
func TestRunOffloadsThenRecallsWithoutCompacting(t *testing.T) {
	dir := tempDir(t)
	var turns []scriptedTurn
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("f%d.txt", i)
		body := fmt.Sprintf("MARKER-%d\n", i) + strings.Repeat("line of source text\n", 250)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c := call("read", map[string]string{"path": filepath.Join(dir, name)})
		c.ID = fmt.Sprintf("read-%d", i)
		turns = append(turns, scriptedTurn{calls: []model.ToolCall{c}})
	}
	back := call("recall", map[string]string{"call_id": "read-0"})
	turns = append(turns, scriptedTurn{calls: []model.ToolCall{back}}, scriptedTurn{text: "done"})

	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	adapter := &sizedAdapter{scriptedAdapter: &scriptedAdapter{turns: turns}, window: 12_000}
	l := NewLoop(adapter, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	l.Compactor = NewCompactor(adapter, 0.90)

	if reason, err := l.Run(context.Background(), "read everything"); err != nil || reason != TermCompleted {
		t.Fatalf("run: %s %v", reason, err)
	}

	evs, _ := store.Events("sess1")
	var offloads, compactions int
	var recalled string
	for _, e := range evs {
		switch e.Type {
		case EvContextOffloaded:
			offloads++
		case EvCompactStarted:
			compactions++
		case EvObservation:
			var o Observation
			_ = json.Unmarshal(e.Payload, &o)
			if o.Tool == "recall" {
				recalled = o.Content
			}
		}
	}
	if offloads == 0 {
		t.Fatal("seven large reads under a 12k window never triggered an offload")
	}
	if compactions != 0 {
		t.Fatalf("compaction fired %d time(s); offloading should have kept the window clear of it", compactions)
	}
	if !strings.Contains(recalled, "MARKER-0") || strings.Count(recalled, "line of source text") < 200 {
		t.Fatalf("recall did not bring the first file back whole:\n%.160s", recalled)
	}

	// The last request the model saw: the early reads are stubs, the recent
	// ones are whole, and it is smaller than what the session actually read.
	last := adapter.gotRequests[len(adapter.gotRequests)-1]
	var stubs, whole, inWindow int
	for _, m := range last.Messages {
		inWindow += len(m.Content)
		if m.Role != model.RoleTool {
			continue
		}
		if strings.HasPrefix(m.Content, offloadMark) {
			stubs++
		} else {
			whole++
		}
	}
	var recorded int
	for _, e := range evs {
		if e.Type == EvObservation {
			var o Observation
			_ = json.Unmarshal(e.Payload, &o)
			recorded += len(o.Content)
		}
	}
	if stubs < 3 || whole != l.Offloader.KeepRecent {
		t.Fatalf("final window has %d stubs and %d whole results; want the old ones stubbed and exactly the last %d whole",
			stubs, whole, l.Offloader.KeepRecent)
	}
	if inWindow >= recorded {
		t.Fatalf("the window (%d chars) is no smaller than what was read (%d)", inWindow, recorded)
	}
}
