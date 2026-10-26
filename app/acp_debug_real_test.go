package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/dapbridge"
)

// A real Go debug session: Delve in the container tier, through the shipped
// bridge. Opt in with ABHED_DEBUG_REAL_IMAGE (an image with Go and dlv, on
// the engine already) and ABHED_REQUIRE_CONTAINER=1.
func TestStudioDebugsGoForReal(t *testing.T) {
	img := os.Getenv("ABHED_DEBUG_REAL_IMAGE")
	if img == "" || os.Getenv("ABHED_REQUIRE_CONTAINER") == "" {
		t.Skip("set ABHED_DEBUG_REAL_IMAGE and ABHED_REQUIRE_CONTAINER=1 to debug Go in a real container")
	}
	// The bridge, built for the container's architecture, beside this binary as beside the engine.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(filepath.Dir(exe), dapbridge.Name(runtime.GOARCH))
	build := exec.Command("go", "build", "-o", bridge, "github.com/zybuu-ai/abhed/cmd/abhed-dapbridge") // #nosec G204 -- this module's own command
	build.Env = append(withoutGOROOTEnv(os.Environ()), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the bridge: %v\n%s", err, out)
	}
	r := newStudioRig(t, `,"sandbox":{"min_tier":"container","image":"`+img+`"}`)
	id := r.open()
	r.write("go.mod", "module dbg\n\ngo 1.24\n")
	r.write("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello from debuggee\") }\n")
	from := r.cl.mark()
	var res struct {
		DebugID string `json:"debugId"`
	}
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "go", "program": ".", "env": map[string]string{"GOCACHE": "/tmp/gocache", "GOFLAGS": "-mod=mod"}}, &res)
	send := func(m string) {
		r.cl.ok("_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(m)}, nil)
	}
	send(`{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"go","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path"}}`)
	r.cl.waitFor(from, "launched", func(m rpcMessage) bool {
		if m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"failed"`) {
			t.Fatalf("the session failed: %s", m.Params)
		}
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"launched"`)
	})
	send(`{"seq":2,"type":"request","command":"configurationDone"}`)
	defer func() {
		if t.Failed() {
			for _, m := range r.cl.since(from) {
				t.Logf("%s %s", m.Method, m.Params)
			}
		}
	}()
	r.cl.waitFor(from, "the program's output", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/message" && strings.Contains(string(m.Params), "hello from debuggee")
	})
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
	r.cl.waitFor(from, "ended", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && (strings.Contains(string(m.Params), `"ended"`) || strings.Contains(string(m.Params), `"failed"`))
	})
	if launched, _ := r.recorded(id, agent.EvDebugLaunched); len(launched) != 1 || launched[0]["tier"] != "container" {
		t.Fatalf("debug.launched %v", launched)
	}
}

func withoutGOROOTEnv(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, "GOROOT=") {
			out = append(out, e)
		}
	}
	return out
}
