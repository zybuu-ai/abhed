package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/bgjob"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sessionsandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Prepare fills the session's own folder before it starts: what it wrote is
// where the session begins, not a change; what the agent then changes is the
// session's patch, its owner's alone; and deleting the sandbox removes the folder.
func TestPrepareFillsTheSessionsOwnFolder(t *testing.T) {
	r := newPerSessionRig(t)
	id, err := r.srv.StartSession(context.Background(), StartSpec{Prompt: "hi", User: "alice", Tenant: "default",
		Prepare: func(_ context.Context, root string) error {
			return os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o644)
		}})
	if err != nil {
		t.Fatal(err)
	}
	r.srv.mu.RLock()
	live := r.srv.running[id]
	r.srv.mu.RUnlock()
	ws := r.srv.workspaceOf(live)
	if filepath.Dir(ws) != r.root {
		t.Fatalf("prepared %s, not a sandbox's folder", ws)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a.txt")); string(b) != "one\n" {
		t.Fatalf("the prepared file: %q", b)
	}
	if p, ok, _, err := r.srv.SessionPatch("default", "alice", id, 1<<20); err != nil || !ok || p != "" {
		t.Fatalf("before any change: %q %v %v", p, ok, err)
	}
	runTool(t, live, "read", map[string]string{"path": filepath.Join(ws, "a.txt")})
	if res := runTool(t, live, "write", map[string]string{"path": filepath.Join(ws, "a.txt"), "content": "two\n"}); res.IsError {
		t.Fatal(res.Content)
	}
	p, ok, cut, err := r.srv.SessionPatch("default", "alice", id, 1<<20)
	if err != nil || !ok || cut || !strings.Contains(p, "-one\n+two\n") || !strings.Contains(p, "a/a.txt") {
		t.Fatalf("the patch: %q %v %v", p, ok, err)
	}
	if short, _, cut, _ := r.srv.SessionPatch("default", "alice", id, 10); !cut || short != p[:10] {
		t.Fatalf("cut at 10: %q %v", short, cut)
	}
	if p, ok, _, _ := r.srv.SessionPatch("default", "bob", id, 1<<20); ok || p != "" {
		t.Fatalf("bob got alice's patch: %q", p)
	}
	if err := r.srv.DeleteSessionSandbox(context.Background(), "default", "bob", id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatal("bob removed alice's folder")
	}
	if err := r.srv.DeleteSessionSandbox(context.Background(), "default", "alice", id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("the folder is still there: %v", err)
	}
}

// A Prepare that fails leaves no sandbox behind and is refused as such.
func TestAFailedPrepareRemovesItsSandbox(t *testing.T) {
	r := newPerSessionRig(t)
	var root string
	_, err := r.srv.StartSession(context.Background(), StartSpec{Prompt: "hi", User: "alice", Tenant: "default",
		Prepare: func(_ context.Context, dir string) error { root = dir; return errors.New("disk full at /srv/x") }})
	if !errors.Is(err, errPrepare) {
		t.Fatalf("err %v", err)
	}
	if status, msg, _ := StartRefusal(err); status != http.StatusServiceUnavailable || strings.Contains(msg, "/srv/x") {
		t.Fatalf("refusal %d %q", status, msg)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the failed session's folder is still there: %v", err)
	}
}

// Without a folder of the session's own, shared by the server or by the
// user's sessions, nothing is prepared and the session is refused.
func TestPrepareNeedsAFolderOfTheSessionsOwn(t *testing.T) {
	ran := false
	prep := func(context.Context, string) error { ran = true; return nil }
	shared := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{})})
	if _, err := shared.StartSession(context.Background(), StartSpec{Prompt: "hi", User: "alice", Prepare: prep}); !errors.Is(err, ErrNoOwnWorkspace) {
		t.Fatalf("shared workspace: %v", err)
	}
	if status, _, _ := StartRefusal(ErrNoOwnWorkspace); status != http.StatusConflict {
		t.Fatalf("status %d", status)
	}
	user := newPerSessionRigScoped(t, sessionsandbox.ScopeUser, nil)
	if _, err := user.srv.StartSession(context.Background(), StartSpec{Prompt: "hi", User: "alice", Tenant: "default", Prepare: prep}); !errors.Is(err, ErrNoOwnWorkspace) {
		t.Fatalf("a user's folder: %v", err)
	}
	if ran {
		t.Fatal("Prepare ran on a shared folder")
	}
}

// A snapshot's upload is not clamped to the JSON size: its handler caps it.
func TestSnapshotUploadsAreNotClampedToTheJSONSize(t *testing.T) {
	var got int64
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Mounts: []Mount{func(_ *Server, mux *http.ServeMux) {
			mux.HandleFunc("POST "+bgjob.PathSnapshots, func(w http.ResponseWriter, r *http.Request) {
				got, _ = io.Copy(io.Discard, r.Body)
			})
		}}})
	body := strings.NewReader(strings.Repeat("x", maxJSONBody*3))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", bgjob.PathSnapshots, body))
	if got != maxJSONBody*3 {
		t.Fatalf("the handler read %d of %d bytes", got, maxJSONBody*3)
	}
}
