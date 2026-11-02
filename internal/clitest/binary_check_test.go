package clitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildWith builds abhed with these -X flags, and never runs it.
func buildWith(t *testing.T, name string, flags []string) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), name)
	args := []string{"build", "-buildvcs=false", "-o", bin}
	if len(flags) > 0 {
		args = append(args, "-ldflags", strings.Join(flags, " "))
	}
	cmd := exec.Command("go", append(args, "./cmd/abhed")...) // #nosec G204 -- go build of this module
	cmd.Dir = root
	cmd.Env = withoutGOROOT(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, out)
	}
	return bin
}

// replaced is flags with the -X for key set to value, or dropped when value
// is "".
func replaced(flags []string, key, value string) []string {
	var out []string
	for i := 0; i < len(flags); i += 2 {
		if k, _, _ := strings.Cut(flags[i+1], "="); k == key {
			if value != "" {
				out = append(out, "-X", key+"="+value)
			}
			continue
		}
		out = append(out, flags[i], flags[i+1])
	}
	return out
}

// The harness's own binary passes both checks. Builds that could read this
// machine's managed files, or reach a real model service, are refused by
// their build information alone, so none of them is ever run.
func TestBinaryCheckRefusesABuildThatCouldReadTheMachine(t *testing.T) {
	if err := checkBinary(Binary(t)); err != nil {
		t.Fatalf("the harness's own binary: %v", err)
	}
	none := filepath.Join(t.TempDir(), "none")
	full := testLDFlags(none)
	short := "internal/managed"
	cases := map[string]struct {
		flags []string
		want  string
	}{
		"no flags": {nil, "testDirEnv"},
		// The linker ignores -X for a symbol it cannot find.
		"short package path":        {append(replaced(full, managedPkg+".testDirEnv", ""), "-X", short+".testDirEnv="+managedEnv), "testDirEnv"},
		"memory left at /etc/abhed": {replaced(full, managedPkg+".MemoryDir", ""), "MemoryDir"},
		"styles not cleaned":        {replaced(full, managedPkg+".StylesDir", "/etc//abhed/./styles"), "StylesDir"},
		"no defaults flags":         {replaced(replaced(full, defaultsPkg+".LocalModelURL", ""), defaultsPkg+".OllamaURL", ""), "LocalModelURL"},
		"defaults at Ollama":        {replaced(full, defaultsPkg+".OllamaURL", "http://127.0.0.1:11434/v1"), "OllamaURL"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bin := buildWith(t, "abhed", c.flags)
			err := checkBuildFlags(bin)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v; want a refusal naming %s", err, c.want)
			}
		})
	}
}

// The runtime half on its own: a binary whose hook reads another variable
// passes nothing to it from the harness, so its doctor reports no managed
// file and the marker check refuses it. Every managed path of this build is
// in a folder that does not exist, so running it reads nothing real.
func TestMarkerCheckRefusesABinaryThatIgnoresTheManagedDirectory(t *testing.T) {
	none := filepath.Join(t.TempDir(), "none")
	flags := replaced(testLDFlags(none), managedPkg+".testDirEnv", "ABHED_CLITEST_SOME_OTHER_DIR")
	bin := buildWith(t, "abhed", flags)
	if err := checkBuildFlags(bin); err == nil {
		t.Fatal("the build check passed a binary whose hook reads another variable")
	}
	if err := checkMarker(bin); err == nil || !strings.Contains(err.Error(), "did not take its managed configuration") {
		t.Fatalf("marker check: %v; want a refusal", err)
	}
	if err := checkMarker(Binary(t)); err != nil {
		t.Fatalf("marker check of the harness's own binary: %v", err)
	}
}

// The defaults check parses a URL as the HTTP client does: the scheme in any
// case, and the scheme's port when none is written.
func TestSafeDefault(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://127.0.0.1:9/v1": true, "http://localhost/v1": true, "https://example.test/v1": false,
		"http://localhost.:11434/v1": false, "http://127.1:11434/v1": false, "http://x.localhost:11434": false,
		"HTTP://localhost:11434/v1": false, "http://LOCALHOST:11434": false, "Https://[::1]:4000/v1": false,
		"http://127.0.0.1:11434/v1": false, "ftp://127.0.0.1:9": false, "": false, "localhost:11434": false,
	} {
		if got := safeDefault(raw); got != want {
			t.Errorf("safeDefault(%q) = %v, want %v", raw, got, want)
		}
	}
}
