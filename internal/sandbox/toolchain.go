package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Toolchain is a language toolchain a workspace looks like it needs: Program
// is what its commands run, and Markers the files that say so.
type Toolchain struct {
	Name    string
	Program string
	Markers []string
	// Image is a public image that has it, for a hint.
	Image string
}

// toolchains are the ones the doctor looks for, in the order it names them.
var toolchains = []Toolchain{
	{Name: "Go", Program: "go", Markers: []string{"go.mod", "go.work"}, Image: "docker.io/library/golang:1.26-bookworm"},
	{Name: "Node", Program: "node", Markers: []string{"package.json"}, Image: "docker.io/library/node:22-bookworm-slim"},
	{Name: "Python", Program: "python3", Markers: []string{"pyproject.toml", "requirements.txt", "setup.py", "Pipfile"}, Image: "docker.io/library/python:3.12-slim-bookworm"},
	{Name: "Rust", Program: "cargo", Markers: []string{"Cargo.toml"}, Image: "docker.io/library/rust:1-slim-bookworm"},
}

// ProjectToolchains are the toolchains the workspace's top folder names by
// its marker files. Only that folder is looked at, so the check stays cheap.
func ProjectToolchains(workspace string) []Toolchain {
	var out []Toolchain
	for _, tc := range toolchains {
		for _, m := range tc.Markers {
			if info, err := os.Lstat(filepath.Join(workspace, m)); err == nil && info.Mode().IsRegular() {
				out = append(out, tc)
				break
			}
		}
	}
	return out
}

// ErrImageNotPresent is MissingPrograms on an image the engine has not
// pulled: nothing is pulled to look.
var ErrImageNotPresent = errors.New("the image is not on this engine yet, so it was not checked")

// MissingPrograms are those of programs the sandbox image lacks. The default
// image is known to lack every toolchain; any other is looked in only when
// the engine already has it, with no network and no pull.
func (c *Container) MissingPrograms(ctx context.Context, programs []string) ([]string, error) {
	return c.missingPrograms(ctx, programs, c.engineCmd)
}

func (c *Container) missingPrograms(ctx context.Context, programs []string, engine func(context.Context, ...string) *exec.Cmd) ([]string, error) {
	if len(programs) == 0 {
		return nil, nil
	}
	img := SandboxImage()
	if img == DefaultImage {
		return programs, nil
	}
	if err := engine(ctx, "image", "inspect", "--format", "{{.Id}}", img).Run(); err != nil {
		return nil, ErrImageNotPresent
	}
	// The look runs the image's own shell, so it gets less than a command would.
	args := []string{"run", "--rm", "--pull=never", "--network=none", "--cap-drop=ALL",
		"--security-opt", "no-new-privileges", "--read-only", "--user", "65534:65534",
		"--entrypoint", "/bin/sh", "--name", containerName("abhed-look-"), img,
		"-c", `for p; do command -v "$p" >/dev/null 2>&1 || echo "$p"; done`, "sh"}
	out, err := engine(ctx, append(args, programs...)...).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
