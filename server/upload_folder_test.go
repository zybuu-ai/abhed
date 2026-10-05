package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// dropFile posts one file to the session's upload route, into dir when it is not nil.
func (wb *workbench) dropFile(tenant string, dir *string, name string, data []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if dir != nil {
		_ = mw.WriteField("dir", *dir)
	}
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions/"+wb.session+"/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Abhed-Tenant", tenant)
	wb.h.ServeHTTP(rec, req)
	return rec
}

func ptr(s string) *string { return &s }

// A file dropped on a folder lands there under its own name, as the person's
// recorded write; a name already there is refused and left as it was.
func TestUploadIntoAFolderIsThePersonsRecordedWrite(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("docs/keep.md", "keep\n")
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16))

	rec := wb.dropFile("acme", ptr("docs"), "../diagram.png", png)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var out uploadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Path != "docs/diagram.png" || !out.Image {
		t.Fatalf("the reply: %+v", out)
	}
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "docs/diagram.png")); !bytes.Equal(got, png) {
		t.Fatalf("the file on disk: %q", got)
	}

	var requested, observed bool
	for _, e := range wb.events() {
		p := string(e.Payload)
		if e.Type == agent.EvActionRequested && e.Actor == agent.ActorUser && strings.Contains(p, `"via":"upload"`) && strings.Contains(p, "diagram.png") {
			requested = true
		}
		if e.Type == agent.EvObservation && strings.Contains(p, "uploaded 24 bytes to docs/diagram.png") {
			observed = true
		}
	}
	if !requested || !observed {
		t.Fatalf("the upload is not in the record as the person's write: requested %v, observed %v", requested, observed)
	}

	if rec := wb.dropFile("acme", ptr("docs"), "keep.md", []byte("replaced")); rec.Code != http.StatusConflict {
		t.Fatalf("a name already there: %d %s", rec.Code, rec.Body)
	}
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "docs/keep.md")); string(got) != "keep\n" {
		t.Fatalf("an existing file was replaced: %q", got)
	}
	// The workspace root is a folder too.
	if rec := wb.dropFile("acme", ptr(""), "top.txt", []byte("hi")); rec.Code != http.StatusOK {
		t.Fatalf("upload to the root: %d %s", rec.Code, rec.Body)
	}
	// Without a folder, the composer's upload still goes to the session's upload folder.
	rec = wb.dropFile("acme", nil, "note.txt", []byte("hi"))
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || !strings.Contains(out.Path, filepath.Join(uploadDirName, wb.session)) {
		t.Fatalf("composer upload: %d %+v", rec.Code, out)
	}
}

// A folder upload is held to the rules a save meets: write and read rules,
// the server's state, paths outside the workspace and another tenant's session.
func TestUploadIntoAFolderIsHeldToTheRules(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "write(**/locked/**)", "read(**/secret/**)")
	})
	wb.write("locked/a.txt", "a")
	wb.write("secret/a.txt", "a")
	wb.write(".abhed/x.json", "{}")
	if err := os.Symlink(filepath.Join(wb.workspace, "locked"), filepath.Join(wb.workspace, "alias")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		dir, name string
		want      int
	}{
		{"locked", "b.txt", http.StatusForbidden},
		{"alias", "b.txt", http.StatusForbidden}, // a link into a write-denied folder
		{"secret", "b.txt", http.StatusForbidden},
		{".abhed", "b.txt", http.StatusNotFound},
		{"../", "b.txt", http.StatusNotFound},
		{"nowhere", "b.txt", http.StatusNotFound},
	} {
		if rec := wb.dropFile("acme", ptr(c.dir), c.name, []byte("x")); rec.Code != c.want {
			t.Errorf("upload into %q: %d %s, want %d", c.dir, rec.Code, rec.Body, c.want)
		}
	}
	for _, p := range []string{"locked/b.txt", "secret/b.txt", ".abhed/b.txt", "../b.txt"} {
		if _, err := os.Lstat(filepath.Join(wb.workspace, p)); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	denied := 0
	for _, e := range wb.events() {
		if e.Type == agent.EvActionDenied {
			denied++
		}
	}
	if denied < 2 {
		t.Fatalf("a write rule's refusal of an upload is not recorded: %d denials", denied)
	}
	if rec := wb.dropFile("other", ptr(""), "b.txt", []byte("x")); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant's upload: %d %s", rec.Code, rec.Body)
	}
}

// A server whose sessions have no write tool takes no upload into a folder:
// the upload is a write, and a save would be refused there too.
func TestUploadIntoAFolderNeedsTheWriteTool(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	dir := t.TempDir()
	s := New(Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	wb := &workbench{t: t, s: s, h: s.Handler(), workspace: dir}
	wb.write("docs/a.txt", "a")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"workbench":true}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	var created createResponse
	if rec.Code/100 != 2 || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("create the session: %d %s", rec.Code, rec.Body.String())
	}
	wb.session = created.SessionID
	if rec := wb.dropFile("acme", ptr("docs"), "b.txt", []byte("x")); rec.Code != http.StatusForbidden {
		t.Fatalf("upload with no write tool: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Lstat(filepath.Join(wb.workspace, "docs/b.txt")); err == nil {
		t.Fatal("the file was written")
	}
}
