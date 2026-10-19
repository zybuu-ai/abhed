package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProjectToolchains(t *testing.T) {
	ws := t.TempDir()
	if got := ProjectToolchains(ws); len(got) != 0 {
		t.Fatalf("empty folder: %v", got)
	}
	for _, f := range []string{"go.mod", "package.json", "requirements.txt"} {
		if err := os.WriteFile(filepath.Join(ws, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A folder named like a marker is not one.
	if err := os.Mkdir(filepath.Join(ws, "Cargo.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	var progs []string
	for _, tc := range ProjectToolchains(ws) {
		progs = append(progs, tc.Program)
	}
	if !slices.Equal(progs, []string{"go", "node", "python3"}) {
		t.Fatalf("toolchains %v", progs)
	}
}

// The default image is known to lack every toolchain, with no engine asked.
func TestMissingProgramsDefaultImage(t *testing.T) {
	t.Setenv("ABHED_SANDBOX_IMAGE", "")
	old := Image
	Image = ""
	t.Cleanup(func() { Image = old })
	c := &Container{runtime: filepath.Join(t.TempDir(), "no-engine")}
	got, err := c.MissingPrograms(context.Background(), []string{"go", "node"})
	if err != nil || !slices.Equal(got, []string{"go", "node"}) {
		t.Fatalf("%v %v", got, err)
	}
	// Another image the engine cannot show is not checked, and nothing is pulled.
	SetConfiguredImage("docker.io/library/golang:1.26-bookworm")
	t.Cleanup(func() { SetConfiguredImage("") })
	if SandboxImage() != "docker.io/library/golang:1.26-bookworm" {
		t.Fatalf("image %s", SandboxImage())
	}
	if _, err := c.MissingPrograms(context.Background(), []string{"go"}); !errors.Is(err, ErrImageNotPresent) {
		t.Fatalf("err %v", err)
	}
}

// sandbox.image beats the variable; Image, set by a caller, beats both.
func TestSandboxImageOrder(t *testing.T) {
	t.Setenv("ABHED_SANDBOX_IMAGE", "env:1")
	old := Image
	Image = ""
	t.Cleanup(func() { Image = old; SetConfiguredImage("") })
	if SandboxImage() != "env:1" {
		t.Fatal(SandboxImage())
	}
	SetConfiguredImage(" cfg:1 ")
	if SandboxImage() != "cfg:1" {
		t.Fatal(SandboxImage())
	}
	Image = "set:1"
	if SandboxImage() != "set:1" {
		t.Fatal(SandboxImage())
	}
}

// The look container runs the image's shell with no network, no pull, no
// capabilities, no privilege gain, a read-only root and an unprivileged user.
func TestMissingProgramsLooksInAHardenedContainer(t *testing.T) {
	old := Image
	Image = "docker.io/library/golang:1.26-bookworm"
	t.Cleanup(func() { Image = old })
	var runArgs []string
	engine := func(ctx context.Context, args ...string) *exec.Cmd {
		if args[0] == "run" {
			runArgs = args
			return exec.CommandContext(ctx, "/bin/sh", "-c", "echo node")
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", "true")
	}
	got, err := (&Container{}).missingPrograms(context.Background(), []string{"go", "node"}, engine)
	if err != nil || !slices.Equal(got, []string{"node"}) {
		t.Fatalf("%v %v", got, err)
	}
	line := strings.Join(runArgs, " ")
	for _, want := range []string{"--pull=never", "--network=none", "--cap-drop=ALL", "--security-opt no-new-privileges", "--read-only", "--user 65534:65534"} {
		if !strings.Contains(line, want) {
			t.Errorf("look container without %s: %s", want, line)
		}
	}
	if i := slices.Index(runArgs, Image); i < 0 || slices.Index(runArgs, "--user") > i {
		t.Errorf("an option after the image: %s", line)
	}
}
