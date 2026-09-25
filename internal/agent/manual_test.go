package agent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func manualLoop(t *testing.T) (*Loop, *MemStore) {
	t.Helper()
	store := NewMemStore()
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddDeny("bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	return &Loop{Tools: tools.NewRegistry(tools.Bash{}), Policy: pol, Recorder: NewRecorder(store, "s-manual", "")}, store
}

func bashArgs(command string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": command})
	return b
}

func decisionsOf(t *testing.T, store *MemStore, id string) []Event {
	t.Helper()
	evs, err := store.Events("s-manual")
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range evs {
		var p map[string]string
		_ = json.Unmarshal(e.Payload, &p)
		if (e.Type == EvActionApproved || e.Type == EvActionDenied) && p["call_id"] == id {
			out = append(out, e)
		}
	}
	return out
}

// A typed destructive command needs an answer; the answer is what the record holds.
func TestManualAuthorizeTypedConfirmsDestructive(t *testing.T) {
	l, store := manualLoop(t)

	tool, refused, confirm, err := l.ManualAuthorizeTyped("u1", bashArgs("rm -rf build"), Unanswered)
	if err != nil || tool != nil || refused != nil || confirm == "" {
		t.Fatalf("unanswered: tool %v refused %v confirm %q err %v", tool, refused, confirm, err)
	}
	if evs, _ := store.Events("s-manual"); len(evs) != 0 {
		t.Fatalf("an unanswered prompt was recorded: %d events", len(evs))
	}

	tool, refused, _, err = l.ManualAuthorizeTyped("u2", bashArgs("rm -rf build"), Declined)
	if err != nil || tool != nil || refused == nil || !refused.IsError {
		t.Fatalf("declined: tool %v refused %v err %v", tool, refused, err)
	}
	if ds := decisionsOf(t, store, "u2"); len(ds) != 1 || ds[0].Type != EvActionDenied || ds[0].Actor != ActorUser {
		t.Fatalf("declined record: %+v", ds)
	}

	tool, refused, _, err = l.ManualAuthorizeTyped("u3", bashArgs("rm -rf build"), Confirmed)
	if err != nil || tool == nil || refused != nil {
		t.Fatalf("confirmed: tool %v refused %v err %v", tool, refused, err)
	}
	ds := decisionsOf(t, store, "u3")
	var p map[string]string
	if len(ds) == 1 {
		_ = json.Unmarshal(ds[0].Payload, &p)
	}
	if len(ds) != 1 || ds[0].Type != EvActionApproved || p["confirmed"] != "true" || p["step"] != "destructive" || p["by"] != "user" {
		t.Fatalf("confirmed record: %+v %v", ds, p)
	}

	// A deny holds whatever the answer; an ordinary ask needs none.
	if _, refused, _, _ := l.ManualAuthorizeTyped("u4", bashArgs("curl http://x"), Confirmed); refused == nil {
		t.Fatal("a confirmed line passed a deny rule")
	}
	if tool, _, confirm, _ := l.ManualAuthorizeTyped("u5", bashArgs("touch a"), Unanswered); tool == nil || confirm != "" {
		t.Fatalf("an ordinary ask was held for confirmation: %q", confirm)
	}
	// Declining a line that needed no confirmation runs and records nothing.
	if tool, refused, _, err := l.ManualAuthorizeTyped("u6", bashArgs("touch a"), Declined); !errors.Is(err, ErrNothingToDecline) || tool != nil || refused != nil {
		t.Fatalf("declined ordinary line: tool %v refused %v err %v", tool, refused, err)
	}
	evs, _ := store.Events("s-manual")
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if p["call_id"] == "u6" {
			t.Fatalf("a declined ordinary line was recorded: %s", e.Type)
		}
	}
}

// The other manual paths are unchanged: an explorer delete is answered by the
// person's click, and the interactive terminal screens only for a deny.
func TestManualAuthorizeAndScreenAreUnchanged(t *testing.T) {
	l, store := manualLoop(t)
	if tool, refused, err := l.ManualAuthorize("bash", "u1", bashArgs("rm -rf build")); err != nil || tool == nil || refused != nil {
		t.Fatalf("ManualAuthorize: tool %v refused %v err %v", tool, refused, err)
	}
	var p map[string]string
	ds := decisionsOf(t, store, "u1")
	if len(ds) == 1 {
		_ = json.Unmarshal(ds[0].Payload, &p)
	}
	if len(ds) != 1 || ds[0].Type != EvActionApproved || p["by"] != "user" {
		t.Fatalf("ManualAuthorize record: %+v", ds)
	}
	if _, found := p["confirmed"]; found {
		t.Fatalf("ManualAuthorize recorded a confirmation: %v", p)
	}
	if refused, err := l.ManualScreen("u2", "rm -rf build"); err != nil || refused != nil {
		t.Fatalf("ManualScreen refused a destructive line: %v %v", refused, err)
	}
	if refused, err := l.ManualScreen("u3", "ls; curl http://x"); err != nil || refused == nil {
		t.Fatalf("ManualScreen passed a denied command in a chain: %v %v", refused, err)
	}
}
