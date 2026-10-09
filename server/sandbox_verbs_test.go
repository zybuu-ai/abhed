package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/server/expose"
)

func TestSandboxSnapshotAndRestoreRoutes(t *testing.T) {
	r := newCPRigWith(t, nil, nil, driver.CapSnapshot)
	sb := r.ready(t, "alice")
	path := "/v1/sandboxes/" + sb.ID

	rec := r.do(t, cpCall{method: "POST", path: path + ":snapshot", token: "alice", body: `{"name":"before"}`})
	if rec.Code != http.StatusCreated {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var snap snapshotResult
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil || snap.Snapshot.ID == "" || snap.Generation != sb.Generation {
		t.Fatalf("snapshot body %s: %v", rec.Body.String(), err)
	}
	if rec := r.do(t, cpCall{method: "POST", path: path + ":snapshot", token: "bob"}); rec.Code != http.StatusNotFound {
		t.Fatalf("bob snapshots alice's sandbox: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "POST", path: path + ":snapshot", token: "alice", body: `{"bogus":1}`}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}

	snap.Snapshot.Digest = "sha256:00"
	body, _ := json.Marshal(map[string]any{"snapshot": snap.Snapshot})
	// A running sandbox is not restored.
	if rec := r.do(t, cpCall{method: "POST", path: path + ":restore", token: "alice", body: string(body)}); rec.Code != http.StatusConflict {
		t.Fatalf("restore while ready: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(t, cpCall{method: "POST", path: path + ":stop", token: "alice", body: `{"grace_seconds":0}`}); rec.Code != http.StatusAccepted {
		t.Fatalf("stop: %d", rec.Code)
	}
	_ = r.svc.Reconcile(context.Background(), sb.ID)
	if rec := r.do(t, cpCall{method: "POST", path: path + ":restore", token: "alice", body: `{}`}); rec.Code != http.StatusBadRequest {
		t.Fatalf("restore without a snapshot: %d", rec.Code)
	}
	rec = r.do(t, cpCall{method: "POST", path: path + ":restore", token: "alice", body: string(body)})
	if got := decodeSandbox(t, rec); rec.Code != http.StatusAccepted || got.State != controlplane.StateStarting || got.Generation != sb.Generation+1 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSandboxCopyRoutes(t *testing.T) {
	r := newCPRigWith(t, nil, nil, driver.CapCopy)
	sb := r.ready(t, "alice")
	path := "/v1/sandboxes/" + sb.ID

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()

	if rec := r.do(t, cpCall{method: "POST", path: path + ":cp?direction=in&path=src", token: "alice", body: archive.String()}); rec.Code != http.StatusOK {
		t.Fatalf("copy in: %d %s", rec.Code, rec.Body.String())
	}
	rec := r.do(t, cpCall{method: "POST", path: path + ":cp", token: "alice", body: `{"direction":"out","path":"src"}`})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-tar" || rec.Body.String() != archive.String() {
		t.Fatalf("copy out: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, c := range []cpCall{
		{path: path + ":cp?direction=sideways&path=x"},
		{path: path + ":cp?direction=out"},
		{path: path + ":cp", body: `{"direction":"out","path":"x","extra":1}`},
	} {
		c.method, c.token = "POST", "alice"
		if rec := r.do(t, c); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: %d", c.path, c.body, rec.Code)
		}
	}
	if rec := r.do(t, cpCall{method: "POST", path: path + ":cp?direction=out&path=missing", token: "alice"}); rec.Code != http.StatusNotFound {
		t.Fatalf("a missing path: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "POST", path: path + ":cp?direction=out&path=src", token: "bob"}); rec.Code != http.StatusNotFound {
		t.Fatalf("bob copies from alice's sandbox: %d", rec.Code)
	}
}

func TestSandboxExposeRoute(t *testing.T) {
	var svc *controlplane.Service
	ex := expose.New(expose.Options{Enabled: true, Current: func(ctx context.Context, id string) (int64, bool, error) {
		return svc.Current(ctx, id)
	}})
	r := newCPRigWith(t, nil, ex, driver.CapForward)
	svc = r.svc
	sb := r.ready(t, "alice")
	path := "/v1/sandboxes/" + sb.ID

	for _, body := range []string{`{"port":0}`, `{"port":80,"ttl_seconds":7200}`, `{"port":"80"}`} {
		if rec := r.do(t, cpCall{method: "POST", path: path + ":expose", token: "alice", body: body}); rec.Code != http.StatusBadRequest {
			t.Errorf("expose %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec := r.do(t, cpCall{method: "POST", path: path + ":expose", token: "alice", body: `{"port":8000,"ttl_seconds":60}`})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expose: %d %s", rec.Code, rec.Body.String())
	}
	var e exposeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || !strings.HasPrefix(e.Path, expose.DefaultPrefix) || !strings.HasSuffix(e.URL, e.Path) {
		t.Fatalf("expose body %s: %v", rec.Body.String(), err)
	}
	// Only its owner reaches it; everyone else, and nobody signed in, gets
	// what an unknown exposure gets.
	if rec := r.do(t, cpCall{method: "GET", path: e.Path, token: "bob"}); rec.Code != http.StatusNotFound {
		t.Fatalf("bob: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "GET", path: e.Path}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("nobody: %d", rec.Code)
	}
	// Bob's exposure of his own sandbox is his alone, too.
	bobs := r.ready(t, "bob")
	rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + bobs.ID + ":expose", token: "bob", body: `{"port":8000}`})
	var be exposeResult
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &be) != nil {
		t.Fatalf("bob's expose: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(t, cpCall{method: "GET", path: be.Path, token: "alice"}); rec.Code != http.StatusNotFound {
		t.Fatalf("alice on bob's exposure: %d", rec.Code)
	}
	// The owner's request goes through the driver's forward; the fake
	// echoes rather than answering HTTP, so the proxy says so.
	if rec := r.do(t, cpCall{method: "GET", path: e.Path, token: "alice"}); rec.Code != http.StatusBadGateway {
		t.Fatalf("alice: %d %s", rec.Code, rec.Body.String())
	}
	// Stopping the sandbox ends it.
	if rec := r.do(t, cpCall{method: "POST", path: path + ":stop", token: "alice"}); rec.Code != http.StatusAccepted {
		t.Fatalf("stop: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "GET", path: e.Path, token: "alice"}); rec.Code != http.StatusNotFound {
		t.Fatalf("after stop: %d", rec.Code)
	}
	if got := ex.List(sb.ID); len(got) != 0 {
		t.Fatalf("still exposed: %+v", got)
	}
	evs, _ := r.store.AuditFor(context.Background(), sb.ID)
	seen := false
	for _, ev := range evs {
		seen = seen || ev.Action == "expose" && ev.Outcome == controlplane.OutcomeOK && ev.Detail["port"] == "8000"
	}
	if !seen {
		t.Fatalf("the exposure is not in the record: %+v", evs)
	}
}
