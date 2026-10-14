package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestUploadRequiresOwnership pins the fix for a cross-session write.
//
// The upload handler took the session id straight from the path and wrote the
// file without checking that the caller owns that session — unlike download,
// stream and delete, which all call mayAccess. Any authenticated account could
// therefore POST a file (up to the size limit) into another user's session
// directory, where the victim's agent reads it as (untrusted) input: a
// cross-session prompt-injection delivery channel. Ownership must be enforced,
// failing closed with 404 exactly as the sibling routes do.
func TestUploadRequiresOwnership(t *testing.T) {
	s := proxyServer(t)

	mk := func(user string) string {
		req := httptest.NewRequest("POST", "/v1/sessions",
			strings.NewReader(`{"prompt":"hi"}`))
		req.Header.Set("X-Abhed-User", user)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var out struct {
			SessionID string `json:"session_id"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.SessionID
	}

	upload := func(user, id string) int {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		fw, err := mw.CreateFormFile("file", "note.txt")
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte("planted content"))
		mw.Close()

		req := httptest.NewRequest("POST", "/v1/sessions/"+id+"/upload", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-Abhed-User", user)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	sid := mk("alice")

	// bob must not be able to plant a file in alice's session.
	if code := upload("bob", sid); code != http.StatusNotFound {
		t.Errorf("bob uploaded into alice's session: got %d, want 404", code)
	}
	// alice can still upload to a session she owns.
	if code := upload("alice", sid); code != http.StatusOK {
		t.Errorf("owner could not upload to their own session: got %d, want 200", code)
	}
}

// The pre-session staging path (POST /v1/uploads, or a "new" id) has no session
// to own yet, so it must keep working for any caller: the file lands under a
// freshly generated staged- id, never one the caller supplied.
func TestStagedUploadNeedsNoOwnership(t *testing.T) {
	s := proxyServer(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("hello"))
	mw.Close()

	req := httptest.NewRequest("POST", "/v1/uploads", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Abhed-User", "nobody-in-particular")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("staged upload was refused: got %d, want 200", rec.Code)
	}
}

// The folder an upload lands in does not name the session, so another account
// listing the shared uploads folder learns no one's session id; two uploads
// to one session get folders of their own.
func TestUploadFolderDoesNotNameTheSession(t *testing.T) {
	s := proxyServer(t)
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi"}`))
	req.Header.Set("X-Abhed-User", "alice")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var created struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	var paths []string
	for i := 0; i < 2; i++ {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		fw, _ := mw.CreateFormFile("file", "note.txt")
		fw.Write([]byte("hello"))
		mw.Close()
		req := httptest.NewRequest("POST", "/v1/sessions/"+created.SessionID+"/upload", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-Abhed-User", "alice")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var out uploadResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("upload: %d %s", rec.Code, rec.Body)
		}
		if strings.Contains(out.Path, created.SessionID) {
			t.Fatalf("the upload's path names the session: %s", out.Path)
		}
		paths = append(paths, filepath.Dir(out.Path))
	}
	if paths[0] == paths[1] {
		t.Fatalf("two uploads shared one folder: %s", paths[0])
	}
}
