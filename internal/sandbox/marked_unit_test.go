//go:build unix

package sandbox

import (
	"fmt"
	"os/exec"
	"sort"
	"syscall"
	"testing"
)

// fakeMarked makes markedPids list pids, of the fake table only, that carry
// marker; no real process is read.
func fakeMarked(t *testing.T, f *fakeTable, carriers map[int]string) {
	old := markedPids
	markedPids = func(marker string) []int {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []int
		for pid, m := range carriers {
			if _, live := f.starts[pid]; live && m == marker {
				out = append(out, pid)
			}
		}
		sort.Ints(out)
		return out
	}
	t.Cleanup(func() { markedPids = old })
}

// fakeSessions makes signalOne consult a fake session table and record what
// it would signal; it refuses any pid outside the fake table.
func fakeSessions(t *testing.T, f *fakeTable, sids map[int]int) {
	old := signalOne
	signalOne = func(pid, sid int, sig syscall.Signal) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if pid < fakeBase {
			panic(fmt.Sprintf("the session sweep would signal %d, which is not in the fake table", pid))
		}
		if _, live := f.starts[pid]; !live || sids[pid] != sid {
			return false
		}
		f.sent = append(f.sent, pid)
		if sig == killSignal {
			delete(f.parents, pid)
			delete(f.starts, pid)
		}
		return true
	}
	t.Cleanup(func() { signalOne = old })
}

const testMarker = markerName + "=00112233445566778899aabb"

// A marker never proves a process the command's own: only one followed from
// the command's start, or a member of the session it created, is ended. An
// app that inherited the marker, one into which it was planted, and a
// descendant that escaped before it was seen are reported, not signalled.
func TestMarkerNarrowsNeverWidens(t *testing.T) {
	const (
		root      = fakeBase + 100
		child     = fakeBase + 101 // a real descendant, followed
		escaped   = fakeBase + 102 // followed, then reparented to launchd
		unseen    = fakeBase + 103 // escaped to launchd before any read saw it
		inherited = fakeBase + 500 // an editor started from a terminal: launchd's, marked
		planted   = fakeBase + 600 // an app launchd started after launchctl setenv
		member    = fakeBase + 700 // in the command's session, not followed
	)
	f := &fakeTable{
		parents: map[int]int{root: fakeBase + 50, child: root, escaped: child, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, root: 10, child: 11, escaped: 12},
	}
	f.install(t)
	fakeMarked(t, f, map[int]string{child: testMarker, escaped: testMarker, unseen: testMarker,
		inherited: testMarker, planted: testMarker, member: testMarker})
	fakeSessions(t, f, map[int]int{child: root, member: root, escaped: escaped})
	w := &Watch{root: root, seen: map[int]uint64{root: 10}, leader: Leader{Pid: root, start: 10, ok: true},
		stop: make(chan struct{}), polled: make(chan struct{})}
	w.poll()
	f.mu.Lock()
	f.parents[escaped] = 1
	f.parents[unseen], f.starts[unseen] = 1, 13
	f.parents[inherited], f.starts[inherited] = 1, 50
	f.parents[planted], f.starts[planted] = 1, 60
	f.parents[member], f.starts[member] = 1, 70
	f.mu.Unlock()
	f.stillFake(t)

	unproven := endMarked(testMarker, w.owns)

	got := map[int]bool{}
	for _, pid := range f.sent {
		got[pid] = true
	}
	for _, pid := range []int{inherited, planted, unseen} {
		if got[pid] {
			t.Errorf("signalled %d, which only carries the marker", pid)
		}
	}
	for _, pid := range []int{child, escaped, member} {
		if !got[pid] {
			t.Errorf("did not end %d, which is proven the command's own; sent %v", pid, f.sent)
		}
	}
	if unproven != 3 {
		t.Errorf("reported %d unproven marked processes, want 3 (inherited, planted, unseen)", unproven)
	}
	if note := unprovenNote(unproven); note == "" {
		t.Error("the unproven processes were not reported")
	}
}

// The session proves only its members: a marker inherited by an editor that
// left the session (setsid) is counted as unproven and never signalled.
func TestSessionProofIsTheSessionOnly(t *testing.T) {
	const (
		shell  = fakeBase + 100
		job    = fakeBase + 101 // still in the shell's session
		editor = fakeBase + 500 // left it with setsid, keeps the marker
	)
	f := &fakeTable{
		parents: map[int]int{job: shell, editor: 1},
		starts:  map[int]uint64{shell: 10, job: 11, editor: 50},
	}
	f.install(t)
	fakeMarked(t, f, map[int]string{job: testMarker, editor: testMarker})
	fakeSessions(t, f, map[int]int{shell: shell, job: shell, editor: editor})
	unproven := endMarked(testMarker, inSession(shell))
	if len(f.sent) == 0 || f.sent[0] != job {
		t.Errorf("the session member was not ended: sent %v", f.sent)
	}
	for _, pid := range f.sent {
		if pid == editor {
			t.Fatal("signalled the editor, which only carries the terminal's marker")
		}
	}
	if unproven != 1 {
		t.Errorf("reported %d unproven, want 1", unproven)
	}
	// The leader itself, pid 1 and a session id of 1 are never signalled.
	for _, tc := range []struct{ sid, pid int }{{shell, shell}, {shell, 1}, {1, job}} {
		if inSession(tc.sid)(tc.pid, stopSignal) {
			t.Errorf("inSession(%d) signalled %d", tc.sid, tc.pid)
		}
	}
}

// A sandbox closing ends only followed commands: a shell, or a command
// without a Watch, is not reached through its marker.
func TestClosingSignalsNoMarkedProcessOfAnUnwatchedCommand(t *testing.T) {
	f := &fakeTable{parents: map[int]int{fakeBase + 500: 1}, starts: map[int]uint64{fakeBase + 500: 50}}
	f.install(t)
	called := false
	old := markedPids
	markedPids = func(string) []int { called = true; return []int{fakeBase + 500} }
	t.Cleanup(func() { markedPids = old })
	fakeSessions(t, f, nil)
	var c commandSet
	c.add(&exec.Cmd{Path: "/bin/sh", Env: []string{testMarker}})
	c.end()
	if called || len(f.sent) > 0 {
		t.Errorf("closing searched by marker (%v) or signalled %v", called, f.sent)
	}
}

// A marker abhed itself inherited (started from a command's terminal, say)
// is not passed on to the commands it runs on the host.
func TestHostCommandsDoNotInheritAMarker(t *testing.T) {
	t.Setenv(markerName, "ffeeddccbbaa998877665544")
	for _, kv := range HostCommandEnv() {
		if markerIn([]string{kv}) != "" {
			t.Fatalf("a host command inherits %s", kv)
		}
	}
}
