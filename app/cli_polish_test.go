package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// headlessRun runs -p with stream-json output and the given stdin, killed
// after limit, and returns its events by type, stderr and whether it ran out.
func headlessRun(t *testing.T, ws string, stdin *os.File, limit time.Duration, args ...string) (map[string][]map[string]any, string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := mainHelper(append([]string{"-C", ws, "-output-format", "stream-json"}, args...))
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	timedOut := false
	select {
	case <-done:
	case <-ctx.Done():
		timedOut = true
		_ = cmd.Process.Kill() // our own child, by its handle
		<-done
	}
	evs := map[string][]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil {
			evs[ev.Type] = append(evs[ev.Type], ev.Payload)
		}
	}
	return evs, errOut.String(), timedOut
}

// #11: with a task given, -p does not wait on a stdin pipe nobody closes;
// "-" attaches stdin; input left unread is said once.
func TestPrintTaskDoesNotWaitOnStdin(t *testing.T) {
	m := &bgModelServer{}
	ws := bgWorkspace(t, m.start(t), "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	evs, stderr, timedOut := headlessRun(t, ws, r, 20*time.Second, "-p", "say hi")
	if timedOut || len(evs["user.message"]) != 1 {
		t.Fatalf("-p with a task waited on an open stdin pipe (timed out %v): %v\n%s", timedOut, evs["user.message"], stderr)
	}
	if strings.Contains(stderr, "stdin was not read") {
		t.Errorf("an empty pipe was reported as unread input: %s", stderr)
	}

	// Input already waiting is left out, and said so.
	r2, w2, _ := os.Pipe()
	_, _ = w2.WriteString("THE-LOG-LINE\n")
	evs, stderr, timedOut = headlessRun(t, ws, r2, 20*time.Second, "-p", "summarise")
	_ = r2.Close()
	_ = w2.Close()
	if timedOut || len(evs["user.message"]) != 1 || strings.Contains(stringOf(evs["user.message"][0]["text"]), "THE-LOG-LINE") {
		t.Fatalf("waiting input was read without -: %v", evs["user.message"])
	}
	if !strings.Contains(stderr, "stdin was not read, since the task is given; add - to attach it") {
		t.Errorf("no note about the unread input: %s", stderr)
	}

	// "-" attaches it.
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("THE-LOG-LINE\n")
	_, _ = f.Seek(0, 0)
	defer func() { _ = f.Close() }()
	evs, stderr, _ = headlessRun(t, ws, f, 20*time.Second, "-p", "summarise", "-")
	if u := evs["user.message"]; len(u) != 1 || !strings.Contains(stringOf(u[0]["text"]), "THE-LOG-LINE") {
		t.Fatalf("- did not attach stdin: %v\n%s", u, stderr)
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// "-" is a -p switch and disagrees with -no-stdin and stream-json input.
func TestStdinDashFlag(t *testing.T) {
	f := parse(t, "-p", "fix it", "-")
	if !f.stdinDash || f.task() != "fix it" || checkHeadlessFlags(f) != 0 {
		t.Fatalf("%+v", f)
	}
	for _, args := range [][]string{{"-", "fix"}, {"-p", "x", "-", "-no-stdin"}, {"-p", "-", "-input-format", "stream-json"}} {
		if checkHeadlessFlags(parse(t, args...)) != 2 {
			t.Errorf("%q was accepted", args)
		}
	}
	// After "--" a lone dash is a word of the task.
	if f := parse(t, "-p", "--", "a", "-", "b"); f.stdinDash || f.task() != "a - b" {
		t.Errorf("%+v", f)
	}
	var warn bytes.Buffer
	if readsStdin(&cliFlags{}, "task", true, func() bool { return false }, &warn) || warn.Len() != 0 {
		t.Error("a task read stdin")
	}
	if !readsStdin(&cliFlags{}, "", true, func() bool { return false }, &warn) {
		t.Error("no task did not read stdin")
	}
	if !readsStdin(&cliFlags{stdinDash: true}, "task", true, func() bool { return false }, &warn) {
		t.Error("- did not read stdin")
	}
	if readsStdin(&cliFlags{stdinDash: true}, "task", false, func() bool { return true }, &warn) {
		t.Error("a terminal was read")
	}
}
