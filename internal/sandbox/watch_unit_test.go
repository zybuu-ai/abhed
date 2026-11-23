//go:build unix

package sandbox

import (
	"fmt"
	"reflect"
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
	// origins are macOS's unique ids, by pid; a pid without one has none.
	origins map[int]procOrigin
}

func (f *fakeTable) install(t *testing.T) {
	oldP, oldS, oldK, oldG := watchParents, watchStart, watchKill, sweepGrace
	oldSs, oldO := watchStarts, watchOrigin
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
		watchStarts, watchOrigin = oldSs, oldO
	})
}

// fakeBase puts every fake pid above Linux's highest pid (4194304) and
// macOS's (99998): one that reached kill(2) by mistake would name no process.
const fakeBase = 10_000_000

// stillFake fails the test unless the Watch's signal function is the fake.
func (f *fakeTable) stillFake(t *testing.T) {
	t.Helper()
	if reflect.ValueOf(watchKill).Pointer() != reflect.ValueOf(f.kill).Pointer() {
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
