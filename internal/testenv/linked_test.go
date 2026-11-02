package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const managedPkg = "github.com/zybuu-ai/abhed/internal/managed"

// goList runs go list in the module's root.
func goList(t *testing.T, args ...string) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gobin, append([]string{"list"}, args...)...) // #nosec G204 -- go list of this module
	cmd.Dir = root
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOROOT=") {
			env = append(env, e)
		}
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return string(out)
}

// isolatingMain matches a TestMain that goes through this package, or
// through clitest's Main, which does.
var isolatingMain = regexp.MustCompile(`(?s)func TestMain\(m \*testing\.M\) \{.*?(testenv\.(Main|Isolate)\(|clitest\.Main\(|os\.Exit\(Main\(m\)\))`)

// Every test binary that links internal/managed, where every path the
// organisation's files are read from lives, runs its tests through Isolate,
// so no test reads this machine's managed files whatever it does. Nothing
// moves the paths at run time: release builds and an embedder's own test
// binaries keep reading the machine's files.
func TestEveryTestBinaryLinkingTheManagedPathsIsIsolated(t *testing.T) {
	dirs := map[string]string{}
	for _, line := range strings.Split(goList(t, "-f", "{{.ImportPath}}|{{.Dir}}", "./..."), "\n") {
		if ip, dir, ok := strings.Cut(line, "|"); ok {
			dirs[ip] = dir
		}
	}
	var missing []string
	for _, line := range strings.Split(goList(t, "-test", "-f", `{{.ImportPath}}|{{join .Deps ","}}`, "./..."), "\n") {
		ip, deps, ok := strings.Cut(line, "|")
		if !ok || !strings.HasSuffix(ip, ".test") || !slices.Contains(strings.Split(deps, ","), managedPkg) {
			continue
		}
		pkg := strings.TrimSuffix(ip, ".test")
		if pkg == "github.com/zybuu-ai/abhed/internal/testenv" {
			continue // the isolation itself, tested here
		}
		dir, ok := dirs[pkg]
		if !ok {
			t.Errorf("%s: no directory listed", pkg)
			continue
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
		found := false
		for _, f := range files {
			data, err := os.ReadFile(f) // #nosec G304 -- this module's own test files
			if err == nil && isolatingMain.Match(data) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, strings.TrimPrefix(pkg, "github.com/zybuu-ai/abhed/"))
		}
	}
	if len(missing) > 0 {
		t.Errorf("these test binaries link internal/managed with no TestMain through internal/testenv:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// The test framework stays out of what ships: a release binary and the SDK
// an embedder links do not import testing.
func TestTestingIsNotLinkedIntoReleases(t *testing.T) {
	for _, pkg := range []string{"./cmd/abhed", "./sdk"} {
		if slices.Contains(strings.Fields(goList(t, "-deps", pkg)), "testing") {
			t.Errorf("%s links the testing package", pkg)
		}
	}
}
