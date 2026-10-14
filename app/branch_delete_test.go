package app

import (
	"context"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store/local"
)

// failingCopy is the local record, failing the copied events' appends.
type failingCopy struct{ *local.Store }

func (f failingCopy) Append(ev agent.Event) error {
	if ev.Type != agent.EvSessionBranched {
		return errors.New("disk full")
	}
	return f.Store.Append(ev)
}

// A branch whose copy fails part way is deleted, so no half branch is left
// in the record's list.
func TestAFailedBranchLeavesNoSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec, err := openRecord(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	src := []agent.Event{{ID: "e1", SessionID: "src", Seq: 1, Type: agent.EvUserMessage, Payload: []byte(`{"text":"hi"}`),
		Actor: agent.ActorUser, Trust: agent.Trusted}}
	if _, err := copyBranch(context.Background(), failingCopy{rec}, config.Config{}, "src", src, 0, "", false); err == nil {
		t.Fatal("the copy did not fail")
	}
	if l, _ := rec.Index().List(local.Filter{All: true}); len(l) != 0 {
		t.Fatalf("a half branch is left listed: %+v", l)
	}
}
