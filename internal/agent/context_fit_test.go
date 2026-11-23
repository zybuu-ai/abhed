package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// windowAdapter is a stub model with a window, a char/4 token count, and a
// script of replies for the conversation: each either a refusal or text.
// Summary requests (compaction) are answered apart and not scripted.
type windowAdapter struct {
	name      string
	window    int
	truncates bool
	script    []windowReply
	main      []model.Request // the conversation's requests, in order
	summaries int
	// failInStream sends a refusal as a stream error rather than from Complete.
	failInStream bool
}

type windowReply struct {
	err   error
	text  string
	calls []model.ToolCall
	usage *model.Usage
}

func (w *windowAdapter) Name() string { return w.name }
func (w *windowAdapter) Profile() model.Profile {
	return model.Profile{Name: w.name, ContextWindow: w.window, SilentTruncation: w.truncates}
}
func (w *windowAdapter) CountTokens(req model.Request) (int, error) {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content)
	}
	return n / 4, nil
}

func (w *windowAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 8)
	if strings.HasPrefix(req.System, "You summarize") {
		w.summaries++
		ch <- model.Chunk{Type: model.ChunkText, Text: "summary of the earlier work"}
		ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
		close(ch)
		return ch, nil
	}
	w.main = append(w.main, req)
	r := windowReply{text: "done"}
	if len(w.script) > 0 {
		r, w.script = w.script[0], w.script[1:]
	}
	if r.err != nil {
		if !w.failInStream {
			close(ch)
			return nil, r.err
		}
		ch <- model.Chunk{Type: model.ChunkError, Err: r.err}
		close(ch)
		return ch, nil
	}
	if r.text != "" {
		ch <- model.Chunk{Type: model.ChunkText, Text: r.text}
	}
	for i := range r.calls {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &r.calls[i]}
	}
	u := r.usage
	if u == nil {
		u = &model.Usage{InputTokens: 10}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: u}
	close(ch)
	return ch, nil
}

// fitLoop is a loop over a with a compactor and a long history already in
// it: older exchanges worth summarising, then the new request.
func fitLoop(t *testing.T, a model.Adapter) (*Loop, *MemStore) {
	t.Helper()
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	l := NewLoop(a, tools.NewRegistry(tools.Read{}, Recall{Store: store, SessionID: "sess1"}), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	l.Compactor = NewCompactor(a, 0.9)
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("k8s-%d", i)
		l.messages = append(l.messages,
			model.Message{Role: model.RoleUser, Content: fmt.Sprintf("look at the pods, round %d", i)},
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: id, Name: "read", Args: json.RawMessage(`{}`)}}},
			model.Message{Role: model.RoleTool, ToolCallID: id, Content: strings.Repeat("pod-x Running 0 5d\n", 300)},
			model.Message{Role: model.RoleAssistant, Content: "noted"})
	}
	return l, store
}

func fitEvents(store *MemStore, typ EventType) []Event {
	evs, _ := store.Events("sess1")
	var out []Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// Each provider's refusal of an over-long prompt is answered once: the window
// learned from it, the conversation compacted, and the turn asked again,
// which then completes. Without the recovery the first refusal ended the run.
func TestOverflowCompactsAndRetriesOnce(t *testing.T) {
	refusals := map[string]error{
		"anthropic": fmt.Errorf("model call failed: anthropic returned %w", &model.StatusError{Status: 400, Attempts: 1,
			Body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 9000 tokens > 8000 maximum"}}`}),
		"openai": fmt.Errorf("endpoint returned %w", &model.StatusError{Status: 400, Attempts: 1,
			Body: `{"error":{"message":"This model's maximum context length is 8000 tokens. However, your messages resulted in 9000 tokens.","code":"context_length_exceeded"}}`}),
		"gemini": fmt.Errorf("gemini returned %w", &model.StatusError{Status: 400, Attempts: 1,
			Body: `{"error":{"code":400,"message":"The input token count (9000) exceeds the maximum number of tokens allowed (8000).","status":"INVALID_ARGUMENT"}}`}),
		"ollama": fmt.Errorf("endpoint returned %w", &model.StatusError{Status: 400, Attempts: 1,
			Body: `{"error":{"message":"the input length exceeds the context length of 8000"}}`}),
	}
	for name, refusal := range refusals {
		for _, inStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", name, inStream), func(t *testing.T) {
				a := &windowAdapter{name: "m", window: 1_000_000, failInStream: inStream,
					script: []windowReply{{err: refusal}, {text: "the pods are fine"}}}
				l, store := fitLoop(t, a)
				reason, err := l.Run(context.Background(), "and now?")
				if err != nil || reason != TermCompleted {
					t.Fatalf("run: %s %v", reason, err)
				}
				if len(a.main) != 2 || a.summaries != 1 {
					t.Fatalf("%d conversation calls and %d summaries, want 2 and 1", len(a.main), a.summaries)
				}
				over := fitEvents(store, EvContextOverflow)
				if len(over) != 1 {
					t.Fatalf("%d context.overflow events", len(over))
				}
				var o ContextOverflow
				_ = json.Unmarshal(over[0].Payload, &o)
				if o.Action != OverflowRetried || o.Limit != 8000 || o.AfterTokens >= o.BeforeTokens {
					t.Fatalf("overflow %+v", o)
				}
				// The retry was smaller than the refused request.
				first, _ := a.CountTokens(a.main[0])
				second, _ := a.CountTokens(a.main[1])
				if second >= first {
					t.Fatalf("retry sent %d tokens, the refused request %d", second, first)
				}
			})
		}
	}
}

// A second refusal in a row ends the run, once, with what happened and what
// to do: never a loop of compactions.
func TestOverflowGivesUpAfterOneRetry(t *testing.T) {
	refusal := fmt.Errorf("anthropic returned %w", &model.StatusError{Status: 400, Attempts: 1,
		Body: `prompt is too long: 9000 tokens > 8000 maximum`})
	a := &windowAdapter{name: "claude-x", window: 1_000_000,
		script: []windowReply{{err: refusal}, {err: refusal}, {err: refusal}, {text: "never"}}}
	l, store := fitLoop(t, a)
	reason, err := l.Run(context.Background(), "and now?")
	if reason != TermError || err == nil {
		t.Fatalf("run: %s %v", reason, err)
	}
	var ce *ContextOverflowError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "/compact") || !strings.Contains(err.Error(), "/model") ||
		!strings.Contains(err.Error(), "8000 tokens") {
		t.Fatalf("error %v", err)
	}
	if len(a.main) != 2 {
		t.Fatalf("%d conversation calls, want 2: one refused, one retried", len(a.main))
	}
	var actions []string
	for _, e := range fitEvents(store, EvContextOverflow) {
		var o ContextOverflow
		_ = json.Unmarshal(e.Payload, &o)
		actions = append(actions, o.Action)
	}
	if strings.Join(actions, ",") != OverflowRetried+","+OverflowGaveUp {
		t.Fatalf("actions %v", actions)
	}
}

// Another 400 is not an overflow: it ends the turn as before, unretried.
func TestOtherRefusalIsNotRetried(t *testing.T) {
	a := &windowAdapter{name: "m", window: 1_000_000,
		script: []windowReply{{err: &model.StatusError{Status: 400, Body: "invalid x-api-key"}}}}
	l, store := fitLoop(t, a)
	if reason, err := l.Run(context.Background(), "go"); reason != TermError || err == nil {
		t.Fatalf("run: %s %v", reason, err)
	}
	if len(a.main) != 1 || len(fitEvents(store, EvContextOverflow)) != 0 {
		t.Fatalf("%d calls", len(a.main))
	}
}

// A tool result is capped at a quarter of the window before it enters the
// conversation; the cap says how to read the rest, and the record keeps all
// of it.
func TestLargeToolResultIsCapped(t *testing.T) {
	dir := tempDir(t)
	big := strings.Repeat("pod-abc-123   1/1   Running   0   12d\n", 4000) // ~150k chars
	path := dir + "/pods.txt"
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	c := call("read", map[string]string{"path": path})
	c.ID = "get-pods"
	a := &windowAdapter{name: "small", window: 32_768, script: []windowReply{{calls: []model.ToolCall{c}}, {text: "ok"}}}
	sess, _ := tools.NewSession(dir)
	store := NewMemStore()
	l := NewLoop(a, tools.NewRegistry(tools.Read{}, Recall{Store: store, SessionID: "sess1"}), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "list the pods"); err != nil {
		t.Fatal(err)
	}
	var result string
	for _, m := range l.messages {
		if m.Role == model.RoleTool && m.ToolCallID == "get-pods" {
			result = m.Content
		}
	}
	limit := 32_768 / 4 * 36 / 10
	if len(result) > limit+400 || len(result) < limit/2 {
		t.Fatalf("result in context is %d characters, cap %d", len(result), limit)
	}
	if !strings.Contains(result, `recall with call_id "get-pods"`) {
		t.Fatalf("the cap does not say how to read the rest:\n%s", result[limit/2-50:limit/2+300])
	}
	var o Observation
	for _, e := range fitEvents(store, EvObservation) {
		_ = json.Unmarshal(e.Payload, &o)
	}
	// The record keeps what the tool returned, before the cap.
	if len(o.Content) <= len(result) || strings.Contains(o.Content, "elided to fit the context window") {
		t.Fatalf("the record kept %d characters, the context %d", len(o.Content), len(result))
	}
	// A large window takes the result whole.
	if got := (&Loop{Adapter: &windowAdapter{window: 1_000_000}, Tools: tools.NewRegistry()}).fitResult("x", big); got != big {
		t.Fatal("a result within a large window's share was cut")
	}
}

// Switching to a model with a smaller window re-fits the conversation before
// the next call: results cut to the new share, history compacted, and the
// request the new model receives fits its window.
func TestModelSwitchRefits(t *testing.T) {
	big := &windowAdapter{name: "big", window: 1_000_000}
	l, store := fitLoop(t, big)
	small := &windowAdapter{name: "small", window: 8_000}
	before, _ := small.CountTokens(model.Request{Messages: l.messages})
	if before < 8_000*9/10 {
		t.Fatalf("fixture too small: %d", before)
	}
	if err := l.SwitchModel("small", small); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "carry on"); err != nil {
		t.Fatal(err)
	}
	if len(small.main) == 0 {
		t.Fatal("the new model was never asked")
	}
	sent, _ := small.CountTokens(small.main[0])
	if sent >= 8_000*9/10 {
		t.Fatalf("the first request to the small model is %d tokens of its 8,000", sent)
	}
	refit := fitEvents(store, EvContextRefit)
	if len(refit) != 1 {
		t.Fatalf("%d context.refit events", len(refit))
	}
	var r ContextRefit
	_ = json.Unmarshal(refit[0].Payload, &r)
	if r.Reason != "model_switch" || r.FromModel != "big" || r.ToModel != "small" || r.ToWindow != 8_000 ||
		r.AfterTokens >= r.BeforeTokens || (!r.Compacted && r.ResultsCapped == 0) {
		t.Fatalf("refit %+v", r)
	}
	// Ordered: the re-fit comes before the new model's call.
	evs, _ := store.Events("sess1")
	var refitAt, callAt int64
	for _, e := range evs {
		switch {
		case e.Type == EvContextRefit:
			refitAt = e.Seq
		case e.Type == EvModelCall && callAt == 0 && refitAt != 0:
			callAt = e.Seq
		}
	}
	if callAt == 0 || refitAt > callAt {
		t.Fatalf("refit at %d, next call at %d", refitAt, callAt)
	}
	// A switch to a larger window re-fits nothing.
	l2, store2 := fitLoop(t, small)
	if err := l2.SwitchModel("big", big); err != nil {
		t.Fatal(err)
	}
	if l2.pendingRefit != nil || len(fitEvents(store2, EvContextRefit)) != 0 {
		t.Fatal("a larger window was re-fitted")
	}
}

// An assumed window is said once, on the session's first turn.
func TestAssumedWindowIsSaid(t *testing.T) {
	a := &assumedAdapter{windowAdapter{name: "mystery", window: model.FallbackWindow}}
	l, store := fitLoop(t, a)
	for i := 0; i < 2; i++ {
		if _, err := l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
	}
	notes := fitEvents(store, EvContextWindow)
	if len(notes) != 1 {
		t.Fatalf("%d context.window events", len(notes))
	}
	var n ContextWindowNote
	_ = json.Unmarshal(notes[0].Payload, &n)
	if n.Source != model.WindowFallback || !strings.Contains(n.Note, "context_window") {
		t.Fatalf("%+v", n)
	}
}

type assumedAdapter struct{ windowAdapter }

func (a *assumedAdapter) Profile() model.Profile {
	p := a.windowAdapter.Profile()
	p.WindowSource = model.WindowFallback
	return p
}

// The compaction check counts tokens as the model reported them: a prompt
// the model counts at twice the estimate compacts at half the estimate.
func TestCalibratedCount(t *testing.T) {
	a := &windowAdapter{name: "m", window: 10_000}
	c := NewCompactor(a, 0.9)
	msgs := longMessages(10, 2_000) // 5,000 estimated
	if should, _, _ := c.ShouldCompactNow("", msgs, nil); should {
		t.Fatal("5,000 of 10,000 compacted before calibration")
	}
	c.SetScale(2)
	if should, used, _ := c.ShouldCompactNow("", msgs, nil); !should || used != 10_000 {
		t.Fatalf("calibrated: %v %d", should, used)
	}
	c.SetScale(0.5)
	if c.Scale() != 1 {
		t.Fatal("the scale never makes the count smaller")
	}
}

// A server that cuts an over-long prompt (Ollama) is caught by its reported
// prompt size: the window is learned and the conversation fitted before the
// next call.
func TestTruncatedPromptIsFitted(t *testing.T) {
	a := &windowAdapter{name: "local", window: 32_768, truncates: true,
		script: []windowReply{{text: "answer from a cut prompt", usage: &model.Usage{InputTokens: 4096}}}}
	l, store := fitLoop(t, a)
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	over := fitEvents(store, EvContextOverflow)
	if len(over) != 1 {
		t.Fatalf("%d context.overflow events", len(over))
	}
	var o ContextOverflow
	_ = json.Unmarshal(over[0].Payload, &o)
	if o.Action != OverflowTruncated || o.Read != 4096 {
		t.Fatalf("%+v", o)
	}
}

// Once a refusal has been compacted and retried, the next refusal gives up
// at once, however much there would be left to compact.
func TestOverflowAfterRetryNeverCompacts(t *testing.T) {
	a := &windowAdapter{name: "m", window: 1_000_000}
	l, store := fitLoop(t, a)
	l.overflowRetried = true
	before := len(l.messages)
	reason, done, err := l.overflowRecovery(context.Background(), errors.New("prompt is too long"), 0)
	if reason != TermError || !done || err == nil || a.summaries != 0 || len(l.messages) != before {
		t.Fatalf("%s %v %v; %d summaries, %d messages of %d", reason, done, err, a.summaries, len(l.messages), before)
	}
	if l.overflowRetried || len(fitEvents(store, EvCompactStarted)) != 0 {
		t.Fatal("gave up but compacted, or left the retry marked")
	}
}

// openAIStub is an OpenAI-compatible server that refuses, in Bedrock's
// words and naming no limit, any request body over limit bytes, and answers
// the rest with a short streamed reply.
func openAIStub(t *testing.T, limit int, sizes *[]int) *httptest.Server {
	return refusingStub(t, limit, sizes, `{"message":"Input is too long for requested model."}`)
}

// refusingStub is openAIStub refusing with the given body.
func refusingStub(t *testing.T, limit int, sizes *[]int, refusal string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*sizes = append(*sizes, len(body))
		if len(body) > limit {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(refusal))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"fine"},"finish_reason":null}]}`+"\n\n"+
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":2}}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// largeRecent adds eight recent reads of 36 KB each to a fitLoop history.
func largeRecent(l *Loop) {
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("big-%d", i)
		l.messages = append(l.messages,
			model.Message{Role: model.RoleUser, Content: fmt.Sprintf("read part %d", i)},
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: id, Name: "read", Args: json.RawMessage(`{}`)}}},
			model.Message{Role: model.RoleTool, ToolCallID: id, Content: strings.Repeat("node-7 Ready 3d v1.31\n", 1650)},
			model.Message{Role: model.RoleAssistant, Content: "read"})
	}
}

// A refusal that names no limit (Bedrock's "Input is too long") lowers the
// window below what was refused, so the one retry fits: before, the window
// stayed at the model's published 1M, the retry was refused again, and the
// run ended naming a window the provider never stated.
func TestOverflowWithNoNamedLimitLearnsBelowTheRefusal(t *testing.T) {
	var sizes []int
	srv := openAIStub(t, 200_000, &sizes)
	a, err := model.New(model.Spec{Type: "openai", Model: "gpt-4.1", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	l, store := fitLoop(t, a)
	largeRecent(l)
	if reason, err := l.Run(context.Background(), "and now?"); err != nil || reason != TermCompleted {
		t.Fatalf("run: %s %v; request sizes %v", reason, err, sizes)
	}
	p := a.Profile()
	if p.WindowSource != model.WindowLearned || p.ContextWindow >= 1_047_576 {
		t.Fatalf("window %d (%s) after an unnamed refusal", p.ContextWindow, p.WindowSource)
	}
	over := fitEvents(store, EvContextOverflow)
	var o ContextOverflow
	_ = json.Unmarshal(over[0].Payload, &o)
	if len(over) != 1 || o.Action != OverflowRetried || o.Limit != 0 || p.ContextWindow != o.Sent*4/5 {
		t.Fatalf("%d overflow events; %+v; window %d", len(over), o, p.ContextWindow)
	}
}

// When even that retry is refused, the run ends saying the provider named no
// limit and how large the refused request was, never a window it did not state.
func TestUnnamedOverflowGiveUpSaysWhatIsKnown(t *testing.T) {
	var sizes []int
	srv := openAIStub(t, 1_000, &sizes)
	a, err := model.New(model.Spec{Type: "openai", Model: "gpt-4.1", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := fitLoop(t, a)
	largeRecent(l)
	reason, err := l.Run(context.Background(), "and now?")
	if reason != TermError || err == nil {
		t.Fatalf("run: %s %v", reason, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "did not name its limit") || !strings.Contains(msg, "the refused request was about") ||
		strings.Contains(msg, "1047576") || strings.Contains(msg, "context window of") {
		t.Fatalf("message: %s", msg)
	}
}

// Calibration through a run: a provider that counts the prompt at twice the
// estimate makes the check after the tool results compact, where the
// estimate alone would not have.
func TestRunCalibratesTheCount(t *testing.T) {
	c := call("read", map[string]string{"path": "/nonexistent-ctx-test"})
	a := &windowAdapter{name: "m", window: 10_000, script: []windowReply{
		{calls: []model.ToolCall{c}, usage: &model.Usage{InputTokens: 10_000}},
		{text: "done"},
	}}
	sess, _ := tools.NewSession(tempDir(t))
	store := NewMemStore()
	l := NewLoop(a, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	l.Compactor = NewCompactor(a, 0.9)
	l.messages = longMessages(10, 2_000) // about 5,000 estimated
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(fitEvents(store, EvCompactStarted)) == 0 {
		t.Fatal("the calibrated count did not compact")
	}
}

// A switch starts the calibration again: the new model counts its own way.
func TestSwitchResetsCalibration(t *testing.T) {
	a := &windowAdapter{name: "a", window: 100_000}
	l, _ := fitLoop(t, a)
	l.Compactor.SetScale(2)
	if err := l.SwitchModel("b", &windowAdapter{name: "b", window: 100_000}); err != nil {
		t.Fatal(err)
	}
	if l.Compactor.Scale() != 1 {
		t.Fatalf("scale %v after a switch", l.Compactor.Scale())
	}
}

// A retry marked by an earlier run that ended on another error does not make
// this run's first refusal give up; and the retry is not counted as a turn,
// so a refusal on the last turn allowed is still retried.
func TestOverflowRetryIsPerRunAndUncounted(t *testing.T) {
	refusal := &model.StatusError{Status: 400, Body: "prompt is too long: 9000 tokens > 8000 maximum"}
	a := &windowAdapter{name: "m", window: 1_000_000, script: []windowReply{{err: refusal}, {text: "ok"}}}
	l, _ := fitLoop(t, a)
	l.overflowRetried = true // left by an earlier run
	l.Config.MaxTurns = 1
	if reason, err := l.Run(context.Background(), "and now?"); err != nil || reason != TermCompleted {
		t.Fatalf("run: %s %v", reason, err)
	}
}

// The size ceiling bounds compaction, the history kept and each result
// whatever the window: by default a 1M-token model is compacted near 200k.
func TestCompactAtTokensCeiling(t *testing.T) {
	a := &windowAdapter{name: "big", window: 1_000_000}
	c := NewCompactor(a, 0.9)
	msgs := longMessages(10, 100_000) // 250,000 estimated
	if should, _, _ := c.ShouldCompactNow("", msgs, nil); should {
		t.Fatal("250k of 1M compacted with no ceiling")
	}
	c.Ceiling = DefaultCompactAtTokens
	if should, _, _ := c.ShouldCompactNow("", msgs, nil); !should {
		t.Fatal("250k did not compact under a 200k ceiling")
	}
	if got := c.Effective(1_000_000); got != 222_222 {
		t.Fatalf("effective window %d", got)
	}
	if got := c.Effective(32_768); got != 32_768 {
		t.Fatalf("a small window was changed: %d", got)
	}
	l, _ := fitLoop(t, a)
	l.fitCompactor()
	if got, want := l.resultLimitChars(), int(float64(222_222/4)*3.6); got != want {
		t.Fatalf("result cap %d, want %d", got, want)
	}
	if DefaultConfig().CompactAtTokens != DefaultCompactAtTokens {
		t.Fatal("no default ceiling")
	}
}

// The share taken below an unnamed refusal is what lets the one retry fit a
// limit well under the refused size: here the server takes about 38% of the
// first request, which four fifths reaches and the whole refused size (a
// window learned at 100%) does not.
func TestUnnamedShareLeavesMargin(t *testing.T) {
	var sizes []int
	srv := openAIStub(t, 125_000, &sizes)
	a, err := model.New(model.Spec{Type: "openai", Model: "gpt-4.1", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := fitLoop(t, a)
	largeRecent(l)
	if reason, err := l.Run(context.Background(), "and now?"); err != nil || reason != TermCompleted {
		t.Fatalf("run: %s %v; request sizes %v", reason, err, sizes)
	}
	t.Logf("request sizes %v, window %d", sizes, a.Profile().ContextWindow)
}

// A ceiling far below any useful window still leaves a window and a cap: a
// result is cut, never passed whole.
func TestTinyCeilingStillCaps(t *testing.T) {
	a := &windowAdapter{name: "big", window: 1_000_000}
	l, _ := fitLoop(t, a)
	l.Config.CompactAtTokens = 1
	l.fitCompactor()
	if got := l.Compactor.Effective(1_000_000); got < model.MinWindow {
		t.Fatalf("effective window %d", got)
	}
	big := strings.Repeat("x", 2_000_000)
	if got := l.fitResult("c", big); len(got) >= len(big)/10 {
		t.Fatalf("a 2M-character result kept %d characters", len(got))
	}
	small := &Loop{Adapter: &windowAdapter{window: 3}, Tools: tools.NewRegistry()}
	if got := small.resultLimitChars(); got <= 0 {
		t.Fatalf("a positive window gave cap %d, which is no cap", got)
	}
}

// Offloading starts at its fraction of the ceiling's window, not the
// model's: a 1M-token model offloads old results near 133k tokens.
func TestOffloadUnderCeiling(t *testing.T) {
	a := &windowAdapter{name: "big", window: 1_000_000}
	l, store := fitLoop(t, a)
	l.messages = nil
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("r-%d", i)
		l.messages = append(l.messages,
			model.Message{Role: model.RoleUser, Content: "next"},
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: id, Name: "read", Args: json.RawMessage(`{}`)}}},
			model.Message{Role: model.RoleTool, ToolCallID: id, Content: strings.Repeat("y", 60_000)})
	}
	// 150,000 tokens: under 0.6 of 1M, over 0.6 of the default ceiling's window.
	l.Offloader = NewOffloader(0.6)
	l.fitCompactor()
	l.offloadIfNeeded()
	if len(fitEvents(store, EvContextOffloaded)) != 1 {
		t.Fatal("no offload under the ceiling")
	}
	l2, store2 := fitLoop(t, a)
	l2.messages = l.messages[:0:0]
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("r-%d", i)
		l2.messages = append(l2.messages,
			model.Message{Role: model.RoleUser, Content: "next"},
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: id, Name: "read", Args: json.RawMessage(`{}`)}}},
			model.Message{Role: model.RoleTool, ToolCallID: id, Content: strings.Repeat("y", 60_000)})
	}
	l2.Offloader = NewOffloader(0.6)
	l2.Config.CompactAtTokens = 0 // no ceiling: the model's window
	l2.fitCompactor()
	l2.offloadIfNeeded()
	if len(fitEvents(store2, EvContextOffloaded)) != 0 {
		t.Fatal("offloaded at 15% of the model's window with no ceiling")
	}
}

type keptWindows map[string]int

func (k keptWindows) Recall(key string) (int, time.Time) { return k[key], time.Time{} }
func (k keptWindows) Remember(key string, w int)         { k[key] = w }
func (k keptWindows) Forget(key string)                  { delete(k, key) }
func (k keptWindows) Secret() []byte                     { return []byte("agent-test-secret-32-bytes-long!") }

// Through a run: a limit the provider named is kept for later sessions; one
// inferred below a refusal that named none is not.
func TestRunKeepsOnlyNamedLimits(t *testing.T) {
	kept := keptWindows{}
	model.SetWindowMemory(kept)
	t.Cleanup(func() { model.SetWindowMemory(nil) })

	var sizes []int
	named := refusingStub(t, 200_000, &sizes, `{"error":{"message":"This model's maximum context length is 60000 tokens. However, your messages resulted in 90000 tokens.","code":"context_length_exceeded"}}`)
	a, err := model.New(model.Spec{Type: "openai", Model: "gpt-4.1", BaseURL: named.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := fitLoop(t, a)
	largeRecent(l)
	if reason, err := l.Run(context.Background(), "and now?"); err != nil || reason != TermCompleted {
		t.Fatalf("named: %s %v", reason, err)
	}
	if len(kept) != 1 {
		t.Fatalf("named limit kept %d times: %v", len(kept), kept)
	}
	for _, w := range kept {
		if w != 60_000 {
			t.Fatalf("kept %d", w)
		}
	}

	clear(kept)
	var sizes2 []int
	unnamed := openAIStub(t, 200_000, &sizes2)
	b, _ := model.New(model.Spec{Type: "openai", Model: "gpt-4.1", BaseURL: unnamed.URL, APIKey: "k"})
	l2, _ := fitLoop(t, b)
	largeRecent(l2)
	if reason, err := l2.Run(context.Background(), "and now?"); err != nil || reason != TermCompleted {
		t.Fatalf("unnamed: %s %v", reason, err)
	}
	if len(kept) != 0 {
		t.Fatalf("an inferred limit was kept: %v", kept)
	}
}
