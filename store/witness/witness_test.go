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
	if p, _ := Check(stmts, "local", "default", at); len(p) != 0 {
		t.Fatalf("an intact record failed: %+v", p)
	}
	record["s1"] = []string{"h1", "h2"} // cut back
	if p, _ := Check(stmts, "local", "default", at); len(p) != 1 || !strings.Contains(p[0].Reason, "no longer has it") {
		t.Fatalf("a cut record passed: %+v", p)
	}
	record["s1"] = []string{"h1", "h2", "x3"} // rechained
	if p, _ := Check(stmts, "local", "default", at); len(p) != 1 || !strings.Contains(p[0].Reason, "rewritten") {
		t.Fatalf("a rewritten record passed: %+v", p)
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
