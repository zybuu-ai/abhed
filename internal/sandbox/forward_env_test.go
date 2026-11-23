package sandbox

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A secret reaches a container by name, never by value in the engine's arguments.
func TestForwardEnvPassesNamesIntoTheContainer(t *testing.T) {
	c := &Container{runtime: "docker", policy: Policy{Workspace: t.TempDir()}}
	cmd := c.Command(t.Context(), "", "echo $TOKEN")
	cmd.Env = append(cmd.Env, "TOKEN=s3cret")
	ForwardEnv(cmd, []string{"TOKEN"})
	at := slices.Index(cmd.Args, SandboxImage())
	if at < 2 || cmd.Args[at-2] != "-e" || cmd.Args[at-1] != "TOKEN" {
		t.Fatalf("TOKEN not forwarded before the image: %v", cmd.Args)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "s3cret") {
		t.Fatalf("the value is in the engine's arguments: %v", cmd.Args)
	}
	plain := exec.Command("bash", "-c", "true")
	ForwardEnv(plain, []string{"TOKEN"})
	if len(plain.Args) != 3 {
		t.Fatalf("a non-container command was changed: %v", plain.Args)
	}
}

// The fence's and a container's commands get the person's git name and
// email, as their home holds none of the person's configuration.
func TestGitIdentityReachesFenceAndContainer(t *testing.T) {
	id := []string{"GIT_AUTHOR_NAME=Ana Person", "GIT_AUTHOR_EMAIL=ana@example.test"}
	f := NewFence(Policy{GitIdentity: id})
	for _, kv := range id {
		if !slices.Contains(f.env(), kv) {
			t.Errorf("the fence's env lacks %s: %v", kv, f.env())
		}
	}
	c := &Container{runtime: "docker", policy: Policy{Workspace: t.TempDir(), GitIdentity: id}}
	cmd := c.Command(t.Context(), "", "git commit")
	at := slices.Index(cmd.Args, SandboxImage())
	for _, kv := range id {
		i := slices.Index(cmd.Args, kv)
		if i < 1 || i > at || cmd.Args[i-1] != "-e" {
			t.Errorf("the container does not get %s: %v", kv, cmd.Args)
		}
	}
}
