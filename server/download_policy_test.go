package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// downloadServer has a workspace holding the server's own state and a file a
// deny rule covers, and a session whose owner asks to download them.
func downloadServer(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "read(**/.env)", "read(**/secrets/**)")
	ws := t.TempDir()
	s := New(Options{Workspace: ws, Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Glob{})})
	for path, body := range map[string]string{
		".abhed/users.json":  `{"alice":{"hash":"$2a$10$secret"}}`,
		".abhed/config.json": `{"model":{"api_key":"sk-live"}}`,
		".env":               "DATABASE_PASSWORD=hunter2",
		"secrets/key.pem":    "-----BEGIN PRIVATE KEY-----",
		"report.txt":         "quarterly numbers",
	} {
		full := filepath.Join(ws, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi"}`)))
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	time.Sleep(150 * time.Millisecond)
	return h, created.SessionID, ws
}

// A signed-in user could download the server's own state — the password
// hashes in .abhed/users.json — and any file a deny rule kept from the agent,
// because the download path checked only that the file was inside the
// workspace.
func TestDownloadRefusesServerStateAndDeniedFiles(t *testing.T) {
	h, id, ws := downloadServer(t)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		q := "/v1/sessions/" + id + "/download?path=" + path
		h.ServeHTTP(rec, httptest.NewRequest("GET", q, nil))
		return rec
	}

	for _, path := range []string{
		".abhed/users.json", ".abhed/config.json", "./.abhed/users.json",
		filepath.Join(ws, ".abhed", "users.json"),
		".env", "secrets/key.pem",
	} {
		rec := get(path)
		if rec.Code == http.StatusOK {
			t.Errorf("download of %s returned 200 with %q", path, rec.Body.String())
		}
		for _, leak := range []string{"$2a$10$secret", "sk-live", "hunter2", "PRIVATE KEY"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("download of %s leaked %q", path, leak)
			}
		}
	}

	if rec := get("report.txt"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "quarterly") {
		t.Fatalf("an ordinary file stopped downloading: %d", rec.Code)
	}
}

// A link inside the workspace must not be a way round either check.
func TestDownloadJudgesASymlinkByItsTarget(t *testing.T) {
	h, id, ws := downloadServer(t)
	if err := os.Symlink(filepath.Join(ws, ".abhed", "users.json"), filepath.Join(ws, "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/"+id+"/download?path=notes.txt", nil))
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "$2a$10$secret") {
		t.Fatalf("a link to the password file downloaded it: %d", rec.Code)
	}
}
