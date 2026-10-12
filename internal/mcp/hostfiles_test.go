package mcp

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// A server set to host_files is launched by the network-only launcher; any
// other by the one that confines its files too.
func TestHostFilesPicksTheNetworkOnlyLauncher(t *testing.T) {
	var used []string
	launcher := func(tag string) Launcher {
		return func(context.Context, string, []string, []string, func(string, map[string]any) error) (*exec.Cmd, error) {
			used = append(used, tag)
			return nil, errors.New("stop here")
		}
	}
	g := NewGateway()
	defer g.Close()
	g.Confine, g.ConfineNet = launcher("files"), launcher("net")
	_ = g.Connect(context.Background(), []ServerConfig{
		{Name: "a", Command: "/bin/true", Enabled: true},
		{Name: "b", Command: "/bin/true", Enabled: true, HostFiles: true},
	})
	if len(used) != 2 || used[0] != "files" || used[1] != "net" {
		t.Fatalf("launchers used: %v", used)
	}
}
