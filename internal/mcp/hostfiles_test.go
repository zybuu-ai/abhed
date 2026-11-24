package mcp

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
)

// A server set to host_files is launched by the network-only launcher; any
// other by the one that confines its files too.
func TestHostFilesPicksTheNetworkOnlyLauncher(t *testing.T) {
	// Servers start at once, so each launch is noted by its server's name.
	var mu sync.Mutex
	used := map[string]string{}
	launcher := func(tag string) Launcher {
		return func(_ context.Context, name string, _, _ []string, _ func(string, map[string]any) error) (*exec.Cmd, error) {
			mu.Lock()
			defer mu.Unlock()
			used[name] = tag
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
	if len(used) != 2 || used["a"] != "files" || used["b"] != "net" {
		t.Fatalf("launchers used: %v", used)
	}
}
