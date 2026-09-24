package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// workbench is a proxy-auth server over a known workspace, with one session
// owned by tenant acme.
type workbench struct {
	t         *testing.T
	s         *Server
	h         http.Handler
	workspace string
	session   string
}

func newWorkbench(t *testing.T, edit func(*config.Config)) *workbench {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	if edit != nil {
		edit(&cfg)
	}
	dir := t.TempDir()
	s := New(Options{
		Workspace: dir, Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Glob{}),
	})
	wb := &workbench{t: t, s: s, h: s.Handler(), workspace: dir}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"work"}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body)
	}
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	wb.session = created.SessionID
	return wb
}

func (wb *workbench) write(rel, content string) {
	wb.t.Helper()
	path := filepath.Join(wb.workspace, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		wb.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		wb.t.Fatal(err)
	}
}

func (wb *workbench) get(tenant, endpoint string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sessions/"+wb.session+"/"+endpoint, nil)
	req.Header.Set("X-Abhed-Tenant", tenant)
	wb.h.ServeHTTP(rec, req)
	return rec
}

func (wb *workbench) file(path string) (*httptest.ResponseRecorder, fileResponse) {
	rec := wb.get("acme", "file?path="+url.QueryEscape(path))
	var out fileResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (wb *workbench) tree(path string) map[string]treeEntry {
	wb.t.Helper()
	rec := wb.get("acme", "tree?path="+url.QueryEscape(path))
	if rec.Code != http.StatusOK {
		wb.t.Fatalf("tree %q: %d %s", path, rec.Code, rec.Body)
	}
	var out treeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		wb.t.Fatal(err)
	}
	names := map[string]treeEntry{}
	for _, e := range out.Entries {
		names[e.Name] = e
	}
	return names
}

// The workspace holds source, and a session's changes are its work product:
// another tenant must not learn the session exists, from any of the three.
func TestWorkbenchIsScopedLikeReplay(t *testing.T) {
	wb := newWorkbench(t, nil)
	wb.write("main.go", "package main\n")

	for _, endpoint := range []string{"tree", "file?path=main.go", "changes"} {
		if got := wb.get("other", endpoint); got.Code != http.StatusNotFound ||
			!strings.Contains(got.Body.String(), "session not found") {
			t.Errorf("cross-tenant %s: got %d %s, want 404 session not found", endpoint, got.Code, got.Body)
		}
		if strings.Contains(wb.get("other", endpoint).Body.String(), "main") {
			t.Errorf("cross-tenant %s leaked workspace content", endpoint)
		}
		if got := wb.get("acme", endpoint); got.Code != http.StatusOK {
			t.Errorf("owner's %s: %d %s", endpoint, got.Code, got.Body)
		}
	}

	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/nope/tree", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session: got %d, want 404", rec.Code)
	}
}

func TestWorkbenchFileIsJSON(t *testing.T) {
	wb := newWorkbench(t, nil)
	page := "<script>alert(1)</script>\n"
	wb.write("sub/page.html", page)

	rec, got := wb.file("sub/page.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("a workspace file must never be served as its own type, got %q", ct)
	}
	if got.Path != "sub/page.html" || got.Content != page || got.Size != int64(len(page)) || got.Binary || got.Truncated {
		t.Fatalf("unexpected body: %+v", got)
	}
}

func TestWorkbenchRefusesPathsOutsideTheWorkspace(t *testing.T) {
	wb := newWorkbench(t, nil)
	wb.write("ok.txt", "fine\n")

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(wb.workspace, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wb.workspace, "linkdir")); err != nil {
		t.Fatal(err)
	}
	up, err := filepath.Rel(wb.workspace, secret)
	if err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"traversal":             up,
		"traversal from inside": "sub/../" + up,
		"etc passwd":            "../../../../../../../../etc/passwd",
		"absolute":              secret,
		"symlink":               "link.txt",
		"through a symlink dir": "linkdir/secret.txt",
	} {
		rec, got := wb.file(path)
		if rec.Code != http.StatusNotFound || got.Content != "" || strings.Contains(rec.Body.String(), "TOP-SECRET") {
			t.Errorf("%s (%s): got %d %s, want 404 and no content", name, path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{up, secret, outside, "linkdir", ".."} {
		if rec := wb.get("acme", "tree?path="+url.QueryEscape(path)); rec.Code != http.StatusNotFound {
			t.Errorf("tree of %s: got %d %s, want 404", path, rec.Code, rec.Body)
		}
	}

	// A link that leads outside is not offered either.
	names := wb.tree("")
	if _, ok := names["ok.txt"]; !ok {
		t.Fatalf("tree is missing an ordinary file: %v", names)
	}
	for _, link := range []string{"link.txt", "linkdir"} {
		if _, ok := names[link]; ok {
			t.Errorf("tree lists %s, which resolves outside the workspace", link)
		}
	}
}

// A deny rule on read binds the viewer as it binds the agent, including
// through a link with an innocent name.
func TestWorkbenchHonoursReadDenyRules(t *testing.T) {
	wb := newWorkbench(t, func(cfg *config.Config) {
		cfg.Permissions.Deny = append(cfg.Permissions.Deny,
			"read(**/.env)", "read(**/.ssh/**)")
	})
	wb.write(".env", "API_KEY=hunter2\n")
	wb.write(".ssh/id_rsa", "PRIVATE-KEY\n")
	wb.write("app/.env", "API_KEY=hunter2\n")
	wb.write("app/main.go", "package main\n")
	if err := os.Symlink(filepath.Join(wb.workspace, ".env"), filepath.Join(wb.workspace, "notes.txt")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{".env", "app/.env", ".ssh/id_rsa", "notes.txt",
		filepath.Join(wb.workspace, ".env")} {
		rec, got := wb.file(path)
		body := rec.Body.String()
		if rec.Code != http.StatusForbidden || got.Content != "" ||
			strings.Contains(body, "hunter2") || strings.Contains(body, "PRIVATE-KEY") {
			t.Errorf("%s: got %d %s, want 403 and no content", path, rec.Code, body)
		}
	}
	if rec := wb.get("acme", "tree?path=.ssh"); rec.Code != http.StatusForbidden {
		t.Errorf("listing a denied directory: got %d %s, want 403", rec.Code, rec.Body)
	}
	if rec, got := wb.file("app/main.go"); rec.Code != http.StatusOK || got.Content == "" {
		t.Errorf("an ordinary file beside a denied one: %d %s", rec.Code, rec.Body)
	}

	names := wb.tree("")
	for _, hidden := range []string{".env", ".ssh", "notes.txt"} {
		if _, ok := names[hidden]; ok {
			t.Errorf("tree lists %s, which policy denies reading", hidden)
		}
	}
	if _, ok := wb.tree("app")[".env"]; ok {
		t.Error("tree lists app/.env, which policy denies reading")
	}
}

// .abhed under the workspace is the server's own state: users.json holds the
// password hashes. No rule has to be written for the viewer to leave it alone.
func TestWorkbenchSkipsRepositoryAndServerState(t *testing.T) {
	wb := newWorkbench(t, nil)
	wb.write(".git/config", "[remote]\n")
	wb.write(".abhed/users.json", "HASHES")
	wb.write("node_modules/left-pad/index.js", "x\n")
	wb.write("src/.git/HEAD", "ref\n")
	wb.write("src/a.go", "package a\n")
	wb.write("README.md", "# hi\n")

	names := wb.tree("")
	for _, skipped := range []string{".git", ".abhed", "node_modules"} {
		if _, ok := names[skipped]; ok {
			t.Errorf("tree lists %s", skipped)
		}
	}
	if e, ok := names["src"]; !ok || !e.Dir {
		t.Fatalf("tree is missing the src directory: %v", names)
	}
	if e, ok := names["README.md"]; !ok || e.Dir || e.Size != 5 || e.Path != "README.md" {
		t.Fatalf("unexpected README entry: %+v", e)
	}
	sub := wb.tree("src")
	if _, ok := sub[".git"]; ok {
		t.Error("tree lists a nested .git")
	}
	if e, ok := sub["a.go"]; !ok || e.Path != "src/a.go" {
		t.Fatalf("unexpected src listing: %v", sub)
	}

	for _, path := range []string{".git/config", ".abhed/users.json", "src/.git/HEAD"} {
		if rec, got := wb.file(path); rec.Code != http.StatusNotFound || got.Content != "" {
			t.Errorf("%s: got %d %s, want 404", path, rec.Code, rec.Body)
		}
	}
	if rec := wb.get("acme", "tree?path=.abhed"); rec.Code != http.StatusNotFound {
		t.Errorf("listing .abhed: got %d, want 404", rec.Code)
	}
}

func TestWorkbenchReportsBinaryWithoutContent(t *testing.T) {
	wb := newWorkbench(t, nil)
	wb.write("logo.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

	rec, got := wb.file("logo.png")
	if rec.Code != http.StatusOK || !got.Binary || got.Content != "" || got.Size == 0 {
		t.Fatalf("got %d %+v, want binary with no content", rec.Code, got)
	}
	if strings.Contains(rec.Body.String(), "IHDR") {
		t.Fatal("binary bytes were sent")
	}
}

func TestWorkbenchTruncatesLargeFiles(t *testing.T) {
	wb := newWorkbench(t, nil)
	// A three-byte character laid across the cap: it must be dropped whole,
	// not sent as a broken half.
	big := strings.Repeat("a", maxViewBytes-1) + "अ" + strings.Repeat("b", 4096)
	wb.write("big.txt", big)
	wb.write("small.txt", "small\n")

	rec, got := wb.file("big.txt")
	if rec.Code != http.StatusOK || !got.Truncated || got.Binary {
		t.Fatalf("got %d truncated=%v binary=%v", rec.Code, got.Truncated, got.Binary)
	}
	if got.Size != int64(len(big)) {
		t.Fatalf("size %d, want the full %d", got.Size, len(big))
	}
	if got.Content != strings.Repeat("a", maxViewBytes-1) {
		t.Fatalf("content is %d bytes ending %q", len(got.Content), got.Content[len(got.Content)-4:])
	}
	if _, small := wb.file("small.txt"); small.Truncated {
		t.Fatal("a small file was marked truncated")
	}
}

func TestWorkbenchTreeSaysWhenTruncated(t *testing.T) {
	wb := newWorkbench(t, nil)
	for i := 0; i < maxDirEntries+5; i++ {
		wb.write(filepath.Join("many", fmt.Sprintf("f%04d", i)), "")
	}
	rec := wb.get("acme", "tree?path=many")
	var out treeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || len(out.Entries) != maxDirEntries {
		t.Fatalf("got %d entries, truncated=%v", len(out.Entries), out.Truncated)
	}
}

// editingAdapter plays an agent making the tool calls it is given, one turn
// each, and then stopping.
type editingAdapter struct {
	mu    sync.Mutex
	turn  int
	calls [][]model.ToolCall
}

func (a *editingAdapter) Name() string { return "editing" }
func (a *editingAdapter) Profile() model.Profile {
	return model.Profile{Name: "editing", ContextWindow: 32000}
}
func (a *editingAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (a *editingAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch := make(chan model.Chunk, 8)
	if a.turn < len(a.calls) {
		for i := range a.calls[a.turn] {
			ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &a.calls[a.turn][i]}
		}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	a.turn++
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

func toolCall(id, name string, args map[string]string) model.ToolCall {
	raw, _ := json.Marshal(args)
	return model.ToolCall{ID: id, Name: name, Args: raw}
}

func TestWorkbenchChangesShowsWhatTheAgentEdited(t *testing.T) {
	adapter := &editingAdapter{}
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Permissions.Mode = "auto"
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "read(**/.env)")
	dir := t.TempDir()
	greet := filepath.Join(dir, "greet.txt")
	if err := os.WriteFile(greet, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter.calls = [][]model.ToolCall{
		{toolCall("c1", "read", map[string]string{"path": greet})},
		{toolCall("c2", "write", map[string]string{"path": greet, "content": "one\n2\nthree\n"})},
		{toolCall("c3", "write", map[string]string{"path": filepath.Join(dir, "new.txt"), "content": "fresh\n"})},
		{toolCall("c4", "write", map[string]string{"path": filepath.Join(dir, ".env"), "content": "API_KEY=hunter2\n"})},
	}
	s := New(Options{
		Workspace: dir, Config: cfg, Adapter: adapter,
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}),
	})
	h := s.Handler()

	ended := make(chan struct{})
	id, err := s.StartSession(context.Background(), StartSpec{
		Prompt: "edit", Tenant: "acme", User: "anonymous",
		OnEnd: func(string, error) { close(ended) },
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the scripted session did not end")
	}

	changes := func(tenant string) (int, changesResponse) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/sessions/"+id+"/changes", nil)
		req.Header.Set("X-Abhed-Tenant", tenant)
		h.ServeHTTP(rec, req)
		var out changesResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	_, got := changes("acme")
	if !got.Available || len(got.Files) != 3 {
		t.Fatalf("want three changed files, got %+v", got)
	}
	byPath := map[string]changedFile{}
	for _, f := range got.Files {
		byPath[filepath.Base(f.Path)] = f
	}

	edited := byPath["greet.txt"]
	want := "--- a/greet.txt\n+++ b/greet.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n"
	if edited.Path != "greet.txt" || edited.Status != "modified" || edited.Diff != want ||
		edited.Added != 1 || edited.Removed != 1 {
		t.Errorf("greet.txt: %+v", edited)
	}
	created2 := byPath["new.txt"]
	if created2.Status != "added" || created2.Added != 1 || !strings.Contains(created2.Diff, "@@ -0,0 +1,1 @@\n+fresh\n") {
		t.Errorf("new.txt: %+v", created2)
	}
	// The agent may write what policy forbids reading; the viewer still may
	// not read it back.
	denied := byPath[".env"]
	if denied.Diff != "" || denied.Note != "denied by policy" {
		t.Errorf(".env: %+v", denied)
	}
	if raw, _ := json.Marshal(got); bytes.Contains(raw, []byte("hunter2")) {
		t.Error("the changes view leaked a file policy denies reading")
	}

	if code, other := changes("other"); code != http.StatusNotFound || len(other.Files) != 0 {
		t.Errorf("cross-tenant changes: %d %+v", code, other)
	}
}
