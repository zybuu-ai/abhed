package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

type requestIDs struct{ seen, calls, recorded []string }

func (r *requestIDs) Approve(ctx context.Context, _ string, _ json.RawMessage, _ policy.Result) (bool, error) {
	r.seen = append(r.seen, RequestIDOf(ctx))
	r.calls = append(r.calls, CallIDOf(ctx))
	if p, ok := RequestedOf(ctx); ok {
		r.recorded = append(r.recorded, p.CallID+" "+string(p.Args))
	}
	return true, nil
}

// Models reuse call ids across turns (call_0, name-1), so an answer is bound
// to the recorded request instead: each ask names its own action.requested.
func TestApproverIsToldAUniqueRequestIDPerAsk(t *testing.T) {
	dir := tempDir(t)
	same := func(name string) model.ToolCall {
		b, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, name), "content": "x"})
		return model.ToolCall{ID: "call_0", Name: "write", Args: b}
	}
	l, store := harnessIn(t, dir, []scriptedTurn{{calls: []model.ToolCall{same("a.txt")}}, {calls: []model.ToolCall{same("b.txt")}}},
		policy.ModeDefault, true)
	ids := &requestIDs{}
	l.Approver = ids
	if _, err := l.Run(context.Background(), "write two files"); err != nil {
		t.Fatal(err)
	}
	if len(ids.seen) != 2 || ids.seen[0] == "" || ids.seen[0] == ids.seen[1] {
		t.Fatalf("request ids = %q, want two distinct", ids.seen)
	}
	// The call id is told too, so an approver can name the call its events name.
	if len(ids.calls) != 2 || ids.calls[0] != "call_0" || ids.calls[1] != "call_0" {
		t.Fatalf("call ids = %q, want the model's call_0 twice", ids.calls)
	}
	if len(ids.recorded) != 2 || !strings.Contains(ids.recorded[0], "a.txt") || !strings.Contains(ids.recorded[1], "b.txt") {
		t.Fatalf("recorded requests = %q, want each ask's own", ids.recorded)
	}
	evs, _ := store.Events("sess1")
	var asked []string
	for _, e := range evs {
		if e.Type == EvActionRequested {
			asked = append(asked, e.ID)
		}
	}
	if len(asked) != 2 || asked[0] != ids.seen[0] || asked[1] != ids.seen[1] {
		t.Fatalf("request ids %q are not the action.requested events %q", ids.seen, asked)
	}
}

type requestedArgs struct{ args []string }

func (r *requestedArgs) Approve(ctx context.Context, _ string, _ json.RawMessage, _ policy.Result) (bool, error) {
	p, _ := RequestedOf(ctx)
	r.args = append(r.args, string(p.Args))
	return false, nil
}

// The approver is shown the request as recorded, so a secret in the call's
// arguments reaches it redacted, as it reaches the record.
func TestApproverSeesTheRedactedRequest(t *testing.T) {
	dir := tempDir(t)
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("TOKEN", "hunter2-s3cr3t-value"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "a.txt"), "content": "hunter2-s3cr3t-value"})
	l, _ := harnessIn(t, dir, []scriptedTurn{{calls: []model.ToolCall{{ID: "call_0", Name: "write", Args: b}}}}, policy.ModeDefault, true)
	l.Recorder.Redact = vault.Redactor()
	got := &requestedArgs{}
	l.Approver = got
	if _, err := l.Run(context.Background(), "write it"); err != nil {
		t.Fatal(err)
	}
	if len(got.args) != 1 || strings.Contains(got.args[0], "hunter2") || !strings.Contains(got.args[0], "[secret:TOKEN]") {
		t.Fatalf("approver saw %q", got.args)
	}
}

// A request the record withheld is reported as withheld, never as an empty one.
func TestRequestedOfReportsAWithheldRecord(t *testing.T) {
	for _, payload := range []string{string(withheldPayload), `{"call_id":`, `{"tool":"bash"}`} {
		r, ok := RequestedOf(WithRequested(context.Background(), Event{Payload: json.RawMessage(payload)}))
		if !ok || !r.Withheld || r.Args != nil {
			t.Fatalf("payload %s: ok %v requested %+v", payload, ok, r)
		}
	}
	good, _ := json.Marshal(ActionRequested{CallID: "c1", Tool: "bash", Args: json.RawMessage(`{"command":"ls"}`)})
	if r, ok := RequestedOf(WithRequested(context.Background(), Event{Payload: good})); !ok || r.Withheld || r.CallID != "c1" {
		t.Fatalf("readable record: ok %v requested %+v", ok, r)
	}
	if _, ok := RequestedOf(context.Background()); ok {
		t.Fatal("a request outside the loop was reported")
	}
}
