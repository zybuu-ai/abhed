package witness

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Heads go to the file and the URL signed; read back, the record that still
// holds them passes, and one cut or rewritten since fails.
func TestWitnessCatchesACutAndARewrite(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posted = append(posted, r.Header.Get("Authorization")+" "+string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	w, err := Open(Config{File: filepath.Join(dir, "heads.jsonl"), URL: srv.URL, Token: "tok", KeyFile: filepath.Join(dir, "k", "witness.key")})
	if err != nil {
		t.Fatal(err)
	}
	w.Local("default", "s1", 3, 3, "h3")
	w.Postgres("default", "s2", 5, "p5")
	w.Close(5 * time.Second)
	if len(posted) != 2 || !strings.HasPrefix(posted[0], "Bearer tok {") {
		t.Fatalf("posted %q", posted)
	}
	f, _ := os.Open(filepath.Join(dir, "heads.jsonl"))
	defer f.Close()
	stmts, bad := Read(f)
	if len(bad) != 0 || len(stmts) != 2 {
		t.Fatalf("read %d statements, errors %v", len(stmts), bad)
	}
	record := map[string][]string{"s1": {"h1", "h2", "h3"}}
	at := func(session string, n int64) (string, bool, error) {
		l := record[session]
		if n < 1 || n > int64(len(l)) {
			return "", false, nil
		}
		return l[n-1], true, nil
	}
	key, err := PublicKey(filepath.Join(dir, "k", "witness.key"))
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := Check(stmts, key, "local", "default", at, nil); len(p) != 0 {
		t.Fatalf("an intact record failed: %+v", p)
	}
	// Statements forged under a key of their own verify alone, but not as this installation's.
	forger, err := Open(Config{File: filepath.Join(dir, "forged.jsonl"), KeyFile: filepath.Join(dir, "forger.key")})
	if err != nil {
		t.Fatal(err)
	}
	forger.Local("default", "s1", 2, 2, "h2")
	ff, _ := os.Open(filepath.Join(dir, "forged.jsonl"))
	defer ff.Close()
	forged, bad := Read(ff)
	if len(bad) != 0 || len(forged) != 1 {
		t.Fatalf("forged: %d, %v", len(forged), bad)
	}
	if p, _ := Check(forged, key, "local", "default", at, nil); len(p) != 1 || !strings.Contains(p[0].Reason, "not signed by this installation") {
		t.Fatalf("a statement under another key passed: %+v", p)
	}
	if _, err := Check(stmts, nil, "local", "default", at, nil); err == nil {
		t.Fatal("a check with no installation key ran")
	}
	if _, err := PublicKey(filepath.Join(dir, "none.key")); err == nil {
		t.Fatal("a missing key file gave a key")
	}
	record["s1"] = []string{"h1", "h2"} // cut back
	if p, _ := Check(stmts, key, "local", "default", at, nil); len(p) != 1 || !strings.Contains(p[0].Reason, "no longer has it") {
		t.Fatalf("a cut record passed: %+v", p)
	}
	record["s1"] = []string{"h1", "h2", "x3"} // rechained
	if p, _ := Check(stmts, key, "local", "default", at, nil); len(p) != 1 || !strings.Contains(p[0].Reason, "rewritten") {
		t.Fatalf("a rewritten record passed: %+v", p)
	}
	// A removal on the record accounts for a missing head; nothing else does.
	delete(record, "s1")
	removed := func(session string, line int64, hash string) (string, error) {
		if session == "s1" && line <= 3 {
			return "pruned", nil
		}
		return "", nil
	}
	if p, _ := Check(stmts, key, "local", "default", at, removed); len(p) != 1 || !p[0].Removed || Failed(p) != 0 {
		t.Fatalf("a pruned session's heads failed: %+v", p)
	}
	if p, _ := Check(stmts, key, "local", "default", at, nil); Failed(p) != 1 {
		t.Fatalf("a missing session passed: %+v", p)
	}
}

// A statement changed after it was signed does not verify.
func TestWitnessStatementsAreSigned(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Config{File: filepath.Join(dir, "heads.jsonl"), KeyFile: filepath.Join(dir, "witness.key")})
	if err != nil {
		t.Fatal(err)
	}
	w.Local("default", "s1", 1, 1, "h1")
	b, _ := os.ReadFile(filepath.Join(dir, "heads.jsonl"))
	var s Statement
	_ = json.Unmarshal(b, &s)
	if s.Verify() != nil {
		t.Fatal("a genuine statement did not verify")
	}
	s.Hash = "forged"
	if s.Verify() == nil {
		t.Fatal("an altered statement verified")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "witness.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
}

// A link where the witness file should be is refused.
func TestWitnessRefusesALink(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "elsewhere"), nil, 0o600)
	_ = os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, "heads.jsonl"))
	if _, err := Open(Config{File: filepath.Join(dir, "heads.jsonl"), KeyFile: filepath.Join(dir, "witness.key")}); err == nil {
		t.Fatal("a link was taken as the witness file")
	}
}

// A head that does not reach the URL or the file is reported as it fails,
// not only at close.
func TestWitnessReportsFailuresAsTheyHappen(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	got := make(chan error, 4)
	file := filepath.Join(dir, "heads.jsonl")
	w, err := Open(Config{File: file, URL: srv.URL, KeyFile: filepath.Join(dir, "witness.key"),
		OnError: func(err error) { got <- err }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(time.Second)
	w.Postgres("t", "s1", 1, "h")
	select {
	case err := <-got:
		if !strings.Contains(err.Error(), "503") {
			t.Fatalf("reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failed post was not reported before close")
	}
	// The file goes away (a share unmounted): reported at once, from the writer.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	w.Postgres("t", "s1", 2, "h2")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-got:
			if !strings.Contains(err.Error(), "503") {
				return
			}
		case <-deadline:
			t.Fatal("the failed append was not reported")
		}
	}
}

// A delivery error names the witness without a credential its URL carries.
func TestWitnessErrorHidesURLCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	for _, base := range []string{srv.URL, dead.URL} {
		u := strings.Replace(base, "://", "://alice:hunter2@", 1) + "/heads?token=sekrit#frag"
		w, err := Open(Config{URL: u, KeyFile: filepath.Join(t.TempDir(), "witness.key")})
		if err != nil {
			t.Fatal(err)
		}
		w.Local("default", "s1", 1, 1, "h1")
		w.Close(5 * time.Second)
		select {
		case err := <-w.Errors():
			for _, secret := range []string{"hunter2", "alice", "sekrit", "frag"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error shows %q: %v", secret, err)
				}
			}
			if !strings.Contains(err.Error(), "/heads") {
				t.Errorf("the error does not name the witness: %v", err)
			}
		default:
			t.Fatalf("no error from %s", base)
		}
	}
}
