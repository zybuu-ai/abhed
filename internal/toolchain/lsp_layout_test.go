package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/lsprelay"
)

// The start script finds the TypeScript Abhed installs with
// typescript-language-server: the toolchain's own layout, a sh wrapper in
// bin and the packages in lib/node_modules, as installNPM lays them out.
func TestTSServerLookupFindsAbhedsLayout(t *testing.T) {
	top := t.TempDir()
	mods := filepath.Join(top, "lib", "node_modules")
	cli := filepath.Join(mods, "typescript-language-server", "lib", "cli.mjs")
	ts := filepath.Join(mods, "typescript", "lib", "tsserver.js")
	for _, f := range []string{cli, ts} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(top, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "bin", "typescript-language-server"), []byte(nodeWrapper("typescript-language-server", "lib/cli.mjs")), 0o755); err != nil {
		t.Fatal(err)
	}
	// node stands in: it says what it was asked to run.
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "node"), []byte("#!/bin/sh\necho \"node $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	realTS, _ := filepath.EvalSymlinks(ts)
	for _, shell := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(shell); err != nil {
			continue
		}
		cmd := exec.Command(shell, "-c", lsprelay.Script([]string{"typescript-language-server", "--stdio"}))
		cmd.Env = []string{"PATH=" + filepath.Join(top, "bin") + ":" + stub + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
		var so, se strings.Builder
		cmd.Stdout, cmd.Stderr = &so, &se
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v %q", shell, err, se.String())
		}
		if !strings.Contains(se.String(), lsprelay.TSServerMarker+realTS) || !strings.Contains(so.String(), "lib/node_modules/typescript-language-server/lib/cli.mjs --stdio") {
			t.Fatalf("%s: stdout %q stderr %q", shell, so.String(), se.String())
		}
	}
}
