//go:build unix

package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeImage makes the image lack missing, counting the looks, and forgets
// what earlier tests found.
func fakeImage(t *testing.T, missing ...string) *int {
	t.Helper()
	looks := 0
	was := ImageMissing
	ImageMissing = func(_ *Container, programs []string) ([]string, error) {
		looks++
		var out []string
		for _, p := range programs {
			for _, m := range missing {
				if p == m {
					out = append(out, p)
				}
			}
		}
		return out, nil
	}
	imageLooks.Clear()
	t.Cleanup(func() { ImageMissing = was; imageLooks.Clear() })
	return &looks
}

func needProcess(t *testing.T, p Policy) {
	t.Helper()
	if ok, why := NewProcess(p).Available(); !ok {
		t.Skip("the process tier is not available here: " + why)
	}
}

// With podman up and an image without git, a person's own workspace gets the
// process tier, and the choice says why.
func TestPreferOwnToolsPassesOverAnImageWithoutGit(t *testing.T) {
	resetEngineProbes()
	t.Cleanup(resetEngineProbes)
	countingEngine(t)
	looks := fakeImage(t, "git", "curl", "ping", "node")
	p := Policy{Workspace: t.TempDir(), MinTier: TierProcess, PreferOwnTools: true}
	needProcess(t, p)
	sb, err := Select(p)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Tier() != TierProcess {
		t.Fatalf("tier %s, want process", sb.Tier())
	}
	why := ChoiceReason(sb)
	if !strings.Contains(why, "has no git, curl, ping, node") || !strings.Contains(why, "sandbox.tier") {
		t.Fatalf("the choice does not say why: %q", why)
	}
	// Asked once per image, however many sessions choose.
	if _, err := Select(p); err != nil || *looks != 1 {
		t.Fatalf("looked %d times (%v)", *looks, err)
	}
}

// An image with git keeps the container tier; without the preference, or
// with a minimum above the process tier, nothing is passed over.
func TestPreferOwnToolsKeepsTheContainerOtherwise(t *testing.T) {
	resetEngineProbes()
	t.Cleanup(resetEngineProbes)
	countingEngine(t)
	ws := t.TempDir()
	for name, c := range map[string]struct {
		missing []string
		p       Policy
	}{
		"image with git":     {[]string{"ping"}, Policy{Workspace: ws, MinTier: TierProcess, PreferOwnTools: true}},
		"no preference":      {[]string{"git"}, Policy{Workspace: ws, MinTier: TierProcess}},
		"container required": {[]string{"git"}, Policy{Workspace: ws, MinTier: TierContainer, PreferOwnTools: true}},
	} {
		fakeImage(t, c.missing...)
		sb, err := Select(c.p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sb.Tier() != TierContainer {
			t.Fatalf("%s: tier %s, want container", name, sb.Tier())
		}
		if why := ChoiceReason(sb); !strings.Contains(why, "strongest tier available") {
			t.Fatalf("%s: choice %q", name, why)
		}
	}
}

// An image that cannot be looked in is not passed over.
func TestPreferOwnToolsKeepsTheContainerWhenTheLookFails(t *testing.T) {
	resetEngineProbes()
	t.Cleanup(resetEngineProbes)
	countingEngine(t)
	was := ImageMissing
	ImageMissing = func(*Container, []string) ([]string, error) { return nil, errors.New("engine hung") }
	imageLooks.Clear()
	t.Cleanup(func() { ImageMissing = was; imageLooks.Clear() })
	sb, err := Select(Policy{Workspace: t.TempDir(), MinTier: TierProcess, PreferOwnTools: true})
	if err != nil || sb.Tier() != TierContainer {
		t.Fatalf("got %v, %v", sb, err)
	}
}

// A chosen tier is built or refused, never replaced, and never below the
// minimum: a managed min_tier of container refuses a chosen process tier.
func TestChosenTier(t *testing.T) {
	resetEngineProbes()
	t.Cleanup(resetEngineProbes)
	countingEngine(t)
	fakeImage(t, "git")
	ws := t.TempDir()
	p := Policy{Workspace: ws, MinTier: TierProcess, Tier: TierProcess}
	needProcess(t, p)
	sb, err := Select(p)
	if err != nil || sb.Tier() != TierProcess || !strings.Contains(ChoiceReason(sb), "chosen by sandbox.tier (process)") {
		t.Fatalf("process: %v %v", sb, err)
	}
	// Chosen, a container is used though its image has no git.
	sb, err = Select(Policy{Workspace: ws, MinTier: TierProcess, Tier: TierContainer, PreferOwnTools: true})
	if err != nil || sb.Tier() != TierContainer {
		t.Fatalf("container: %v %v", sb, err)
	}
	if _, err := Select(Policy{Workspace: ws, MinTier: TierContainer, Tier: TierProcess}); err == nil || !strings.Contains(err.Error(), "weaker than the required minimum") {
		t.Fatalf("process under a container minimum: %v", err)
	}
	// gVisor is not registered with the stub engine: refused, not replaced.
	if _, err := Select(Policy{Workspace: ws, MinTier: TierProcess, Tier: TierVM}); err == nil || !strings.Contains(err.Error(), "no other tier is used in its place") {
		t.Fatalf("vm without runsc: %v", err)
	}
	if _, err := Select(Policy{Workspace: ws, Tier: TierNone}); err == nil {
		t.Fatal("none was chosen")
	}
}

// The default image is known to lack the basic tools without a look.
func TestMissingToolsOfTheDefaultImage(t *testing.T) {
	looks := fakeImage(t)
	t.Setenv("ABHED_SANDBOX_IMAGE", "")
	SetConfiguredImage("")
	got, known := MissingTools(NewContainer(Policy{}))
	if !known || len(got) != len(BasicTools) || *looks != 0 {
		t.Fatalf("got %v %v after %d looks", got, known, *looks)
	}
	if _, known := MissingTools(NewProcess(Policy{})); known {
		t.Fatal("the process tier has no image")
	}
}

// A look that fails keeps the container and says so in the choice, and is
// not paid for again.
func TestFailedLookIsSaidAndKept(t *testing.T) {
	resetEngineProbes()
	t.Cleanup(resetEngineProbes)
	countingEngine(t)
	looks := 0
	was := ImageMissing
	ImageMissing = func(*Container, []string) ([]string, error) { looks++; return nil, errors.New("did not answer") }
	imageLooks.Clear()
	t.Cleanup(func() { ImageMissing = was; imageLooks.Clear() })
	p := Policy{Workspace: t.TempDir(), MinTier: TierProcess, PreferOwnTools: true}
	for range 2 {
		sb, err := Select(p)
		if err != nil || sb.Tier() != TierContainer || !strings.Contains(ChoiceReason(sb), "could not be looked in") {
			t.Fatalf("got %v %v %q", sb, err, ChoiceReason(sb))
		}
	}
	if looks != 1 {
		t.Fatalf("looked %d times", looks)
	}
}

// The look starts the image as commands do; an image whose entry point runs
// no shell lacks every program.
func TestLookUsesTheCommandsEntryPoint(t *testing.T) {
	SetConfiguredImage("registry.example/tools:1")
	t.Cleanup(func() { SetConfiguredImage("") })
	var ran []string
	engine := func(answer string, code int) func(context.Context, ...string) *exec.Cmd {
		return func(ctx context.Context, args ...string) *exec.Cmd {
			ran = args
			if args[0] == "image" {
				return exec.CommandContext(ctx, "true")
			}
			return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '"+answer+"'; exit "+strconv.Itoa(code))
		}
	}
	c := NewContainer(Policy{})
	got, err := c.missingPrograms(context.Background(), []string{"git", "curl"}, engine("abhed-look\\ncurl\\n", 0))
	if err != nil || strings.Join(got, ",") != "curl" || slices.Contains(ran, "--entrypoint") {
		t.Fatalf("got %v %v; ran %v", got, err, ran)
	}
	got, err = c.missingPrograms(context.Background(), []string{"git", "curl"}, engine("usage: git\\n", 1))
	if err != nil || strings.Join(got, ",") != "git,curl" {
		t.Fatalf("an entry point with no shell: %v %v", got, err)
	}
}
