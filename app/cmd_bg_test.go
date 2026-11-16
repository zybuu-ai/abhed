package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/bgjob"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/bgcli"
	"github.com/zybuu-ai/abhed/profiles"
)

// The read rules a snapshot follows are the repository root's, as a session
// there would have them, trusted or not: a deny rule at the root withholds
// its file, and the built-in rules withhold a .env.
func TestBackgroundSnapshotRulesAreTheRepositoryRoots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(root, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".abhed", "config.json"), []byte(`{"permissions":{"deny":["read(private/**)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, trust := range []config.TrustChoice{config.TrustGranted, config.TrustRefused, ""} {
		withheld, err := bgRulesFor(trust)(root)
		if err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string]bool{
			"private/notes.md": true, "sub/private/x": false, ".env": true, "pkg/a.go": false,
		} {
			if got := withheld(filepath.Join(root, path)); got != want {
				t.Errorf("trust %q: %s withheld = %v, want %v", trust, path, got, want)
			}
		}
	}
}

// From a folder inside the repository, the snapshot still follows the
// root's deny rule: the file it names is not sent.
func TestBackgroundSnapshotFromASubfolderFollowsTheRootsRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, _ := filepath.EvalSymlinks(t.TempDir())
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for name, body := range map[string]string{
		".abhed/config.json": `{"permissions":{"deny":["read(private/**)"]}}`,
		"private/notes.md":   "withheld\n", "sub/a.go": "package sub\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "x")

	var got []byte
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case bgjob.PathSnapshots:
			got, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"snap_1"}`)
		case bgjob.PathJobs:
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"id":"bg_1","session_id":"s-1","state":"running"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hs.Close()
	prof := filepath.Join(t.TempDir(), "profiles.json")
	if err := profiles.Save(prof, &profiles.File{Version: 1, Current: "p", Profiles: map[string]*profiles.Profile{
		"p": {URL: hs.URL, Token: "tok", TokenType: "Bearer"}}}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := bgcli.Main(bgcli.Env{ProfilesPath: prof, Out: &out, Err: &errOut, Workspace: filepath.Join(root, "sub"),
		RulesFor: bgRulesFor("")}, []string{"start", "-y", "go"})
	if code != 0 {
		t.Fatalf("start: %d %s", code, errOut.String())
	}
	zr, err := gzip.NewReader(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "sub/a.go" || !strings.Contains(errOut.String(), "private/notes.md") {
		t.Fatalf("sent %v; start said %s", names, errOut.String())
	}
}
