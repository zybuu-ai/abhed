package clitest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The binary a test runs is marked as under test, so it finds, starts,
// installs and pulls no real Ollama: not through PATH (where a stand-in is
// first anyway), the standard install folders or the macOS app.
func TestRunsReachNoRealOllama(t *testing.T) {
	h := piped(t, Opts{Args: []string{"ollama", "status", "-json", "local"},
		UserConfig: `{"sandbox":{"min_tier":"none"},"model":{"default":"local","providers":{"local":{"type":"ollama","base_url":"http://127.0.0.1:9/v1","model":"gemma4:26b"}}}}`})
	out := string(h.Output())
	if !strings.Contains(out, `"installed": false`) || strings.Contains(out, "/opt/homebrew") || strings.Contains(out, "Ollama.app") {
		t.Fatalf("the run looked at this machine's Ollama:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.Home(), "ollama-ran")); err == nil {
		t.Fatal("the run ran an ollama program")
	}
	env := strings.Join((&run{}).env(), "\n")
	if !strings.Contains(env, "ABHED_TESTENV=1") {
		t.Fatalf("the run's environment is not marked as under test:\n%s", env)
	}
}
