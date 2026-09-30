package abhed_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	abhed "github.com/zybuu-ai/abhed/sdk"
	"github.com/zybuu-ai/abhed/store/local"
)

// An embedded agent given the local record writes a chained session there:
// listed under its workspace, verified, held while the agent is open and
// let go by Close, so the desktop app and the command line share one record.
func TestOptionsStoreKeepsAVerifiedRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(t.TempDir(), "records")
	rec, err := abhed.OpenLocalRecord(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: ws, Store: rec,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "hello there"); err != nil {
		t.Fatal(err)
	}
	other, err := local.Open(local.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Acquire(a.ID()); !errors.Is(err, local.ErrHeldElsewhere) {
		t.Fatalf("an open agent's session was taken: %v", err)
	}
	a.Close()
	l, _ := other.Index().List(local.Filter{Cwd: ws})
	if len(l) != 1 || l[0].ID != a.ID() || l[0].Title != "hello there" {
		t.Fatalf("list: %+v", l)
	}
	if rep, err := other.Verify(a.ID()); err != nil || !rep.OK || rep.Events < 3 {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	if err := other.Acquire(a.ID()); err != nil {
		t.Fatalf("Close did not let the session go: %v", err)
	}
}
