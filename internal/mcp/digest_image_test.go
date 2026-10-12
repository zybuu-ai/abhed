package mcp

import (
	"strings"
	"testing"
)

// An image named by the digest pins the server only as a container engine's
// argument, and only as an image reference, never an option.
func TestImageDigestPinsOnlyAnEngine(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		command string
		args    []string
		pinned  bool
	}{
		{"docker", []string{"run", "-i", "--rm", "ghcr.io/org/server@" + d}, true},
		{"/usr/bin/podman", []string{"run", "-i", "registry.example.com:5000/s@" + d}, true},
		{"nerdctl", []string{"run", "ghcr.io/org/server@" + d}, true},
		{"docker", []string{"run", "--label=x@" + d, "ghcr.io/org/server:latest"}, false},
		{"/no/such/sh", []string{"x@" + d}, false},
		{"/no/such/docker-wrapper", []string{"ghcr.io/org/server@" + d}, false},
	} {
		got, err := pinnedCommand(ServerConfig{Command: c.command, Args: c.args, Digest: d}, nil)
		if pinned := err == nil && got == c.command; pinned != c.pinned {
			t.Errorf("%s %v: pinned %v (%q, %v)", c.command, c.args, pinned, got, err)
		}
	}
}
