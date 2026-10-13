package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// The terminal CLI, -p and doctor start a stdio MCP server with its files
// confined on the process tier whatever sandbox.network is, as rpc and serve
// do: it reads nothing in the home and has a HOME of its own.
func TestCLIStdioServerFilesConfined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no process tier on Windows")
	}
	if ok, why := sandbox.NewProcess(sandbox.DefaultPolicy(t.TempDir())).Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("as root bubblewrap starts no stdio server")
	}
	// A PATH without a container runtime, so the process tier is the one chosen.
	bin := t.TempDir()
	for _, tool := range []string{"sh", "cat", "env", "git", "sandbox-exec", "bwrap"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	srvDir := t.TempDir()
	srv := filepath.Join(srvDir, "srv")
	if err := os.WriteFile(srv, []byte("#!/bin/sh\n{ echo \"HOME=$HOME\"; cat \"$1\" 2>/dev/null; } > \"$HOME/result\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args func(ws string) []string
	}{
		{"cli", func(ws string) []string { return []string{"-C", ws} }},
		{"print", func(ws string) []string { return []string{"-C", ws, "-p", "hi"} }},
		{"doctor", func(ws string) []string { return []string{"-C", ws, "doctor"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &bgModelServer{}
			url := m.start(t)
			ws := bgWorkspace(t, url, `,"mcp":{"servers":[{"name":"probe","command":"`+srv+`","args":["PROBE_PATH"],"enabled":true}]}`)
			home := os.Getenv("HOME")
			secret := filepath.Join(home, "probe.txt")
			if err := os.WriteFile(secret, []byte("SECRET-IN-HOME"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(home, ".abhed", "config.json")
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfgPath, bytes.ReplaceAll(raw, []byte("PROBE_PATH"), []byte(secret)), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := mainHelper(c.args(ws), "PATH="+bin)
			cmd.Stdin = strings.NewReader("")
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			_ = cmd.Run()
			real := home
			if r, err := filepath.EvalSymlinks(home); err == nil {
				real = r
			}
			own := filepath.Join(real, ".abhed", "mcp", "probe", "home")
			got, err := os.ReadFile(filepath.Join(own, "result"))
			if err != nil {
				leaked, _ := os.ReadFile(filepath.Join(home, "result"))
				t.Fatalf("the server did not run with a HOME of its own (%v); in the real home: %q\n%s", err, leaked, out.String())
			}
			if strings.Contains(string(got), "SECRET-IN-HOME") {
				t.Fatalf("the server read a file in the home:\n%s", got)
			}
			if !strings.Contains(string(got), "HOME="+own) {
				t.Fatalf("the server's HOME is not its own %s:\n%s", own, got)
			}
		})
	}
}

// doctor says why a configured server is not connected: a digest that does
// not match, a URL server with a digest.
func TestDoctorSaysWhyAServerIsNotConnected(t *testing.T) {
	m := &bgModelServer{}
	digest := "sha256:" + strings.Repeat("0", 64)
	ws := bgWorkspace(t, m.start(t), `,"mcp":{"servers":[`+
		`{"name":"pinned","command":"/bin/sh","args":["-c","exit 0"],"digest":"`+digest+`","enabled":true},`+
		`{"name":"remote","url":"https://mcp.invalid/x","digest":"`+digest+`","enabled":true}]}`)
	cmd := mainHelper([]string{"-C", ws, "doctor"})
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	for _, want := range []string{
		`mcp server "pinned": the server's program does not match its digest`,
		`mcp server "remote": digest pins a program Abhed starts`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor does not say %q:\n%s", want, out.String())
		}
	}
}

// On the container tier doctor says, as a session does, that a server's
// program is not in the sandbox image, and how to run it.
func TestDoctorSaysAServerProgramIsNotInTheImage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no container engine script on Windows")
	}
	// An engine that answers its probes and finds no program in the image.
	bin := t.TempDir()
	engine := "#!/bin/sh\ncase \"$*\" in *\"command -v\"*) exit 3;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(engine), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &bgModelServer{}
	ws := bgWorkspace(t, m.start(t), `,"mcp":{"servers":[{"name":"js","command":"npx","args":["-y","x"],"enabled":true}]}`)
	cmd := mainHelper([]string{"-C", ws, "doctor"}, "PATH="+bin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	for _, want := range []string{`not connected: mcp server "js"`, "npx is not in the sandbox image", "ABHED_SANDBOX_IMAGE", `"host_files": true`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor does not say %q:\n%s", want, out.String())
		}
	}
}
