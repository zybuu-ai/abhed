package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// fragmentAdapter streams one reply in the given fragments, then ends the
// stream with an error or a cancel when asked to.
type fragmentAdapter struct {
	chunks []string
	err    error
	cancel context.CancelFunc
}

func (f *fragmentAdapter) Name() string                           { return "fragments" }
func (f *fragmentAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (f *fragmentAdapter) CountTokens(model.Request) (int, error) { return 0, nil }

func (f *fragmentAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, len(f.chunks)+2)
	for _, c := range f.chunks {
		ch <- model.Chunk{Type: model.ChunkText, Text: c}
	}
	if f.err != nil {
		ch <- model.Chunk{Type: model.ChunkError, Err: f.err}
	}
	if f.cancel != nil {
		f.cancel()
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// streamRun plays one streamed reply through a loop whose recorder redacts
// the given values, and returns the recorded and the live events.
func streamRun(ctx context.Context, t *testing.T, vals map[string]string, a *fragmentAdapter) (recorded, live []Event) {
	t.Helper()
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	for name, v := range vals {
		if err := vault.Set(name, v); err != nil {
			t.Fatal(err)
		}
	}
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	sub := store.Subscribe("s")
	rec := NewRecorder(store, "s", "")
	rec.Redact = vault.Redactor()
	l := NewLoop(a, tools.NewRegistry(), policy.New(policy.ModeBypass), AutoApprove{Yes: true}, sess, rec, DefaultConfig())
	l.Run(ctx, "go")
	recorded, _ = store.Events("s")
	for len(sub) > 0 {
		live = append(live, <-sub)
	}
	return recorded, live
}

// checkStream fails when a value reaches any event, or when the deltas do not
// add up to the redacted reply.
func checkStream(t *testing.T, vals map[string]string, reply string, recorded, live []Event) {
	t.Helper()
	want := reply
	for _, name := range sortedByLength(vals) {
		want = strings.ReplaceAll(want, vals[name], "[secret:"+name+"]")
	}
	for _, evs := range [][]Event{recorded, live} {
		var deltas strings.Builder
		for _, e := range evs {
			for _, v := range vals {
				if strings.Contains(string(e.Payload), v) {
					t.Fatalf("a value reached %s: %s", e.Type, e.Payload)
				}
			}
			if e.Type == EvAgentDelta {
				var d Delta
				if err := json.Unmarshal(e.Payload, &d); err != nil {
					t.Fatal(err)
				}
				deltas.WriteString(d.Text)
			}
		}
		if deltas.String() != want {
			t.Fatalf("the deltas read\n%q\nwant\n%q", deltas.String(), want)
		}
	}
}

func sortedByLength(vals map[string]string) []string {
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(vals[names[i]]) > len(vals[names[j]]) })
	return names
}

// runeCuts lists every rune boundary inside s, so fragments stay valid text
// as a real endpoint sends them.
func runeCuts(s string) []int {
	var cuts []int
	for i := range s {
		if i > 0 {
			cuts = append(cuts, i)
		}
	}
	return cuts
}

// A secret split across fragments, at every position, never reaches the record
// or the live stream, and the deltas still read as the redacted reply.
func TestSecretSplitAcrossDeltasIsRedacted(t *testing.T) {
	vals := map[string]string{"API_TOKEN": "tok-9f8e7d-4c3b2a"}
	reply := "Here is the key you pasted: tok-9f8e7d-4c3b2a and that is all of it."
	for _, cut := range runeCuts(reply) {
		a := &fragmentAdapter{chunks: []string{reply[:cut], reply[cut:]}}
		recorded, live := streamRun(context.Background(), t, vals, a)
		checkStream(t, vals, reply, recorded, live)
	}
}

// Three fragments, both cuts anywhere around the secret: short fragments are
// coalesced or not, and neither way lets the value through.
func TestSecretSplitThreeWaysIsRedacted(t *testing.T) {
	vals := map[string]string{"API_TOKEN": "tok-9f8e7d-4c3b2a"}
	reply := "Key: tok-9f8e7d-4c3b2a, end."
	cuts := runeCuts(reply)
	for i, a := range cuts {
		for _, b := range cuts[i+1:] {
			ad := &fragmentAdapter{chunks: []string{reply[:a], reply[a:b], reply[b:]}}
			recorded, live := streamRun(context.Background(), t, vals, ad)
			checkStream(t, vals, reply, recorded, live)
		}
	}
}

// Values of different lengths, one inside another, each split everywhere.
func TestSeveralSecretsSplitAcrossDeltas(t *testing.T) {
	vals := map[string]string{
		"SHORT": "k1x",
		"LONG":  "sk-live-0123456789abcdefghijklmnopqrstuv",
		"INNER": "0123456789",
	}
	reply := "first k1x then sk-live-0123456789abcdefghijklmnopqrstuv then 0123456789 and k1x."
	for _, cut := range runeCuts(reply) {
		a := &fragmentAdapter{chunks: []string{reply[:cut], reply[cut:]}}
		recorded, live := streamRun(context.Background(), t, vals, a)
		checkStream(t, vals, reply, recorded, live)
	}
}

// Multi-byte text around and inside the value, split at every rune boundary.
func TestSecretSplitAmongMultiByteRunes(t *testing.T) {
	vals := map[string]string{"KEY": "clé-sécrète-ü✓"}
	reply := "Voilà — ключ: clé-sécrète-ü✓ — fin ✓ ok 日本語."
	for _, cut := range runeCuts(reply) {
		a := &fragmentAdapter{chunks: []string{reply[:cut], reply[cut:]}}
		recorded, live := streamRun(context.Background(), t, vals, a)
		checkStream(t, vals, reply, recorded, live)
	}
}

// What was held back is still emitted, redacted, when the stream fails or the
// session is interrupted mid-reply.
func TestHeldBackTextIsFlushedOnErrorAndCancel(t *testing.T) {
	vals := map[string]string{"API_TOKEN": "tok-9f8e7d-4c3b2a"}
	reply := "The value is tok-9f8e7d-4c3b2a"
	for _, cut := range runeCuts(reply) {
		a := &fragmentAdapter{chunks: []string{reply[:cut], reply[cut:]}, err: errors.New("stream broke")}
		recorded, live := streamRun(context.Background(), t, vals, a)
		checkStream(t, vals, reply, recorded, live)

		ctx, cancel := context.WithCancel(context.Background())
		a = &fragmentAdapter{chunks: []string{reply[:cut], reply[cut:]}, cancel: cancel}
		recorded, live = streamRun(ctx, t, vals, a)
		checkStream(t, vals, reply, recorded, live)
		cancel()
	}
}

// With no secrets configured nothing is held back: each fragment is a delta
// of its own, exactly as it arrived.
func TestNoSecretsMeansNoLag(t *testing.T) {
	chunks := []string{"the first fragment ", "a second fragment ", "and a third ✓ one."}
	recorded, _ := streamRun(context.Background(), t, nil, &fragmentAdapter{chunks: chunks})
	var got []string
	for _, e := range recorded {
		if e.Type == EvAgentDelta {
			var d Delta
			_ = json.Unmarshal(e.Payload, &d)
			got = append(got, d.Text)
		}
	}
	if strings.Join(got, "|") != strings.Join(chunks, "|") {
		t.Fatalf("deltas %q, want %q", got, chunks)
	}
}

// Values that begin as a label does, split into two or three fragments
// anywhere, never reach an event.
func TestBracketSecretsSplitAcrossDeltas(t *testing.T) {
	vals := map[string]string{"BRACKET": "[abcdefghijkl", "LABELLIKE": "[secret:k9zz"}
	reply := "see [abcdefghijkl now, and [secret:k9zz too."
	cuts := runeCuts(reply)
	for i, a := range cuts {
		for _, b := range cuts[i:] {
			ad := &fragmentAdapter{chunks: []string{reply[:a], reply[a:b], reply[b:]}}
			recorded, live := streamRun(context.Background(), t, vals, ad)
			checkStream(t, vals, reply, recorded, live)
		}
	}
}

// A value whose bytes also occur across a JSON escape is matched in the text,
// not the escape: the real occurrence is redacted and the rest left alone.
func TestSecretBesideAnEscapeIsRedacted(t *testing.T) {
	cases := []struct{ value, text, want string }{
		{"003e9a8b7c6d5e", "a>9a8b7c6d5e key 003e9a8b7c6d5e", "a>9a8b7c6d5e key [secret:K]"},
		{"nf00d1e2b3c4", "log:\nf00d1e2b3c4 and key nf00d1e2b3c4", "log:\nf00d1e2b3c4 and key [secret:K]"},
	}
	for _, tc := range cases {
		vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
		if err := vault.Set("K", tc.value); err != nil {
			t.Fatal(err)
		}
		if got := redactedText(vault.Redactor().Redact, tc.text); got != tc.want {
			t.Fatalf("redacted %q to %q, want %q", tc.text, got, tc.want)
		}
		for _, cut := range runeCuts(tc.text) {
			a := &fragmentAdapter{chunks: []string{tc.text[:cut], tc.text[cut:]}}
			recorded, _ := streamRun(context.Background(), t, map[string]string{"K": tc.value}, a)
			var deltas strings.Builder
			for _, e := range recorded {
				var d Delta
				if err := json.Unmarshal(e.Payload, &d); err != nil {
					t.Fatalf("%s is not valid JSON: %s", e.Type, e.Payload)
				}
				if e.Type == EvAgentDelta {
					deltas.WriteString(d.Text)
				}
				if e.Type == EvAgentMessage && d.Text != tc.want {
					t.Fatalf("the message reads %q, want %q", d.Text, tc.want)
				}
			}
			if deltas.String() != tc.want {
				t.Fatalf("cut %d: the deltas read %q, want %q", cut, deltas.String(), tc.want)
			}
		}
	}
}

type brokenRedactor struct{}

func (brokenRedactor) Redact(b []byte) []byte { return breakJSON(b) }
func (brokenRedactor) Span() int              { return 4 }

// Redaction that fails withholds the text; it never falls back to the
// original, and the record still holds valid JSON.
func TestFailedRedactionIsWithheld(t *testing.T) {
	if got := redactedText(breakJSON, "the key is hunter22"); got != Withheld {
		t.Fatalf("a failed redaction returned %q", got)
	}
	rec := NewRecorder(NewMemStore(), "s", "")
	rec.Redact = brokenRedactor{}
	ev, err := rec.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: "the key is hunter22"})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(ev.Payload) || strings.Contains(string(ev.Payload), "hunter22") ||
		!strings.Contains(string(ev.Payload), Withheld) {
		t.Fatalf("recorded %s", ev.Payload)
	}
}

// A typed nil redactor is no redactor: it neither panics nor holds text back.
func TestTypedNilRedactorIsIgnored(t *testing.T) {
	var none *secrets.Redactor
	rec := NewRecorder(NewMemStore(), "s", "")
	rec.Redact = none
	ev, err := rec.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: "plain"})
	if err != nil || !strings.Contains(string(ev.Payload), "plain") {
		t.Fatalf("recorded %s, %v", ev.Payload, err)
	}
	if b := (&Loop{Recorder: rec}).fragments(); b.span != 0 || b.push("as is") != "as is" {
		t.Fatal("a typed nil redactor changed the stream")
	}
}
