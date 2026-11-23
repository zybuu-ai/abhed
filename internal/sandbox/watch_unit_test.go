//go:build unix

package sandbox

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeTable is a process table the Watch reads instead of the real one; no
// real process is read or signalled.
type fakeTable struct {
	mu      sync.Mutex
	parents map[int]int
	starts  map[int]uint64
	sent    []int
	kill    func(int, syscall.Signal) error
	pin     func(int) (func(syscall.Signal) error, func(), error)
	sigs    []syscall.Signal
	// after runs under the table's lock once a signal is recorded.
	after func(pid int, sig syscall.Signal)
	// origins are macOS's unique ids, by pid; a pid without one has none.
	origins map[int]procOrigin
}

func (f *fakeTable) install(t *testing.T) {
	oldP, oldS, oldK, oldG := watchParents, watchStart, watchKill, sweepGrace
	oldSs, oldO, oldPin, oldN := watchStarts, watchOrigin, watchPin, watchName
	// The pin sends through the fake kill below; names are the fake's.
	watchPin = func(pid int) (func(syscall.Signal) error, func(), error) {
		return func(sig syscall.Signal) error { return watchKill(pid, sig) }, func() {}, nil
	}
	f.pin = watchPin
	watchName = func(pid int) string { return fmt.Sprintf("p%d", pid-fakeBase) }
	watchStarts = func() map[int]uint64 {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[int]uint64{}
		for k, v := range f.starts {
			out[k] = v
		}
		return out
	}
	watchOrigin = func(pid int) (procOrigin, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		// As proc_info(2), whatever holds the pid now: its start time may
		// not be the one the table was read at.
		o, ok := f.origins[pid]
		if _, live := f.starts[pid]; !ok || !live {
			return procOrigin{}, false
		}
		return o, true
	}
	watchParents = func() map[int]int {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[int]int{}
		for k, v := range f.parents {
			out[k] = v
		}
		return out
	}
	watchStart = func(pid int) (uint64, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st, ok := f.starts[pid]
		return st, ok
	}
	watchKill = func(pid int, sig syscall.Signal) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		// Only a pid of the fake table is ever "signalled", and none is real:
		// every fake pid is above any system's pid limit.
		if _, ok := f.starts[pid]; !ok || pid < fakeBase {
			panic(fmt.Sprintf("the watch would signal %d, which is not in the fake table", pid))
		}
		f.sent = append(f.sent, pid)
		f.sigs = append(f.sigs, sig)
		if f.after != nil {
			f.after(pid, sig)
		}
		if sig == killSignal {
			delete(f.parents, pid)
			delete(f.starts, pid)
		}
		return nil
	}
	sweepGrace = 200 * time.Millisecond
	f.kill = watchKill
	t.Cleanup(func() {
		watchParents, watchStart, watchKill, sweepGrace = oldP, oldS, oldK, oldG
		watchStarts, watchOrigin, watchPin, watchName = oldSs, oldO, oldPin, oldN
	})
}

// fakeBase puts every fake pid above Linux's highest pid (4194304) and
// macOS's (99998): one that reached kill(2) by mistake would name no process.
const fakeBase = 10_000_000

// stillFake fails the test unless the Watch's signal function is the fake.
func (f *fakeTable) stillFake(t *testing.T) {
	t.Helper()
	if reflect.ValueOf(watchKill).Pointer() != reflect.ValueOf(f.kill).Pointer() ||
		reflect.ValueOf(watchPin).Pointer() != reflect.ValueOf(f.pin).Pointer() {
		t.Fatal("the Watch's signal function is not the fake: stopping before a real pid is signalled")
	}
}

// The cleanup signals only processes proven to descend from the command:
// never its parent, a sibling, an unrelated process of this user, or a pid
// reused by another process after a descendant left it.
func TestWatchSignalsOnlyTheCommandsOwnTree(t *testing.T) {
	f := &fakeTable{
		parents: map[int]int{fakeBase + 100: fakeBase + 50, fakeBase + 101: fakeBase + 100, fakeBase + 102: fakeBase + 101, fakeBase + 200: 1, fakeBase + 300: 42, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, fakeBase + 100: 10, fakeBase + 101: 11, fakeBase + 102: 12, fakeBase + 200: 20, fakeBase + 300: 30},
	}
	f.install(t)
	f.stillFake(t)
	w := &Watch{root: fakeBase + 100, seen: map[int]uint64{fakeBase + 100: 10}, stop: make(chan struct{}), polled: make(chan struct{})}
	w.poll()
	// +101 exits; +102, a double fork, is now launchd's; +103 is its child;
	// +101's pid is taken by an unrelated process; +400 is unrelated.
	f.mu.Lock()
	delete(f.parents, fakeBase+101)
	f.parents[fakeBase+102] = 1
	f.parents[fakeBase+103], f.starts[fakeBase+103] = fakeBase+102, 13
	f.parents[fakeBase+101], f.starts[fakeBase+101] = 1, 99
	f.parents[fakeBase+400], f.starts[fakeBase+400] = 1, 40
	f.mu.Unlock()
	f.stillFake(t)
	if left := w.endFollowed(); left != "" {
		t.Errorf("left %q", left)
	}
	allowed := map[int]bool{fakeBase + 102: true, fakeBase + 103: true}
	for _, pid := range f.sent {
		if !allowed[pid] {
			t.Errorf("signalled %d, which is not the command's own descendant", pid)
		}
	}
	got := map[int]bool{}
	for _, pid := range f.sent {
		got[pid] = true
	}
	if !got[fakeBase+102] || !got[fakeBase+103] {
		t.Errorf("the command's descendants were not ended: sent %v", f.sent)
	}
	// signal itself refuses anything not followed, pid 1, and the root.
	for _, pid := range []int{1, fakeBase + 50, fakeBase + 100, fakeBase + 200, fakeBase + 300, fakeBase + 400} {
		if err := w.signal(pid, killSignal); err == nil {
			t.Errorf("signal(%d) was allowed", pid)
		}
	}
}

// A child forked by a followed process and reparented to launchd before any
// read saw it is still followed, by the unique id of the parent that forked
// it, which the kernel keeps. A launchd child forked by anything else is not,
// whatever its pid or start time, and neither is a process at a reused pid.
func TestAReparentedChildIsFollowedByItsOrigin(t *testing.T) {
	const (
		root     = fakeBase + 100
		launcher = fakeBase + 101 // followed while it ran
		daemon   = fakeBase + 102 // forked by launcher, launchd's before any read
		grand    = fakeBase + 103 // the daemon's own child
		app      = fakeBase + 500 // launchd's child, forked by something else
		reused   = fakeBase + 600 // at a pid whose origin was read at another start
	)
	f := &fakeTable{
		parents: map[int]int{root: fakeBase + 50, launcher: root, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, root: 10, launcher: 11},
		origins: map[int]procOrigin{root: {uniq: 9000, start: 10, parent: 1}, launcher: {uniq: 9001, start: 11, parent: 9000}},
	}
	f.install(t)
	w := &Watch{root: root, seen: map[int]uint64{root: 10}, uniq: map[uint64]bool{}, origins: map[int]origin{},
		stop: make(chan struct{}), polled: make(chan struct{})}
	w.noteUniq(root)
	w.poll()
	f.mu.Lock()
	delete(f.parents, launcher)
	delete(f.starts, launcher)
	f.parents[daemon], f.starts[daemon], f.origins[daemon] = 1, 12, procOrigin{uniq: 9002, start: 12, parent: 9001}
	f.parents[grand], f.starts[grand], f.origins[grand] = daemon, 13, procOrigin{uniq: 9003, start: 13, parent: 9002}
	f.parents[app], f.starts[app], f.origins[app] = 1, 50, procOrigin{uniq: 9500, start: 50, parent: 42}
	// The table read it at start 60; what proc_info then reads at that pid
	// started at 59 and names the launcher: not the process the table saw.
	f.parents[reused], f.starts[reused], f.origins[reused] = 1, 60, procOrigin{uniq: 9600, start: 59, parent: 9001}
	f.mu.Unlock()
	w.poll()
	f.stillFake(t)
	if left := w.endFollowed(); left != "" {
		t.Errorf("left %q", left)
	}
	got := map[int]bool{}
	for _, pid := range f.sent {
		got[pid] = true
	}
	if !got[daemon] || !got[grand] {
		t.Errorf("the reparented daemon and its child were not ended: sent %v", f.sent)
	}
	if got[app] || got[reused] {
		t.Errorf("signalled a launchd child the command did not fork: sent %v", f.sent)
	}
}

// A followed process exits and its pid is taken by an app carrying the
// command's marker (inherited or planted). The start time read before the
// signal is all that tells them apart, and the app is never signalled.
func TestAReusedPidIsNeverSignalled(t *testing.T) {
	const (
		root  = fakeBase + 100
		child = fakeBase + 101
	)
	f := &fakeTable{
		parents: map[int]int{root: fakeBase + 50, child: root, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, root: 10, child: 11},
	}
	f.install(t)
	fakeMarked(t, f, map[int]string{child: testMarker})
	fakeSessions(t, f, nil)
	w := &Watch{root: root, seen: map[int]uint64{root: 10}, stop: make(chan struct{}), polled: make(chan struct{})}
	w.poll()
	// The child exits; an app launchd starts takes its pid, with the marker.
	f.mu.Lock()
	f.parents[child], f.starts[child] = 1, 77
	f.mu.Unlock()
	f.stillFake(t)
	if n := endMarked(testMarker, 0, w.owns); n != 1 {
		t.Errorf("reported %d unproven, want the app at the reused pid", n)
	}
	if len(f.sent) > 0 {
		t.Errorf("signalled %v: the app at a followed process's reused pid", f.sent)
	}
	if err := w.signal(child, killSignal); err == nil {
		t.Error("signal allowed a reused pid")
	}
}

// A followed parent exits and its pid is taken by an unrelated process,
// whose child is then read: it is not followed, nor ever signalled.
func TestAChildOfAReusedParentPidIsNotFollowed(t *testing.T) {
	const (
		root     = fakeBase + 100
		parent   = fakeBase + 101
		stranger = fakeBase + 200
	)
	f := &fakeTable{
		parents: map[int]int{root: fakeBase + 50, parent: root, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, root: 10, parent: 11},
	}
	f.install(t)
	w := &Watch{root: root, seen: map[int]uint64{root: 10}, stop: make(chan struct{}), polled: make(chan struct{})}
	w.poll()
	f.mu.Lock()
	f.parents[parent], f.starts[parent] = 1, 88 // the pid, taken by another process
	f.parents[stranger], f.starts[stranger] = parent, 89
	f.mu.Unlock()
	w.poll()
	f.stillFake(t)
	if _, followed := w.seen[stranger]; followed {
		t.Error("followed the child of a process at a reused parent pid")
	}
	_ = w.endFollowed()
	if len(f.sent) > 0 {
		t.Errorf("signalled %v", f.sent)
	}
}

// What a command's end stops is named in its result, with how to keep a
// program running.
func TestEndedProcessesAreNamed(t *testing.T) {
	f := &fakeTable{
		parents: map[int]int{fakeBase + 100: fakeBase + 50, fakeBase + 101: fakeBase + 100, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, fakeBase + 100: 10, fakeBase + 101: 11},
	}
	f.install(t)
	w := &Watch{root: fakeBase + 100, seen: map[int]uint64{fakeBase + 100: 10}, stop: make(chan struct{}), polled: make(chan struct{})}
	w.poll()
	f.stillFake(t)
	_ = w.endFollowed()
	note := w.EndedNote()
	for _, want := range []string{"ended 1 processes", "p101", "run_in_background", "open -a"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note %q lacks %q", note, want)
		}
	}
	if got := endedNote([]string{"a", "a", "b", "c", "d", "e", "f", "g"}); !strings.Contains(got, "a ×2") || !strings.Contains(got, "and 2 more") {
		t.Errorf("summary %q", got)
	}
}

// A pid taken by another process between the check and the stop is
// continued at once, and reported as not the command's.
func TestAStopThatLandsOnAReusedPidIsUndone(t *testing.T) {
	const root, child = fakeBase + 100, fakeBase + 101
	f := &fakeTable{
		parents: map[int]int{root: fakeBase + 50, child: root, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, root: 10, child: 11},
	}
	f.install(t)
	w := &Watch{root: root, seen: map[int]uint64{root: 10}, stop: make(chan struct{}), polled: make(chan struct{})}
	w.poll()
	f.after = func(pid int, sig syscall.Signal) {
		if pid == child && sig == stopSignal {
			f.starts[child] = 99 // the child exited and its pid was taken in between
		}
	}
	f.stillFake(t)
	if err := w.signal(child, stopSignal); err == nil {
		t.Error("a stop that landed on a reused pid was reported as the command's")
	}
	if len(f.sigs) != 2 || f.sigs[1] != syscall.SIGCONT {
		t.Errorf("the stranger was not continued: %v", f.sigs)
	}
}

// Until the command is known exited and unreaped, its session id proves
// nothing, and no member is signalled through it.
func TestSessionProofWaitsForTheExitedRoot(t *testing.T) {
	const root, member = fakeBase + 100, fakeBase + 700
	f := &fakeTable{
		parents: map[int]int{root: fakeBase + 50, member: 1, fakeBase + 50: 1},
		starts:  map[int]uint64{fakeBase + 50: 1, root: 10, member: 70},
	}
	f.install(t)
	fakeMarked(t, f, map[int]string{member: testMarker})
	fakeSessions(t, f, map[int]int{member: root})
	w := &Watch{root: root, seen: map[int]uint64{root: 10}, leader: Leader{Pid: root, start: 10, ok: true},
		stop: make(chan struct{}), polled: make(chan struct{})}
	f.stillFake(t)
	if n := endMarked(testMarker, 0, w.owns); n != 1 || len(f.sent) > 0 {
		t.Errorf("signalled %v through a session not yet proven (unproven %d)", f.sent, n)
	}
}
