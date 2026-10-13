package app

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// doctor looks for MCP server containers a killed Abhed left only when a
// server is set to run in one: each engine call costs a podman machine.
func TestDoctorSweepsOnlyForContainerServers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no container engine script on Windows")
	}
	for _, c := range []struct {
		name  string
		mcp   string
		sweep bool
	}{
		{"no servers", ``, false},
		{"a URL server", `,"mcp":{"servers":[{"name":"u","url":"http://127.0.0.1:1/mcp","enabled":true}]}`, false},
		{"a host_files server", `,"mcp":{"servers":[{"name":"h","command":"true","host_files":true,"enabled":true}]}`, false},
		{"a disabled server", `,"mcp":{"servers":[{"name":"d","command":"true","enabled":false}]}`, false},
		{"a confined stdio server", `,"mcp":{"servers":[{"name":"s","command":"true","enabled":true}]}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			log := filepath.Join(bin, "calls")
			engine := "#!/bin/sh\necho \"$*\" >> '" + log + "'\ncase \"$*\" in *\"command -v\"*) exit 3;; esac\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(engine), 0o755); err != nil { // #nosec G306 -- a stub engine that must run
				t.Fatal(err)
			}
			m := &bgModelServer{}
			ws := bgWorkspace(t, m.start(t), c.mcp)
			cmd := mainHelper([]string{"-C", ws, "doctor"}, "PATH="+bin)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			_ = cmd.Run()
			b, _ := os.ReadFile(log)
			if !strings.Contains(string(b), "info") {
				t.Fatalf("the container tier was not chosen:\n%s\n%s", b, out.String())
			}
			swept := strings.Contains(string(b), "label=abhed.mcp.owner")
			if swept != c.sweep {
				t.Errorf("swept %v, want %v; engine calls:\n%s", swept, c.sweep, b)
			}
		})
	}
}
