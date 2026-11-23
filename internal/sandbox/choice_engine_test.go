//go:build unix

package sandbox

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// realEngine readies the machine's container engine, with
// ABHED_REQUIRE_CONTAINER set; ABHED_REAL_HOME and ABHED_REAL_TMPDIR are the
// HOME and TMPDIR the engine's client finds its connection by (a podman
// machine's API socket lies in TMPDIR on a Mac).
func realEngine(t *testing.T) {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_CONTAINER") == "" {
		t.Skip("set ABHED_REQUIRE_CONTAINER=1 to choose tiers on a real engine")
	}
	if h := os.Getenv("ABHED_REAL_HOME"); h != "" {
		t.Setenv("HOME", h)
	}
	if d := os.Getenv("ABHED_REAL_TMPDIR"); d != "" {
		t.Setenv("TMPDIR", d)
	}
	// The test's own XDG folders would hide the engine's connection.
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Setenv(k, v)
			_ = os.Unsetenv(k)
		}
	}
	resetEngineProbes()
	t.Cleanup(resetEngineProbes)
	imageLooks.Clear()
	t.Cleanup(func() { imageLooks.Clear(); SetConfiguredImage("") })
	if ok, why := NewContainer(Policy{}).Available(); !ok {
		t.Fatalf("no container engine: %s", why)
	}
}

// On a real engine, the preference looks in a real image: one without git
// is passed over for the process tier, one with git keeps the container,
// whose commands then have git; a chosen container is used whatever it lacks.
func TestPreferOwnToolsOnARealEngine(t *testing.T) {
	realEngine(t)
	ws := t.TempDir()
	p := Policy{Workspace: ws, MinTier: TierProcess, PreferOwnTools: true}
	needProcess(t, p)

	for _, c := range []struct {
		image string
		want  Tier
	}{
		{DefaultImage, TierProcess},
		{os.Getenv("ABHED_IMAGE_WITHOUT_GIT"), TierProcess},
		{os.Getenv("ABHED_IMAGE_WITH_GIT"), TierContainer},
	} {
		if c.image == "" {
			continue
		}
		SetConfiguredImage(c.image)
		resetEngineProbes()
		sb, err := Select(p)
		if err != nil {
			t.Fatalf("%s: %v", c.image, err)
		}
		if sb.Tier() != c.want {
			t.Fatalf("%s: tier %s, want %s (%s)", c.image, sb.Tier(), c.want, ChoiceReason(sb))
		}
		t.Logf("%s: %s — %s", c.image, sb.Tier(), ChoiceReason(sb))
		if c.want == TierContainer {
			missing, known := MissingTools(sb)
			if !known || slices.Contains(missing, "git") {
				t.Fatalf("%s: missing %v (known %v)", c.image, missing, known)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			out, err := sb.Command(ctx, ws, "git --version").CombinedOutput()
			cancel()
			if err != nil || !strings.Contains(string(out), "git version") {
				t.Fatalf("%s: git in the container: %v %s", c.image, err, out)
			}
			_ = Close(sb)
		}
	}
	// Chosen, the container is used though its image has no git.
	SetConfiguredImage(cmpOr(os.Getenv("ABHED_IMAGE_WITHOUT_GIT"), DefaultImage))
	resetEngineProbes()
	sb, err := Select(Policy{Workspace: ws, MinTier: TierProcess, Tier: TierContainer, PreferOwnTools: true})
	if err != nil || sb.Tier() != TierContainer {
		t.Fatalf("chosen container: %v %v", sb, err)
	}
	_ = Close(sb)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
