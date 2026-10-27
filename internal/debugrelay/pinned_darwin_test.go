package debugrelay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// A real debugpy, on the macOS process tier under the egress allowlist,
// debugs a program over the loopback ports opened to it alone.
func TestPinnedDebugpyUnderAllowlist(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	py := ""
	for _, c := range []string{"python3", "python3.13", "python3.12", "python3.14"} {
		if p, err := exec.LookPath(c); err == nil && exec.Command(p, "-c", "import debugpy").Run() == nil {
			py = p
			break
		}
	}
	if py == "" {
		t.Skip("no python3 with debugpy here")
	}
	bin := t.TempDir()
	if err := os.Symlink(py, filepath.Join(bin, "python3")); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	if err := os.WriteFile(filepath.Join(ws, "app.py"), []byte("print('hello from debuggee')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := Plan(Request{Language: Python, Program: "app.py"}, ws, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ports, err := sandbox.FreeLoopbackPorts(4)
	if err != nil {
		t.Fatal(err)
	}
	PinPorts(&spec, ports)
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	p := sandbox.DefaultPolicy(ws)
	p.Egress = pol
	sb := sandbox.NewProcess(p)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := sb.Command(sandbox.WithLoopbackPorts(ctx, ports...), ws, "export PATH="+bin+":/usr/bin:/bin; "+spec.Command)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	seq := 0
	send := func(command string, args any) {
		seq++
		b, _ := json.Marshal(map[string]any{"seq": seq, "type": "request", "command": command, "arguments": args})
		_, _ = fmt.Fprintf(in, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	msgs := make(chan map[string]any, 64)
	go func() {
		br := bufio.NewReader(out)
		for {
			n := 0
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					close(msgs)
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					break
				}
				if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
					n, _ = strconv.Atoi(v)
				}
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(br, b); err != nil {
				close(msgs)
				return
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			msgs <- m
		}
	}()
	send("initialize", map[string]any{"adapterID": "debugpy", "clientID": "abhed"})
	launch := spec.Launch
	send("launch", launch)
	seen := ""
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				t.Fatalf("the adapter ended; it wrote: %s\nseen: %s", stderr.String(), seen)
			}
			if m["event"] == "initialized" {
				send("configurationDone", map[string]any{})
			}
			if body, _ := m["body"].(map[string]any); m["event"] == "output" {
				seen += fmt.Sprint(body["output"])
				if strings.Contains(seen, "hello from debuggee") {
					return
				}
			}
			if m["type"] == "response" && m["success"] == false {
				t.Fatalf("%v failed: %v (stderr %s)", m["command"], m["message"], stderr.String())
			}
		case <-ctx.Done():
			t.Fatalf("no output from the debuggee; stderr: %s\nseen: %s", stderr.String(), seen)
		}
	}
}
