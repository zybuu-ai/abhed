//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingEngine is a podman on PATH alone that logs each call and answers
// info as an engine without gVisor does.
func countingEngine(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" +
		"case \"$1\" in\n" +
		"--version) echo 'podman version 5.2.2' ;;\n" +
		"info) printf 'host:\\n  ociRuntime:\\n    name: crun\\n' ;;\n" +
		"*) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil { // #nosec G306 -- a stub engine that must run
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return log
}

// infoCalls counts the info calls in an engine's log.
func infoCalls(t *testing.T, log string) int {
	t.Helper()
	b, _ := os.ReadFile(log)
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "info") {
			n++
		}
	}
	return n
}

// Choosing a tier asks the engine what it is once per process, however many
// sessions choose one: each call over a podman machine's ssh costs the VM.
func TestSelectProbesEngineOnce(t *testing.T) {
	resetEngineProbes()
	t.Cleanup(resetEngineProbes)
	log := countingEngine(t)
	ws := t.TempDir()
	for range 2 {
		sb, err := Select(Policy{Workspace: ws, MinTier: TierContainer})
		if err != nil {
			t.Fatal(err)
		}
		if sb.Tier() != TierContainer {
			t.Fatalf("tier %s, want container", sb.Tier())
		}
	}
	if n := infoCalls(t, log); n != 1 {
		b, _ := os.ReadFile(log)
		t.Fatalf("%d info calls for two selections, want 1:\n%s", n, b)
	}
}

// gVisor is found in the one info: Docker's Runtimes line, or Podman's
// default runtime.
func TestInfoNamesRunsc(t *testing.T) {
	for info, want := range map[string]bool{
		" Runtimes: io.containerd.runc.v2 runc runsc\n Default Runtime: runc\n": true,
		" Runtimes: io.containerd.runc.v2 runc\n Default Runtime: runc\n":       false,
		"host:\n  ociRuntime:\n    name: runsc\n    path: /usr/bin/runsc\n":     true,
		"host:\n  ociRuntime:\n    name: crun\n    path: /usr/bin/crun\n":       false,
		"host:\n  hostname: runsc\n":                                            false,
	} {
		if got := infoHasRunsc([]byte(info)); got != want {
			t.Errorf("infoHasRunsc(%q) = %v, want %v", info, got, want)
		}
	}
}
