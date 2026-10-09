package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sessionsandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// perSessionRig is a server whose sessions each get a control-plane sandbox,
// on a driver that runs commands unconfined in each sandbox's folder: what
// it shows is which folder every surface works in.
type perSessionRig struct {
	srv  *Server
	h    http.Handler
	mem  *controlplane.Memory
	mgr  *sessionsandbox.Manager
	ws   string
	root string
}

func newPerSessionRig(t *testing.T, mod func(*sessionsandbox.Config)) *perSessionRig {
	t.Helper()
	st, err := controlplane.NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", driver.CapCommand)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	folder := func(id string) (string, error) {
		d := filepath.Join(root, id)
		return d, os.MkdirAll(d, 0o700)
	}
	mem := controlplane.NewMemory()
	svc, err := controlplane.New(controlplane.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: folder, ReleaseWorkspace: func(id string) error { return os.RemoveAll(filepath.Join(root, id)) }})
	if err != nil {
		t.Fatal(err)
	}
	mc := sessionsandbox.Config{Service: svc, Tenant: "default", Workspace: folder, Ready: 10 * time.Second,
		Isolation: func(sb controlplane.Sandbox) tools.Isolation { return tools.Isolation{Tier: sb.Tier} }}
	if mod != nil {
		mod(&mc)
	}
	mgr, err := sessionsandbox.New(mc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"
	mw := &auth.Middleware{PublicPaths: PublicPaths(), Verifier: tokens{
		"alice": {Subject: "alice", Tenant: "default"},
		"bob":   {Subject: "bob", Tenant: "default"},
		"eve":   {Subject: "eve", Tenant: "other"},
	}}
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	srv := New(Options{Workspace: ws, Config: cfg, Adapter: stubAdapter{}, Auth: mw, ControlPlane: svc,
		Registry:         tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{}),
		SessionSandboxes: mgr})
	return &perSessionRig{srv: srv, h: srv.Handler(), mem: mem, mgr: mgr, ws: ws, root: root}
}

func (r *perSessionRig) call(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *perSessionRig) open(t *testing.T, token string) (string, *liveSession) {
	t.Helper()
	rec := r.call(t, "POST", "/v1/sessions", token, `{"workbench":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	var c createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	r.srv.mu.RLock()
	live := r.srv.running[c.SessionID]
	r.srv.mu.RUnlock()
	return c.SessionID, live
}

func runTool(t *testing.T, live *liveSession, name string, args any) tools.Result {
	t.Helper()
	tool, ok := live.Loop.Tools.Get(name)
	if !ok {
		t.Fatalf("no %s tool", name)
	}
	raw, _ := json.Marshal(args)
	return tool.Run(context.Background(), live.Loop.Session, raw)
}

// Each session works in its own sandbox's folder, never the server's, and
// neither user's session can see or write the other's.
func TestPerSessionSandboxesKeepSessionsApart(t *testing.T) {
	r := newPerSessionRig(t, nil)
	aliceID, alice := r.open(t, "alice")
	bobID, bob := r.open(t, "bob")
	aws, bws := r.srv.workspaceOf(alice), r.srv.workspaceOf(bob)
	if aws == bws || aws == r.ws || bws == r.ws || filepath.Dir(aws) != r.root || filepath.Dir(bws) != r.root {
		t.Fatalf("workspaces alice %s, bob %s, server %s", aws, bws, r.ws)
	}
	if real, _ := filepath.EvalSymlinks(alice.Loop.Session.Root); real != aws {
		t.Fatalf("alice's tools are rooted at %s", alice.Loop.Session.Root)
	}

	// The bash tool runs in the session's own sandbox and folder.
	res := runTool(t, alice, "bash", map[string]string{"command": "pwd; echo $ABHED_SANDBOX; echo alice > mine.txt", "description": "where"})
	if res.IsError || !strings.Contains(res.Content, aws) || !strings.Contains(res.Content, filepath.Base(aws)) {
		t.Fatalf("alice's bash: %s", res.Content)
	}
	if err := os.WriteFile(filepath.Join(bws, "secret.txt"), []byte("bob's"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(bws, "secret.txt")

	// Alice's file tools cannot read or write bob's folder, by path or by a link.
	if res := runTool(t, alice, "read", map[string]string{"file_path": secret}); !res.IsError || strings.Contains(res.Content, "bob's") {
		t.Fatalf("alice read bob's file: %s", res.Content)
	}
	if res := runTool(t, alice, "write", map[string]string{"file_path": filepath.Join(bws, "planted.txt"), "content": "x"}); !res.IsError {
		t.Fatalf("alice wrote into bob's folder: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(bws, "planted.txt")); err == nil {
		t.Fatal("a file was planted in bob's folder")
	}
	runTool(t, alice, "bash", map[string]string{"command": "ln -s " + secret + " link.txt", "description": "link"})
	if res := runTool(t, alice, "read", map[string]string{"file_path": filepath.Join(aws, "link.txt")}); !res.IsError || strings.Contains(res.Content, "bob's") {
		t.Fatalf("alice read bob's file through a link: %s", res.Content)
	}

	// Nor can the workbench views, downloads or uploads reach across.
	for _, path := range []string{
		"/v1/sessions/" + aliceID + "/file?path=" + url.QueryEscape(secret),
		"/v1/sessions/" + aliceID + "/file?path=link.txt",
		"/v1/sessions/" + aliceID + "/download?path=link.txt",
		"/v1/sessions/" + aliceID + "/download?path=" + url.QueryEscape(secret),
	} {
		if rec := r.call(t, "GET", path, "alice", ""); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "bob's") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := r.call(t, "GET", "/v1/sessions/"+bobID+"/tree?path=.", "alice", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("alice opened bob's tree: %d", rec.Code)
	}
	var tree treeResponse
	rec := r.call(t, "GET", "/v1/sessions/"+aliceID+"/tree?path=.", "alice", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &tree)
	names := map[string]bool{}
	for _, e := range tree.Entries {
		names[e.Name] = true
	}
	if !names["mine.txt"] || names["secret.txt"] {
		t.Fatalf("alice's tree shows %v", names)
	}
	rec = r.call(t, "GET", "/v1/sessions/"+aliceID+"/file?path=mine.txt", "alice", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("alice's own file: %d %s", rec.Code, rec.Body)
	}
	// The server's own workspace is not any session's.
	if err := os.WriteFile(filepath.Join(r.ws, "server.txt"), []byte("server's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := r.call(t, "GET", "/v1/sessions/"+aliceID+"/file?path=server.txt", "alice", ""); rec.Code == http.StatusOK {
		t.Fatalf("a session read the server's workspace: %s", rec.Body)
	}
}

// The record links the session to its sandbox, and the sandbox to the session.
func TestPerSessionRecordLinksTheSandbox(t *testing.T) {
	r := newPerSessionRig(t, nil)
	id, live := r.open(t, "alice")
	runTool(t, live, "bash", map[string]string{"command": "true", "description": "x"})
	evs, err := r.srv.store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	ws := r.srv.workspaceOf(live)
	sbID := filepath.Base(ws)
	var started, bound bool
	for _, ev := range evs {
		switch ev.Type {
		case agent.EvSessionStarted:
			started = strings.Contains(string(ev.Payload), ws)
		case agent.EvSandboxBound:
			bound = strings.Contains(string(ev.Payload), sbID)
		}
	}
	if !started || !bound {
		t.Fatalf("session.started names the folder %v, sandbox.bound names %s %v", started, sbID, bound)
	}
	sb, err := r.mem.Get(context.Background(), sbID)
	if err != nil || sb.Labels[controlplane.LabelSession] != id {
		t.Fatalf("the sandbox does not name the session: %+v %v", sb, err)
	}
}

func TestDeletingASessionDeletesItsSandbox(t *testing.T) {
	r := newPerSessionRig(t, nil)
	id, live := r.open(t, "alice")
	sbID := filepath.Base(r.srv.workspaceOf(live))
	if rec := r.call(t, "DELETE", "/v1/sessions/"+id, "alice", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := r.mem.Get(context.Background(), sbID); err == nil {
		t.Fatal("the session's sandbox outlived it")
	}
	if _, err := os.Stat(filepath.Join(r.root, sbID)); err == nil {
		t.Fatal("the session's folder outlived it")
	}
}

// A session in a tenant other than the control plane's is refused, and no
// sandbox is made for it.
func TestPerSessionRefusesAnotherTenant(t *testing.T) {
	r := newPerSessionRig(t, nil)
	if rec := r.call(t, "POST", "/v1/sessions", "eve", `{"workbench":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("another tenant's session: %d %s", rec.Code, rec.Body)
	}
	if items, _, _ := r.mem.List(context.Background(), controlplane.ListOptions{}); len(items) != 0 {
		t.Fatalf("sandboxes made: %+v", items)
	}
}

func TestPerSessionUploadsGoToTheSessionsFolder(t *testing.T) {
	r := newPerSessionRig(t, nil)
	id, live := r.open(t, "alice")
	upload := func(sessionPath string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "notes.txt")
		_, _ = fw.Write([]byte("hello"))
		_ = mw.Close()
		req := httptest.NewRequest("POST", sessionPath, &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer alice")
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		return rec
	}
	rec := upload("/v1/sessions/" + id + "/upload")
	var got uploadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || !strings.HasPrefix(got.Path, r.srv.workspaceOf(live)+string(filepath.Separator)) {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if rec := upload("/v1/uploads"); rec.Code != http.StatusConflict {
		t.Fatalf("a staged upload with no session folder: %d %s", rec.Code, rec.Body)
	}
}

// Off, a session works in the server's workspace exactly as before.
func TestWithoutPerSessionTheServerWorkspaceIsShared(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	cfg := config.Default()
	s := New(Options{Workspace: ws, Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	id, err := s.openWorkbench(context.Background(), StartSpec{User: "u", Tenant: "default"})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	live := s.running[id]
	s.mu.RUnlock()
	if s.workspaceOf(live) != ws {
		t.Fatalf("workspace %s, want %s", s.workspaceOf(live), ws)
	}
	got, err := s.sessionWorkspace(context.Background(), "anything")
	if err != nil || got != ws {
		t.Fatalf("session workspace %s %v", got, err)
	}
}
