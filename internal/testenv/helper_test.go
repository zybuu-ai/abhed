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

// needsOwnership skips where checkPrivate cannot read ownership, so a
// helper never reuses a folder.
func needsOwnership(t *testing.T) {
	t.Helper()
	if !ownFolderChecked {
		t.Skip("folder ownership is not checked on this system")
	}
}

// copyBinary copies this test binary into a folder of the test's own.
func copyBinary(t *testing.T) (dir, bin string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "testenv.test")
	if err := os.WriteFile(bin, data, 0o700); err != nil { // #nosec G306 -- an executable
		t.Fatal(err)
	}
	return dir, bin
}

// run runs bin in role and returns what it printed.
func run(t *testing.T, bin, role string, env []string) string {
	t.Helper()
	cmd := exec.Command(bin, "-test.run=^TestProbeRole$") // #nosec G204 -- a copy of this test binary
	cmd.Env = append(append([]string{}, env...), "ABHED_TESTENV_PROBE="+role)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe %s: %v\n%s", role, err, out)
	}
	return string(out)
}

// cleanEnv is this process's environment without the marker, the parent
// folder and this process's isolated home, which would name its parent; the
// home is a plain folder of the test's own.
func cleanEnv(t *testing.T) []string {
	t.Helper()
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, privateEnv+"=") && !strings.HasPrefix(e, Marker+"=") && !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	return append(env, "HOME="+t.TempDir())
}

// TestProbeRole is the probe's side: it says where its paths lead and what
// its Isolate made, and removes what it made, since it exits without Close.
func TestProbeRole(t *testing.T) {
	if os.Getenv("ABHED_TESTENV_PROBE") == "" {
		t.Skip("run by a probe")
	}
	os.Stdout.WriteString("config=" + managed.ConfigFile + "\nprivate=" + g.private + "\n")
	for _, d := range []string{g.private, g.home} {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
}

// A helper with the marker reuses the private folder its parent names and
// makes none of its own.
func TestHelperReusesTheParentsFolder(t *testing.T) {
	needsOwnership(t)
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	_, bin := copyBinary(t)
	out := run(t, bin, "helper", append(cleanEnv(t), Marker+"=1", privateEnv+"="+parent))
	if !strings.Contains(out, "config="+filepath.Join(resolved, "none", "config.json")+"\n") || !strings.Contains(out, "private=\n") {
		t.Fatalf("the helper did not reuse its parent's folder:\n%s", out)
	}
}

// A folder the parent names is used only if no other user can change it or
// swap it: one others may write in, one under a folder others may write in
// (which could be renamed away and replaced), or a relative path, is
// refused, and the helper makes a folder of its own. A link is followed once
// and the folder it leads to used, so repointing it later changes nothing.
func TestHelperRefusesAParentFolderThatIsNotPrivate(t *testing.T) {
	needsOwnership(t)
	base := t.TempDir()
	mk := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	open := filepath.Join(base, "open")
	mk(open, 0o777)
	readable := filepath.Join(base, "readable")
	mk(readable, 0o755)
	shared := filepath.Join(base, "shared")
	mk(shared, 0o777)
	under := filepath.Join(shared, "mine")
	mk(under, 0o700)
	private := filepath.Join(base, "private")
	mk(private, 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	_, bin := copyBinary(t)
	for name, parent := range map[string]string{"world-writable": open, "0755, not 0700": readable,
		"under a writable folder": under, "relative": "private"} {
		out := run(t, bin, "helper", append(cleanEnv(t), Marker+"=1", privateEnv+"="+parent))
		if strings.Contains(out, "none/config.json\nprivate=\n") || !strings.Contains(out, "private=/") {
			t.Fatalf("%s: the helper used %s rather than a folder of its own:\n%s", name, parent, out)
		}
	}
	resolved, err := filepath.EvalSymlinks(private)
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, bin, "helper", append(cleanEnv(t), Marker+"=1", privateEnv+"="+link))
	if !strings.Contains(out, "config="+filepath.Join(resolved, "none", "config.json")+"\n") {
		t.Fatalf("a link to a private folder: the paths do not lead to the folder it resolves to:\n%s", out)
	}
}

// Nothing beside the binary is trusted: a folder planted there, holding a
// managed file, is never where the managed paths lead, whether the binary's
// folder is private or others may write in it.
func TestPlantedFolderBesideTheBinaryIsNotUsed(t *testing.T) {
	for _, mode := range []os.FileMode{0o700, 0o777} {
		dir, bin := copyBinary(t)
		planted := filepath.Join(bin+".no-managed", "none")
		if err := os.MkdirAll(planted, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(planted, "config.json"), []byte(`{"planted":true}`), 0o644); err != nil { // #nosec G306 -- a stand-in
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		out := run(t, bin, "top", cleanEnv(t))
		if strings.Contains(out, ".no-managed") || !strings.Contains(out, "private=/") {
			t.Fatalf("mode %o: the managed paths lead beside the binary:\n%s", mode, out)
		}
	}
}

// When no stub could listen, the defaults lead to a closed port, and a dial
// there is a hit like a call to the stub, so a test that reaches the default
// endpoint still fails the run. Tests also use that closed port as an
// endpoint that refuses, so it is a hit only in fallback.
func TestFallbackDialIsAHit(t *testing.T) {
	was := g.fallback.Load()
	t.Cleanup(func() { g.fallback.Store(was) })
	dial := func() error {
		resp, err := http.Get("http://" + fallbackAddr + "/v1/models")
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if !was {
		before := len(g.Hits())
		_ = dial()
		if len(g.Hits()) != before {
			t.Fatalf("with a stub listening, a dial to %s was noted: %v", fallbackAddr, g.Hits())
		}
	}
	g.fallback.Store(true)
	n := len(g.Hits())
	if err := dial(); err == nil || !strings.Contains(err.Error(), "testenv") {
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

// A helper a sandbox runs keeps HOME but loses this package's variables. Its
// HOME, the home Isolate made inside its parent's private folder, names that
// folder, and the helper reuses it, making neither a folder nor a home of its
// own. A HOME of any other shape, or in a folder that is not private, does
// not count.
func TestSandboxedHelperFindsItsParentByHome(t *testing.T) {
	needsOwnership(t)
	base := t.TempDir()
	parent := filepath.Join(base, privatePrefix+"x")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, homeName), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	env := func(home string) []string {
		var out []string
		for _, e := range cleanEnv(t) {
			if !strings.HasPrefix(e, "HOME=") {
				out = append(out, e)
			}
		}
		return append(out, "HOME="+home)
	}
	_, bin := copyBinary(t)
	out := run(t, bin, "sandboxed", env(filepath.Join(parent, homeName)))
	if !strings.Contains(out, "config="+filepath.Join(resolved, "none", "config.json")+"\n") || !strings.Contains(out, "private=\n") {
		t.Fatalf("the helper did not find its parent by its home:\n%s", out)
	}
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(filepath.Join(other, homeName), 0o700); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(base, privatePrefix+"open")
	if err := os.MkdirAll(filepath.Join(open, homeName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, home := range map[string]string{"another folder": filepath.Join(other, homeName),
		"a folder that is not private": filepath.Join(open, homeName)} {
		if out := run(t, bin, "top", env(home)); !strings.Contains(out, "private=/") {
			t.Fatalf("%s: the binary took %s for its parent's:\n%s", name, home, out)
		}
	}
}

// A top-level binary's home is inside its private folder, so Close removes
// both and nothing is left in the temporary folder.
func TestIsolateHomeIsInsideThePrivateFolder(t *testing.T) {
	if g.home == "" || filepath.Dir(g.home) != g.resolved {
		t.Fatalf("home %q is not inside the private folder %q", g.home, g.resolved)
	}
	if h, _ := os.UserHomeDir(); h != g.home {
		t.Fatalf("HOME is %q, want %q", h, g.home)
	}
}
