package testenv

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// probe runs a copy of this test binary from a folder others may write in,
// so the copy cannot keep its managed paths beside itself, as role, and
// returns what it printed.
func probe(t *testing.T, role string, env []string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o775); err != nil { // #nosec G302 -- group-writable on purpose: not private
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "testenv.test")
	if err := os.WriteFile(bin, data, 0o700); err != nil { // #nosec G306 -- an executable
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestProbeRole$") // #nosec G204 -- a copy of this test binary
	cmd.Env = append(append([]string{}, env...), "ABHED_TESTENV_PROBE="+role)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe %s: %v\n%s", role, err, out)
	}
	return string(out)
}

// TestProbeRole is the probe's side: it says where its paths lead and what
// its Isolate made.
func TestProbeRole(t *testing.T) {
	if os.Getenv("ABHED_TESTENV_PROBE") == "" {
		t.Skip("run by probe")
	}
	os.Stdout.WriteString("config=" + managed.ConfigFile + "\nprivate=" + g.private + "\n")
	// The probe exits without Close.
	for _, d := range []string{g.private, g.home} {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
}

// A test binary go test built keeps its managed paths beside itself, in the
// folder only this user can write: nothing is made, so a helper, which
// usually exits from inside a test, leaves nothing behind.
func TestIsolateMakesNothingBesideAPrivateBinary(t *testing.T) {
	exe, _ := os.Executable()
	if !privateDir(filepath.Dir(exe)) {
		t.Skip("this binary's folder is not private here")
	}
	if want := filepath.Join(exe+".no-managed", "none", "config.json"); managed.ConfigFile != want || g.private != "" {
		t.Fatalf("managed %s, private %q; want %s and nothing made", managed.ConfigFile, g.private, want)
	}
	if _, err := os.Stat(exe + ".no-managed"); !os.IsNotExist(err) {
		t.Fatalf("something was made beside the binary: %v", err)
	}
}

// Where the binary's folder is not private, a helper with the marker reuses
// the private folder its parent names and makes none of its own; one without
// a parent makes its own.
func TestHelperReusesTheParentsFolder(t *testing.T) {
	parent := t.TempDir()
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, privateEnv+"=") && !strings.HasPrefix(e, Marker+"=") {
			env = append(env, e)
		}
	}
	out := probe(t, "helper", append(env, Marker+"=1", privateEnv+"="+parent))
	if !strings.Contains(out, "config="+filepath.Join(parent, "none", "config.json")+"\n") || !strings.Contains(out, "private=\n") {
		t.Fatalf("the helper did not reuse its parent's folder:\n%s", out)
	}
	out = probe(t, "parent", env)
	if !strings.Contains(out, "private=/") {
		t.Fatalf("a binary with no parent folder made none of its own:\n%s", out)
	}
}

// When no stub could listen, the defaults lead to a closed port, and a dial
// there is a hit like a call to the stub, so a test that reaches the default
// endpoint still fails the run.
func TestFallbackDialIsAHit(t *testing.T) {
	// Only while the guard is in fallback: tests also use the closed port
	// as an endpoint that refuses.
	before := len(g.Hits())
	if resp, err := http.Get("http://" + fallbackAddr + "/v1/models"); err == nil {
		_ = resp.Body.Close()
	}
	if len(g.Hits()) != before {
		t.Fatalf("with a stub listening, a dial to %s was noted: %v", fallbackAddr, g.Hits())
	}
	g.fallback.Store(true)
	t.Cleanup(func() { g.fallback.Store(false) })
	n := len(g.Hits())
	resp, err := http.Get("http://" + fallbackAddr + "/v1/models")
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "testenv") {
		t.Fatalf("a dial to the fallback was let through: %v", err)
	}
	hits := g.Hits()
	if len(hits) != n+1 || !strings.Contains(hits[n], "no stub could listen") {
		t.Fatalf("hits %v", hits)
	}
	g.mu.Lock()
	g.hits = g.hits[:n]
	g.mu.Unlock()
}
