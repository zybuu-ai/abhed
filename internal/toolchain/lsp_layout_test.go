package toolchain

import (
	"errors"
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
		cmd := exec.Command(shell, "-c", lsprelay.Script([]string{"typescript-language-server", "--stdio"}, ""))
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

// Abhed's toolchain cache is under the home folder, which the start script
// and the engine otherwise refuse: the cache is exempt, on the process tier
// (the sandbox's HOME is the person's) and the container tier (HOME is the
// sandbox's own, the cache mounted at its host path), while a TypeScript in
// the sandbox's home or temporary folder outside it is still refused.
func TestTSServerInTheToolchainCacheUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	root, err := DefaultRoot()
	if err != nil || !strings.HasPrefix(root, home) {
		t.Fatalf("root %q %v", root, err)
	}
	layout := func(top string) string {
		mods := filepath.Join(top, "lib", "node_modules")
		ts := filepath.Join(mods, "typescript", "lib", "tsserver.js")
		for _, f := range []string{filepath.Join(mods, "typescript-language-server", "lib", "cli.mjs"), ts} {
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
		real, _ := filepath.EvalSymlinks(ts)
		return real
	}
	top := filepath.Join(root, "darwin-arm64", "node", "22.20.0")
	ts := layout(top)
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "node"), []byte("#!/bin/sh\necho \"node $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(bin, toolchainRoot string, env ...string) (string, int) {
		cmd := exec.Command("/bin/sh", "-c", lsprelay.Script([]string{"typescript-language-server", "--stdio"}, toolchainRoot))
		cmd.Env = append([]string{"PATH=" + bin + ":" + stub + ":/usr/bin:/bin"}, env...)
		var se strings.Builder
		cmd.Stderr = &se
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return se.String(), code
	}
	bin := filepath.Join(top, "bin")
	sandboxHome := t.TempDir()
	for name, env := range map[string][]string{
		"process":   {"HOME=" + home, "TMPDIR=" + t.TempDir()},
		"container": {"HOME=" + sandboxHome},
	} {
		if se, code := run(bin, root, env...); code != 0 || !strings.Contains(se, lsprelay.TSServerMarker+ts) {
			t.Errorf("%s tier: the cache's TypeScript was not found: %d %q", name, code, se)
		}
		if lsprelay.TSServerRefused(ts, []string{t.TempDir()}, home, root) {
			t.Errorf("%s tier: the engine refused the cache's TypeScript", name)
		}
	}
	// Without the exemption, under HOME is refused, by the script and the engine.
	if _, code := run(bin, "", "HOME="+home); code != lsprelay.ExitNoTypeScript {
		t.Errorf("a TypeScript under HOME outside the cache was taken: %d", code)
	}
	if !lsprelay.TSServerRefused(ts, nil, home, "") {
		t.Error("the engine took a TypeScript under home with no cache named")
	}
	// One in the sandbox's temporary folder, or a session folder, is refused.
	tmp := t.TempDir()
	planted := layout(filepath.Join(tmp, "npm"))
	if _, code := run(filepath.Join(tmp, "npm", "bin"), root, "HOME="+sandboxHome, "TMPDIR="+tmp); code != lsprelay.ExitNoTypeScript {
		t.Errorf("a TypeScript in TMPDIR was taken: %d", code)
	}
	if !lsprelay.TSServerRefused(planted, []string{tmp}, home, root) {
		t.Error("the engine took a TypeScript in a session folder")
	}
}
