package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapSession is a session whose bash checkpoints are collected by path.
func snapSession(t *testing.T, cfg BashCheckpoints) (*Session, string, map[string]FileBefore) {
	t.Helper()
	s, dir := setup(t)
	got := map[string]FileBefore{}
	s.BashCheckpoint = func(f FileBefore) { got[filepath.Base(f.Path)] = f }
	s.BashCheckpoints = cfg
	return s, dir, got
}

// A command's changes, removals and new files reach the checkpoint as they
// were before it; what it left alone, and the folders glob skips, do not.
func TestBashCheckpointsWhatACommandChanged(t *testing.T) {
	s, dir, got := snapSession(t, BashCheckpoints{})
	writeFile(t, filepath.Join(dir, "a.txt"), "a before\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "b before\n")
	writeFile(t, filepath.Join(dir, "same.txt"), "same\n")
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := run(t, Bash{}, s, bashArgs{Command: "echo after > a.txt; rm b.txt run.sh; echo new > c.txt; echo x > node_modules/m.js", Description: "change files"})
	if res.IsError || strings.Contains(res.Content, "[checkpoint") {
		t.Fatal(res.Content)
	}
	if f := got["a.txt"]; string(f.Before) != "a before\n" || !f.Existed {
		t.Errorf("a.txt %+v", f)
	}
	if f := got["b.txt"]; string(f.Before) != "b before\n" || !f.Existed {
		t.Errorf("b.txt %+v", f)
	}
	if f := got["run.sh"]; f.Mode != 0o755 || !f.Existed {
		t.Errorf("run.sh keeps its bits: %+v", f)
	}
	if f, ok := got["c.txt"]; !ok || f.Existed || f.Before != nil {
		t.Errorf("c.txt %+v %v", f, ok)
	}
	for _, n := range []string{"same.txt", "m.js"} {
		if _, ok := got[n]; ok {
			t.Errorf("%s checkpointed", n)
		}
	}

	// A file changed between commands is read again: the next checkpoint
	// holds what it was just before that command.
	clear(got)
	writeFile(t, filepath.Join(dir, "a.txt"), "edited between\n")
	run(t, Bash{}, s, bashArgs{Command: "echo again > a.txt", Description: "change again"})
	if f := got["a.txt"]; string(f.Before) != "edited between\n" {
		t.Errorf("a.txt second time %q", f.Before)
	}
	if len(got) != 1 {
		t.Errorf("checkpoints %v", got)
	}
}

// A file over the limit is not copied: its checkpoint says why and the
// result says so; "required" refuses the command and runs nothing.
func TestBashCheckpointLimits(t *testing.T) {
	s, dir, got := snapSession(t, BashCheckpoints{MaxBytes: 4})
	writeFile(t, filepath.Join(dir, "big.txt"), "more than four bytes\n")
	res := run(t, Bash{}, s, bashArgs{Command: "echo x > big.txt", Description: "change a big file"})
	if f := got["big.txt"]; f.Skipped == "" || f.Before != nil || !f.Existed {
		t.Fatalf("big.txt %+v", f)
	}
	if !strings.Contains(res.Content, "[checkpoint:") || !strings.Contains(res.Content, "no copy kept") {
		t.Fatalf("no note: %s", res.Content)
	}

	s.BashCheckpoints.Mode = BashCheckpointRequired
	clear(got)
	writeFile(t, filepath.Join(dir, "big.txt"), "more than four bytes\n")
	res = run(t, Bash{}, s, bashArgs{Command: "touch ran.txt", Description: "run"})
	if !res.IsError || !strings.Contains(res.Content, "required") {
		t.Fatalf("not refused: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}

	// Over the file count the walk is cut: a required snapshot refuses.
	s.BashCheckpoints = BashCheckpoints{Mode: BashCheckpointRequired, MaxFiles: 1}
	writeFile(t, filepath.Join(dir, "two.txt"), "2")
	if res := run(t, Bash{}, s, bashArgs{Command: "true", Description: "run"}); !res.IsError || !strings.Contains(res.Content, "more than the 1 files") {
		t.Fatalf("not refused: %s", res.Content)
	}
}

// Off, or with no hook, nothing is walked or recorded.
func TestBashCheckpointsOff(t *testing.T) {
	s, dir, got := snapSession(t, BashCheckpoints{Mode: BashCheckpointOff})
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	run(t, Bash{}, s, bashArgs{Command: "echo b > a.txt", Description: "change"})
	if len(got) != 0 {
		t.Fatalf("checkpointed while off: %v", got)
	}
	if BashCheckpointsFrom("", 0, 0).Mode != BashCheckpointOn {
		t.Fatal("the default is not on")
	}
}

// Files that look like keys are never copied, even in memory.
func TestBashCheckpointSkipsKeyFiles(t *testing.T) {
	s, dir, got := snapSession(t, BashCheckpoints{})
	writeFile(t, filepath.Join(dir, ".env"), "TOKEN=x\n")
	run(t, Bash{}, s, bashArgs{Command: "echo TOKEN=y > .env", Description: "change"})
	if f := got[".env"]; f.Skipped == "" || f.Before != nil {
		t.Fatalf(".env %+v", f)
	}
	for _, e := range s.snaps().last {
		if strings.Contains(string(e.data), "TOKEN") {
			t.Fatal("key file held in memory")
		}
	}
}

// "required" with no one to take the checkpoints refuses, and runs nothing.
func TestBashCheckpointsRequiredWithNoHook(t *testing.T) {
	s, dir := setup(t)
	s.BashCheckpoints = BashCheckpointsFrom("required", 0, 0)
	res := run(t, Bash{}, s, bashArgs{Command: "touch ran.txt", Description: "run"})
	if !res.IsError || !strings.Contains(res.Content, "tools.bash_checkpoints") || !strings.Contains(res.Content, "Nothing was run") {
		t.Fatalf("not refused: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
	// On, or off, with no hook it runs as before.
	for _, mode := range []string{"", "on", "off"} {
		s.BashCheckpoints = BashCheckpointsFrom(mode, 0, 0)
		if res := run(t, Bash{}, s, bashArgs{Command: "true", Description: "run"}); res.IsError {
			t.Fatalf("%q: %s", mode, res.Content)
		}
	}
}

// A background command is never checkpointed, so "required" refuses it.
func TestBashCheckpointsRequiredRefusesBackground(t *testing.T) {
	s, dir, _ := snapSession(t, BashCheckpointsFrom("required", 0, 0))
	res := run(t, Bash{}, s, bashArgs{Command: "touch bg.txt", Description: "run", Background: true})
	if !res.IsError || !strings.Contains(res.Content, "tools.bash_checkpoints") {
		t.Fatalf("not refused: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "bg.txt")); err == nil {
		t.Fatal("the command ran")
	}
}
