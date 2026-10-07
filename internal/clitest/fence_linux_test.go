package clitest

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// These run the abhed binary with sandbox.tier "fence" against the stub
// model, end to end. They need Linux 6.7 or later, an ordinary user and a
// delegated cgroup, and run only with ABHED_REQUIRE_FENCE=1:
//
//	systemd-run --user --scope -p Delegate=yes env ABHED_REQUIRE_FENCE=1 go test ./internal/clitest -run Fence

func fenceConfig(network bool, procs int) string {
	return fmt.Sprintf(`{"sandbox":{"tier":"fence","min_tier":"process","allow_network":%v,"max_procs":%d,"max_memory_mb":1024},`+
		`"permissions":{"allow":["bash(*)"]},`+
		`"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"{{MODEL_URL}}","model":"stub-model","context_window":32768}}}}`, network, procs)
}

// fenceRun runs one headless task whose model calls bash with each command
// in turn, and returns the run and its record.
// fenceRun is fenceRunWith with the network off and small limits.
func fenceRun(t *testing.T, commands ...string) (*Run, Record) {
	t.Helper()
	return fenceRunWith(t, false, 64, 30000, commands...)
}

func fenceRunWith(t *testing.T, network bool, procs, timeoutMS int, commands ...string) (*Run, Record) {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		Pending(t, "fence", "set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to run the fence end to end")
	}
	var script strings.Builder
	for _, c := range commands {
		args, _ := json.Marshal(map[string]any{"command": c, "description": "fence check", "timeout_ms": timeoutMS})
		fmt.Fprintf(&script, "tool bash %s\n\n", args)
	}
	script.WriteString(`text "done"`)
	h := StartRun(t, Opts{Piped: true, Args: []string{"-p", "go", "-output-format", "json"}, UserConfig: fenceConfig(network, procs), Script: Script(script.String())})
	if code := h.Wait(time.Duration(len(commands)*timeoutMS)*time.Millisecond + time.Minute); code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, h.Stdout(), h.Stderr())
	}
	rec := h.Record()
	if !rec.Verified {
		t.Fatal("the record does not verify")
	}
	return h, rec
}

// observations are the bash results in order, by call id.
func observations(rec Record) (ids []string, text map[string]string) {
	text = map[string]string{}
	for _, e := range rec.Events {
		if e.Type != agent.EvObservation {
			continue
		}
		var o struct {
			CallID  string `json:"call_id"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(e.Payload, &o)
		ids = append(ids, o.CallID)
		text[o.CallID] = o.Content
	}
	return ids, text
}

func launched(rec Record) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range rec.Events {
		if e.Type == agent.EvProcessLaunched {
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			id, _ := p["call_id"].(string)
			out[id] = p
		}
	}
	return out
}

// The record holds the fence's qualification at the start and each launch,
// with its call id, before the command's result.
func TestFenceEndToEndRecordsQualificationAndLaunches(t *testing.T) {
	h, rec := fenceRun(t, "echo hi > made.txt && cat made.txt")
	types := rec.Types()
	q, l, o := -1, -1, -1
	for i, ty := range types {
		switch ty {
		case agent.EvFenceQualified:
			q = i
		case agent.EvProcessLaunched:
			l = i
		case agent.EvObservation:
			if o < 0 {
				o = i
			}
		}
	}
	if q < 0 || l < 0 || o < 0 || q >= l || l >= o || types[0] != agent.EvSessionStarted {
		t.Fatalf("events out of order: %v", types)
	}
	ids, text := observations(rec)
	if len(ids) != 1 || !strings.Contains(text[ids[0]], "hi") {
		t.Fatalf("result: %v", text)
	}
	if p := launched(rec)[ids[0]]; p == nil || p["seccomp"] != "command/3" {
		t.Fatalf("no launch for call %s: %v", ids[0], launched(rec))
	}
	if b, err := os.ReadFile(filepath.Join(h.Workspace(), "made.txt")); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("the workspace write did not land: %v %q", err, b)
	}
}

// A command cannot read Abhed's own state in home.
func TestFenceEndToEndDeniesAbhedState(t *testing.T) {
	_, rec := fenceRun(t, `f=$(printf '%s/.ab%s' "{{HOME}}" hed); cat "$f/config.json"; ls "$f"`)
	ids, text := observations(rec)
	if len(ids) != 1 {
		t.Fatalf("results: %v", text)
	}
	out := text[ids[0]]
	if launched(rec)[ids[0]] == nil {
		t.Fatalf("the command was not launched under the fence (refused before?):\n%s", out)
	}
	if !strings.Contains(out, "Permission denied") || strings.Contains(out, "stub-model") {
		t.Fatalf("Abhed's state was readable:\n%s", out)
	}
}

// With the network off, curl to a loopback listener fails and reaches nothing.
func TestFenceEndToEndNetworkOff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	hit := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			hit <- struct{}{}
			_ = c.Close()
		}
	}()
	_, rec := fenceRun(t, fmt.Sprintf("curl -sS --max-time 5 http://127.0.0.1:%d/ && echo reached", ln.Addr().(*net.TCPAddr).Port))
	ids, text := observations(rec)
	if len(ids) != 1 || strings.Contains(text[ids[0]], "reached") || launched(rec)[ids[0]] == nil {
		t.Fatalf("curl result:\n%v", text)
	}
	select {
	case <-hit:
		t.Fatal("the loopback listener was reached")
	default:
	}
}

// A fork bomb is held at the configured process limit and recorded.
func TestFenceEndToEndHoldsAForkBomb(t *testing.T) {
	_, rec := fenceRun(t, `b(){ b|b& }; b; sleep 3; echo survived`)
	var lim map[string]any
	for _, e := range rec.Events {
		if e.Type == agent.EvFenceLimit {
			_ = json.Unmarshal(e.Payload, &lim)
		}
	}
	if lim == nil || lim["pids_max"].(float64) == 0 {
		t.Fatalf("no fence.limit with pids_max: %v", rec.Types())
	}
}

// With the network on, name resolution and package installs work. This
// reaches the internet, so it runs only with ABHED_FENCE_NETWORK=1 as well,
// and reports what each step printed.
func TestFenceEndToEndNetworkOnInstalls(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		Pending(t, "fence", "set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to run the fence end to end")
	}
	// Its own item, so a job can run the fence offline and accept only this.
	if os.Getenv("ABHED_FENCE_NETWORK") != "1" {
		Pending(t, "fencenet", "set ABHED_FENCE_NETWORK=1 as well to reach the internet")
	}
	steps := []string{
		`getent ahosts pypi.org | head -2; python3 -c "import socket; print('getaddrinfo', socket.getaddrinfo('pypi.org', 443)[0][4])"`,
		`python3 -m venv .venv && .venv/bin/pip install --disable-pip-version-check -q six && .venv/bin/python -c 'import six; print("pip ok: six", six.__version__)'`,
		`a=$(uname -m); case $a in aarch64) a=arm64;; x86_64) a=x64;; esac; v=v20.18.0; ` +
			`n=node-$v-linux-$a.tar.xz; get() { curl -sSfL --retry 3 --retry-all-errors "https://nodejs.org/dist/$v/$1" -o "$1"; }; ` +
			`get $n && get SHASUMS256.txt && grep " $n\$" SHASUMS256.txt | sha256sum -c - && tar xJf $n && ` +
			`export PATH=$PWD/node-$v-linux-$a/bin:$PATH && npm install --no-audit --no-fund -s left-pad && ` +
			`node -e 'console.log("npm ok:", require("left-pad")("x", 3))'`,
	}
	_, rec := fenceRunWith(t, true, 512, 300000, steps...)
	ids, text := observations(rec)
	for i, id := range ids {
		t.Logf("step %d (%s):\n%s", i+1, id, text[id])
	}
	all := strings.Join(func() []string {
		var out []string
		for _, id := range ids {
			out = append(out, text[id])
		}
		return out
	}(), "\n")
	for _, want := range []string{"getaddrinfo", "pip ok", "npm ok"} {
		if !strings.Contains(all, want) {
			t.Errorf("%s did not succeed", want)
		}
	}
}

// A command that plants Abhed state in the workspace leaves none there: the
// record says where it went, and the session's next command does not run.
func TestFenceEndToEndPlantedStateDoesNotPersist(t *testing.T) {
	h, rec := fenceRun(t, `d=$(printf '.AB%s' hed); mkdir "$d" && echo '{"users":[]}' > "$d/users.json" && echo planted`, "echo next > next.txt")
	if !slices.Contains(rec.Types(), agent.EvFenceStatePlanted) {
		t.Fatalf("no fence.state_planted: %v", rec.Types())
	}
	ids, text := observations(rec)
	if len(ids) != 2 || !strings.Contains(text[ids[1]], "not run") {
		t.Fatalf("results: %v", text)
	}
	entries, _ := os.ReadDir(h.Workspace())
	for _, e := range entries {
		if strings.EqualFold(e.Name(), ".abhed") {
			t.Fatalf("%s persists in the workspace", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(h.Workspace(), "next.txt")); err == nil {
		t.Fatal("the next command ran")
	}
}
