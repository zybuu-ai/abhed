package clitest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/clitest/vt"
)

// recordingTB notes what cleanup did to the test, without ending it.
type recordingTB struct {
	testing.TB
	skipped, errored string
}

func (r *recordingTB) Helper()                   {}
func (r *recordingTB) Failed() bool              { return r.errored != "" }
func (r *recordingTB) Logf(string, ...any)       {}
func (r *recordingTB) Skipf(f string, a ...any)  { r.skipped = fmt.Sprintf(f, a...) }
func (r *recordingTB) Errorf(f string, a ...any) { r.errored = fmt.Sprintf(f, a...) }

func cleanupAfter(t *testing.T, output string) *recordingTB {
	t.Helper()
	rec := &recordingTB{TB: t}
	h := &run{t: rec, cmd: exec.Command("true"), done: make(chan struct{}), term: vt.New(80, 24, nil),
		raw: []byte(output), root: t.TempDir()}
	close(h.done)
	h.cleanup()
	return rec
}

const editorRace = "WARNING: DATA RACE\nWrite at 0x1 by goroutine 7:\n  github.com/zybuu-ai/abhed/internal/ui.(*editor).insert()\n==================\n"

// The line editor's known race is never passed silently: it is a pending
// skip the CI gate lists, and any other race fails.
func TestCleanupMarksTheKnownRacePending(t *testing.T) {
	if *runPending {
		t.Skip("run without -clitest-run-pending")
	}
	if rec := cleanupAfter(t, editorRace); rec.skipped != "clitest: pending (editor): the binary reported the line editor's known data race" {
		t.Fatalf("known race: skipped %q, errored %q", rec.skipped, rec.errored)
	}
	other := "WARNING: DATA RACE\nRead at 0x2 by goroutine 9:\n  github.com/zybuu-ai/abhed/internal/agent.(*Loop).Run()\n==================\n"
	if rec := cleanupAfter(t, editorRace+other); rec.skipped != "" || rec.errored == "" {
		t.Fatalf("another race: skipped %q, errored %q", rec.skipped, rec.errored)
	}
	if rec := cleanupAfter(t, "no race here"); rec.skipped != "" || rec.errored != "" {
		t.Fatalf("no race: skipped %q, errored %q", rec.skipped, rec.errored)
	}
}

func (r *recordingTB) Fatalf(f string, a ...any) { r.errored = fmt.Sprintf(f, a...) }

// A run's environment keeps it off the developer's own services and browser.
func TestRunEnvironmentIsIsolated(t *testing.T) {
	root := t.TempDir()
	h := &run{t: t, root: root, home: filepath.Join(root, "home"), ws: filepath.Join(root, "ws"), stub: NewStub(t, "")}
	h.writeFiles()
	env := strings.Join(h.env(), "\n")
	if strings.Contains(env, "localhost") || !strings.Contains(env, managedEnv+"="+filepath.Join(root, "etc", "abhed")) {
		t.Fatalf("env:\n%s", env)
	}
	for _, opener := range []string{"open", "xdg-open"} {
		if _, err := os.Stat(filepath.Join(root, "bin", opener)); err != nil {
			t.Errorf("no %s stub: %v", opener, err)
		}
	}
	for _, s := range []string{"http://localhost:11434/v1", "127.0.0.1:4000", "http://[::1]:4000"} {
		rec := &recordingTB{TB: t}
		h.t = rec
		h.refuseRealServices([]string{"-p", s}, nil)
		if rec.errored == "" {
			t.Errorf("%s was not refused", s)
		}
	}
	rec := &recordingTB{TB: t}
	h.t = rec
	h.refuseRealServices([]string{"-p", h.stub.URL()}, h.env())
	if rec.errored != "" {
		t.Errorf("the stub was refused: %s", rec.errored)
	}
}

// stoppingTB ends the goroutine on Fatal, as the testing package does, and
// notes why.
type stoppingTB struct {
	testing.TB
	fatal string
}

func (s *stoppingTB) Helper() {}
func (s *stoppingTB) Fatalf(f string, a ...any) {
	s.fatal = fmt.Sprintf(f, a...)
	runtime.Goexit()
}

// A run that names a real local model service is refused by Start, before
// the binary runs.
func TestStartRefusesARealLocalService(t *testing.T) {
	// A bad flag ends the binary before it loads a configuration, so even
	// if the refusal broke, nothing would reach the service.
	safeArgs := []string{"-p", "hi", "-output-format", "not-a-format"}
	for _, o := range []Opts{
		{Piped: true, Args: safeArgs, UserConfig: `{"model":{"default":"x","providers":{"x":{"type":"openai-compatible","base_url":"http://localhost:11434/v1","model":"m"}}}}`},
		{Piped: true, Args: safeArgs, Env: []string{"ABHED_BASE_URL=http://127.0.0.1:4000/v1"}},
	} {
		s := &stoppingTB{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			start(s, o)
		}()
		<-done
		if !strings.Contains(s.fatal, "real local model service") {
			t.Fatalf("%+v was started: %q", o, s.fatal)
		}
	}
}
