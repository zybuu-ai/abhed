package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubHelper puts a helper with body beside a fake engine, pins want, and
// points the cache at a temporary folder.
func stubHelper(t *testing.T, body, want string) (src, cache string) {
	t.Helper()
	dir, cache := t.TempDir(), t.TempDir()
	src = filepath.Join(dir, HelperName("arm64"))
	if err := os.WriteFile(src, []byte(body), 0o755); err != nil { // #nosec G306 -- a test executable
		t.Fatal(err)
	}
	oldPin, oldBeside, oldRoot := helperPin, helperBeside, helperCacheRoot
	t.Cleanup(func() { helperPin, helperBeside, helperCacheRoot = oldPin, oldBeside, oldRoot })
	helperPin = func(arch string) string {
		if arch == "arm64" {
			return want
		}
		return ""
	}
	helperBeside = func(arch string) (string, error) { return filepath.Join(dir, HelperName(arch)), nil }
	helperCacheRoot = func() (string, error) { return cache, nil }
	return src, cache
}

// v1 is the test helper's body and v1Sum its pin.
const v1 = "helper v1"

var v1Sum = func() string { h := sha256.Sum256([]byte(v1)); return hex.EncodeToString(h[:]) }()

// The shipped helper is used only as a checked copy under the cache, and a
// copy changed afterwards is replaced before it is used again.
func TestBundledHelperIsACheckedCopy(t *testing.T) {
	src, cache := stubHelper(t, "helper v1", v1Sum)
	got, why := BundledHelper("arm64")
	if got == "" || why != "" {
		t.Fatalf("no helper: %s", why)
	}
	if got == src || !strings.HasPrefix(got, cache) {
		t.Fatalf("the helper is used from %s, not a copy under %s", got, cache)
	}
	if b, _ := os.ReadFile(got); string(b) != "helper v1" {
		t.Fatalf("copy holds %q", b)
	}
	if info, _ := os.Stat(got); info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("copy is not executable: %v", info.Mode())
	}
	if err := os.WriteFile(got, []byte("tampered!"), 0o755); err != nil { // #nosec G306 -- a test executable
		t.Fatal(err)
	}
	again, why := BundledHelper("arm64")
	if b, _ := os.ReadFile(again); string(b) != "helper v1" || why != "" {
		t.Fatalf("a changed copy was used: %q %s", b, why)
	}
}

// A helper that does not match the pin, a missing one, and a build that pins
// none are each refused with why.
func TestBundledHelperRefusals(t *testing.T) {
	stubHelper(t, "not the pinned helper", v1Sum)
	if got, why := BundledHelper("arm64"); got != "" || !strings.Contains(why, "does not match") {
		t.Fatalf("a mismatched helper: %q %s", got, why)
	}
	if got, why := BundledHelper("amd64"); got != "" || !strings.Contains(why, "ships no Linux helper") {
		t.Fatalf("an unpinned arch: %q %s", got, why)
	}
	src, _ := stubHelper(t, "helper v1", v1Sum)
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if got, why := BundledHelper("arm64"); got != "" || !strings.Contains(why, "is not there") {
		t.Fatalf("a missing helper: %q %s", got, why)
	}
}

// With no relay_binary for an engine in a VM, the shipped helper is the relay.
func TestContainerRelayFallsBackToTheShippedHelper(t *testing.T) {
	stubHelper(t, "helper v1", v1Sum)
	old, oldArch := containerHostOS, helperArch
	t.Cleanup(func() { containerHostOS, helperArch = old, oldArch })
	containerHostOS, helperArch = "darwin", "arm64"
	c := &Container{policy: Policy{Workspace: t.TempDir()}}
	got, why := c.containerRelay()
	if got == "" || why != "" || filepath.Base(got) != HelperName("arm64") {
		t.Fatalf("relay %q: %s", got, why)
	}
	c.policy.RelayBinary = "/opt/abhed/abhed-linux-arm64"
	if got, _ := c.containerRelay(); got != "/opt/abhed/abhed-linux-arm64" {
		t.Fatalf("relay_binary did not win over the helper: %q", got)
	}
}
