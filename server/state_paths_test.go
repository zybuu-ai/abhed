package server

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every endpoint that reads or writes a workspace path refuses Abhed's own
// state, under any spelling a case-insensitive disk accepts and through any
// link planted in the workspace.
func TestEndpointsRefuseStateThroughLinksAndSpellings(t *testing.T) {
	h, id, ws := downloadServer(t)
	users := filepath.Join(ws, ".abhed", "users.json")
	target := users
	if _, err := os.Stat(filepath.Join(ws, ".ABHED", "users.json")); err == nil {
		target = filepath.Join(ws, ".ABHED", "users.json")
	}
	if err := os.Symlink(target, filepath.Join(ws, "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(ws, ".abhed"), filepath.Join(ws, "settings")); err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/"+id+path, nil))
		return rec
	}
	leaked := func(rec *httptest.ResponseRecorder) bool {
		return strings.Contains(rec.Body.String(), "$2a$10$secret") || strings.Contains(rec.Body.String(), "sk-live")
	}
	for _, p := range []string{
		"/file?path=notes.txt", "/file?path=.ABHED/users.json", "/file?path=.Abhed/config.json",
		"/file?path=settings/users.json", "/tree?path=settings", "/tree?path=.ABHED",
		"/original?path=notes.txt", "/download?path=notes.txt", "/download?path=settings/users.json",
		"/search?q=" + url.QueryEscape("secret"), "/search?q=" + url.QueryEscape("sk-live"),
	} {
		if rec := get(p); leaked(rec) || (rec.Code == http.StatusOK && !strings.HasPrefix(p, "/search") && !strings.HasPrefix(p, "/original")) {
			t.Errorf("%s = %d: %s", p, rec.Code, rec.Body)
		}
	}
	if rec := get("/tree?path="); strings.Contains(rec.Body.String(), `"settings"`) || strings.Contains(rec.Body.String(), `"notes.txt"`) {
		t.Errorf("the tree lists a link into the state: %s", rec.Body)
	}
	if got, _ := os.ReadFile(users); !strings.Contains(string(got), "$2a$10$secret") {
		t.Fatalf("the users file was changed: %s", got)
	}
}

// An upload is written into the workspace, where a planted link could send
// it onto Abhed's state.
func TestUploadRefusesALinkIntoTheState(t *testing.T) {
	h, id, ws := downloadServer(t)
	dir := filepath.Join(ws, uploadDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, ".abhed"), filepath.Join(dir, id)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "users.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(`{"intruder":{"hash":"x"}}`))
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/v1/sessions/"+id+"/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("the upload was written through the link: %s", rec.Body)
	}
	if ents, _ := os.ReadDir(filepath.Join(ws, ".abhed")); len(ents) != 2 {
		t.Fatalf("a file was planted in the state: %v", ents)
	}
}

// A link swapped in between the path check and the open must not be served:
// what was opened is judged, not only the path. The swap runs in a loop
// against download and the file viewer, and the hashes must never come back.
func TestSwappedLinkIsNeverServed(t *testing.T) {
	h, id, ws := downloadServer(t)
	decoyDir, decoyLink := filepath.Join(ws, "decoy-dir"), filepath.Join(ws, "decoy-link")
	if err := os.MkdirAll(decoyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyDir, "users.json"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, ".abhed"), decoyLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	decoy := filepath.Join(ws, "decoy")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(decoyDir, decoy)
			_ = os.Rename(decoy, decoyDir)
			_ = os.Rename(decoyLink, decoy)
			_ = os.Rename(decoy, decoyLink)
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 3000; i++ {
		for _, p := range []string{"/download?path=decoy/users.json", "/file?path=decoy/users.json"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/"+id+p, nil))
			if strings.Contains(rec.Body.String(), "$2a$10$secret") {
				t.Fatalf("%s served the users file on try %d", p, i)
			}
		}
	}
}

// A folder swapped for a link to outside the workspace leads neither download
// nor the file viewer out, and an upload is never written there.
func TestSwappedLinkNeverLeavesTheWorkspace(t *testing.T) {
	h, id, ws := downloadServer(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "notes.txt"), []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoyDir, decoyLink := filepath.Join(ws, "decoy-dir"), filepath.Join(ws, "decoy-link")
	if err := os.MkdirAll(decoyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyDir, "notes.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, decoyLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The upload folder is swapped the same way.
	uploads := filepath.Join(ws, uploadDirName)
	if err := os.MkdirAll(filepath.Join(uploads, "real-"+id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(uploads, "link-"+id)); err != nil {
		t.Fatal(err)
	}
	decoy, slot := filepath.Join(ws, "decoy"), filepath.Join(uploads, id)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(decoyDir, decoy)
			_ = os.Rename(decoy, decoyDir)
			_ = os.Rename(decoyLink, decoy)
			_ = os.Rename(decoy, decoyLink)
			_ = os.Rename(filepath.Join(uploads, "real-"+id), slot)
			_ = os.Rename(slot, filepath.Join(uploads, "real-"+id))
			_ = os.Rename(filepath.Join(uploads, "link-"+id), slot)
			_ = os.Rename(slot, filepath.Join(uploads, "link-"+id))
		}
	}()
	defer func() { close(stop); <-done }()
	upload := func() {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		fw, _ := mw.CreateFormFile("file", "planted.txt")
		_, _ = fw.Write([]byte("planted"))
		_ = mw.Close()
		req := httptest.NewRequest("POST", "/v1/sessions/"+id+"/upload", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	for i := 0; i < 2000; i++ {
		for _, p := range []string{"/download?path=decoy/notes.txt", "/file?path=decoy/notes.txt"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/"+id+p, nil))
			if strings.Contains(rec.Body.String(), "outside-secret") {
				t.Fatalf("%s left the workspace on try %d", p, i)
			}
		}
		if i%10 == 0 {
			upload()
		}
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("an upload was written outside the workspace: %d entries", len(ents))
	}
}

// A refused upload leaves the state exactly as it was: no folder is made
// through the link before the refusal, whichever way the link is planted.
func TestRefusedUploadLeavesTheStateUntouched(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, links := range map[string][][2]string{
		"to the state folder":  {{uploadDirName, ".ABHED"}},
		"through two links":    {{"sub/statelink", "../.abhed"}, {uploadDirName, "sub/statelink"}},
		"to a folder not made": {{uploadDirName, ".abhed/skills"}},
	} {
		t.Run(name, func(t *testing.T) {
			h, id, ws := downloadServer(t)
			for _, l := range links {
				target := l[1]
				if target == ".ABHED" {
					if _, err := os.Stat(filepath.Join(ws, ".ABHED")); err != nil {
						target = ".abhed" // a disk that tells case apart
					}
				}
				at := filepath.Join(ws, l[0])
				if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, at); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			before := snapshot(t, filepath.Join(ws, ".abhed"))
			body := &bytes.Buffer{}
			mw := multipart.NewWriter(body)
			fw, err := mw.CreateFormFile("file", "notes.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write([]byte("hello"))
			_ = mw.Close()
			req := httptest.NewRequest("POST", "/v1/sessions/"+id+"/upload", body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("upload through the link = %d: %s", rec.Code, rec.Body)
			}
			if after := snapshot(t, filepath.Join(ws, ".abhed")); after != before {
				t.Fatalf("a refused upload changed the state:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// snapshot lists every entry under dir with its kind, mode and content.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "%s %v", rel, info.Mode())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %q", data)
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
