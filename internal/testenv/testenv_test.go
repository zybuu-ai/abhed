package testenv

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/defaults"
	"github.com/zybuu-ai/abhed/internal/managed"
)

var g = Isolate()

func TestMain(m *testing.M) {
	code := m.Run()
	// The probes below reach the stub on purpose.
	g.mu.Lock()
	g.hits = nil
	g.mu.Unlock()
	os.Exit(g.Close(code))
}

func TestReal(t *testing.T) {
	for addr, want := range map[string]bool{"localhost:11434": true, "127.0.0.1:11434": true, "[::1]:4000": true,
		"0.0.0.0:11434": true, "127.0.0.1:8867": false, "10.0.0.2:11434": false, "example.com:4000": false, "bad": false} {
		if Real(addr) != want {
			t.Errorf("Real(%q) = %v", addr, !want)
		}
	}
}

// The default endpoints lead to the stub, which notes the call; a dial to a
// real service is refused and noted; home and managed files are elsewhere.
func TestIsolateGuards(t *testing.T) {
	if strings.Contains(defaults.LocalModelURL, "11434") || strings.Contains(defaults.OllamaURL, "11434") {
		t.Fatalf("defaults %s %s", defaults.LocalModelURL, defaults.OllamaURL)
	}
	if !strings.HasPrefix(managed.ConfigFile, os.TempDir()) {
		t.Fatalf("managed %s", managed.ConfigFile)
	}
	if h, _ := os.UserHomeDir(); h == g.OrigHome || os.Getenv("ABHED_MODEL") != "" {
		t.Fatalf("home %s", h)
	}
	resp, err := http.Post(defaults.LocalModelURL+"/chat/completions", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stub: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	if _, err := http.Get("http://127.0.0.1:11434/v1/models"); err == nil || !strings.Contains(err.Error(), "testenv") {
		t.Fatalf("a dial to 11434 was let through: %v", err)
	}
	if n := len(g.Hits()); n != 2 {
		t.Fatalf("hits %v", g.Hits())
	}
}

// Every package whose tests load a configuration or build an engine keeps
// them off this machine: its TestMain goes through this package or clitest.
func TestEveryEnginePackageIsIsolated(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	loads := regexp.MustCompile(`config\.(Load|LoadWith|Default)\(|abhed\.New\(|\bMain\(\[\]string|newSessRig|buildCPRig|manualBench`)
	need, isolated := map[string]bool{}, map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is left out
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(p) // #nosec G304 -- this module's own test files
		if err != nil {
			return nil //nolint:nilerr // left out
		}
		dir := filepath.Dir(p)
		if loads.Match(data) {
			need[dir] = true
		}
		if strings.Contains(string(data), "func TestMain(") && (strings.Contains(string(data), "testenv.") || strings.Contains(string(data), "clitest.Main") || strings.Contains(string(data), "Main(m)")) {
			isolated[dir] = true
		}
		return nil
	})
	for dir := range need {
		if !isolated[dir] && dir != filepath.Join(root, "internal", "testenv") {
			rel, _ := filepath.Rel(root, dir)
			t.Errorf("%s loads a configuration or builds an engine in its tests but has no TestMain through internal/testenv", rel)
		}
	}
}
