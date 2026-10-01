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
	at := slices.Index(cmd.Args, Image)
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
