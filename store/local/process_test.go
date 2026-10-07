package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// The child half of the process tests: it writes to a session until it is
// killed, or holds it, as ABHED_RECORD_CHILD says.
func TestMain(m *testing.M) {
	if role := os.Getenv("ABHED_RECORD_CHILD"); role != "" {
		child(role, os.Getenv("ABHED_RECORD_DIR"))
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// child writes or holds until it is killed; it returns only on a failure.
func child(role, dir string) {
	s, err := Open(Options{Dir: dir, User: "child"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if err := s.CreateSession(context.Background(), store.SessionRecord{ID: "s-child", Workspace: dir}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600)
	rec := agent.NewRecorder(s, "s-child", "")
	big := strings.Repeat("x", 64<<10)
	for i := 0; ; i++ {
		if role == "hold" {
			time.Sleep(time.Hour)
		}
		if _, err := rec.Record(agent.EvObservation, agent.ActorTool, agent.Untrusted, agent.Observation{Content: fmt.Sprint(i, big)}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if _, err := rec.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Trusted, agent.Delta{Text: big[:i%4096]}); err != nil {
			return
		}
	}
}

func startChild(t *testing.T, role, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") // #nosec G304 -- the test binary itself
	cmd.Env = append(os.Environ(), "ABHED_RECORD_CHILD="+role, "ABHED_RECORD_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A writer killed mid-append leaves a record that verifies, perhaps with an
// unfinished last line, and the next writer repairs it and goes on.
func TestKillMidAppendThenRepair(t *testing.T) {
	for round := range 3 {
		dir := t.TempDir()
		cmd := startChild(t, "write", dir)
		// Wait for some lines first: a slow CI machine may write none in a fixed delay.
		waitLines(t, dir, "s-child.jsonl", 3)
		time.Sleep(time.Duration(round*70) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		s := openTest(t, dir)
		rep, err := s.Verify("s-child")
		if err != nil || !rep.OK {
			t.Fatalf("round %d: after the kill: %+v %v", round, rep, err)
		}
		if rep.Events < 2 {
			t.Fatalf("round %d: the child wrote only %d events", round, rep.Events)
		}
		// The lock went with the process: this one claims it and goes on.
		if ok, err := s.ClaimResume(context.Background(), "s-child"); !ok || err != nil {
			t.Fatalf("round %d: claim after the kill: %v %v", round, ok, err)
		}
		evs, _ := s.Events("s-child")
		if rep.Torn > 0 && evs[len(evs)-1].Type != agent.EvRecordRepaired {
			t.Fatalf("round %d: a torn tail was cut without record.repaired", round)
		}
		rec := agent.NewRecorder(s, "s-child", "")
		rec.Advance(evs[len(evs)-1].Seq)
		if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "after the crash"}); err != nil {
			t.Fatal(err)
		}
		if rep, _ := s.Verify("s-child"); !rep.OK || rep.Torn != 0 {
			t.Fatalf("round %d: after going on: %+v", round, rep)
		}
	}
}

// Another process holding a session refuses this one's writes; once it is
// gone, its lock is gone with it.
func TestAnotherProcessHoldsTheSession(t *testing.T) {
	dir := t.TempDir()
	cmd := startChild(t, "hold", dir)
	s := openTest(t, dir)
	ev := agent.Event{ID: agent.NewEventID(), SessionID: "s-child", Seq: 1, Type: agent.EvUserMessage,
		Payload: json.RawMessage(`{"text":"mine"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: time.Now()}
	if err := s.Append(ev); !errors.Is(err, ErrHeldElsewhere) {
		t.Fatalf("append while another process holds it: %v", err)
	}
	if ok, _ := s.ClaimResume(context.Background(), "s-child"); ok {
		t.Fatal("claimed while another process holds it")
	}
	if rec, err := s.GetSession(context.Background(), "s-child"); err != nil || rec.EndedAt != nil {
		t.Fatalf("held elsewhere should read as running: %+v %v", rec, err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if ok, err := s.ClaimResume(context.Background(), "s-child"); !ok || err != nil {
		t.Fatalf("claim after the holder died: %v %v", ok, err)
	}
	if err := s.Append(ev); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, s, "s-child")
}

// waitLines waits until a session file under dir holds at least n lines.
func waitLines(t *testing.T, dir, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var found int
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == name {
				if b, rerr := os.ReadFile(p); rerr == nil {
					found = bytes.Count(b, []byte("\n"))
				}
			}
			return nil
		})
		if found >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the child wrote fewer than %d lines in 20s", n)
}
