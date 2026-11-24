//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"weak"
)

// Watch follows the processes a command starts, from its Start to its end, so
// that none outlives it: on macOS nothing else ends them (bubblewrap's PID
// namespace and --die-with-parent do on Linux). A detached child (nohup, &,
// setsid, a double fork) would otherwise keep writing to the workspace and the
// temp folders after the command, the run and its record had ended.
//
// Only processes proven to descend from the command are ended. The command
// leads a session of its own (EndWithCommand), whose members are swept while
// the exited command is still unreaped. And while the command runs, the
// process table is read every watchPoll for every process whose parent is
// followed, a session it made with setsid included. Every process also
// inherits the command's marker, but a marker is no proof (an app started from
// a terminal inherits it; launchctl setenv can plant it), so a marked process
// neither followed nor in the session is reported, never ended. On macOS a
// child handed to launchd before a read saw it is still followed, by the
// unique id of the parent that forked it. A process with a session of its own
// whose parent was born and gone between two reads escapes (off macOS, one
// that loses its parent between two reads): see the residual risk in the docs.
type Watch struct {
	cmd    *exec.Cmd
	root   int
	leader Leader

	mu   sync.Mutex
	seen map[int]uint64 // pid -> start time, the root included
	// rootStart is the root's start time, set once before any goroutine
	// starts, so it is read without the lock.
	rootStart uint64
	// uniq holds the unique ids of followed processes (macOS), and origins
	// what was read of launchd's children: a process reparented before a read
	// saw it is still followed if the parent that forked it was.
	uniq    map[uint64]bool
	origins map[int]origin
	done    bool
	// exited is set once the root is known exited and unreaped, so the
	// session it led is still its own.
	exited bool
	// ended names the processes the command started that were ended with it.
	ended []string

	stop   chan struct{}
	polled chan struct{}
}

// watchPoll is how often the process table is read while a command runs.
var watchPoll = 20 * time.Millisecond

// The Watch's view of the system, replaced in tests so no real process is
// read or signalled.
var (
	watchParents = processParents
	watchStart   = startTime
	watchKill    = syscall.Kill
	watchStarts  = processStarts
	watchOrigin  = processOrigin
	// watchPin names a pid for signalling: a pidfd on Linux, so the start
	// time read after it and the signal reach the same process.
	watchPin = pinProcess
	// watchName is a process's command name, for the result.
	watchName = processName
)

// procOrigin is what processOrigin reads of a process: its unique id, its
// start time (as startTime gives it), and the unique id of the parent that
// forked it.
type procOrigin struct{ uniq, start, parent uint64 }

// origin is what was read of a process at one start time; ok is false when
// it could not be read.
type origin struct {
	procOrigin
	ok bool
}

// StartWatch begins following cmd's processes. Call it right after Start, and
// Wait on it instead of cmd.
func StartWatch(cmd *exec.Cmd) *Watch {
	w := &Watch{cmd: cmd, seen: map[int]uint64{}, uniq: map[uint64]bool{}, origins: map[int]origin{},
		stop: make(chan struct{}), polled: make(chan struct{})}
	if cmd.Process == nil {
		close(w.polled)
		return w
	}
	w.root = cmd.Process.Pid
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setsid {
		w.leader = Lead(cmd)
	}
	if st, ok := watchStart(w.root); ok {
		w.seen[w.root], w.rootStart = st, st
		w.noteUniq(w.root)
	}
	runningWatches.Store(cmd, w)
	go w.follow()
	return w
}

// runningWatches maps a running command to its Watch, so a sandbox closing ends it.
var runningWatches sync.Map

func (w *Watch) follow() {
	defer close(w.polled)
	t := time.NewTicker(watchPoll)
	defer t.Stop()
	for {
		w.poll()
		select {
		case <-w.stop:
			w.poll()
			return
		case <-t.C:
		}
	}
}

// poll adds every live process whose parent is followed and is still the
// process seen at that pid, and, on macOS, every child of launchd forked by a
// followed process: its parent exited before a read saw it.
func (w *Watch) poll() {
	parents := watchParents()
	if parents == nil {
		return
	}
	starts := watchStarts()
	w.mu.Lock()
	defer w.mu.Unlock()
	same := map[int]bool{}
	isSame := func(pid int) bool {
		if v, ok := same[pid]; ok {
			return v
		}
		st, ok := watchStart(pid)
		v := ok && st == w.seen[pid]
		same[pid] = v
		return v
	}
	for grew := true; grew; {
		grew = false
		for pid, ppid := range parents {
			if _, ok := w.seen[pid]; ok {
				continue
			}
			if _, ok := w.seen[ppid]; !ok || !isSame(ppid) {
				// launchd's child, forked by a followed process: its start
				// time is the one read with its origin.
				if ppid == 1 {
					if o, ok := w.forkedByFollowed(pid, starts[pid]); ok {
						w.seen[pid], same[pid], grew = o.start, true, true
						w.uniq[o.uniq] = true
					}
				}
				continue
			}
			if st, ok := watchStart(pid); ok {
				w.seen[pid], same[pid], grew = st, true, true
				w.noteUniq(pid)
			}
		}
	}
}

// noteUniq records pid's unique id as followed, where the system has one.
func (w *Watch) noteUniq(pid int) {
	if w.uniq == nil {
		w.uniq = map[uint64]bool{}
	}
	if o, ok := watchOrigin(pid); ok && o.start == w.seen[pid] {
		w.uniq[o.uniq] = true
	}
}

// forkedByFollowed reports whether pid, a child of launchd that started at
// start (from the table's read), was forked by a followed process. Its origin
// is read once per process, a reused pid told by its start time, so a poll
// costs a map lookup for each launchd child already read.
func (w *Watch) forkedByFollowed(pid int, start uint64) (procOrigin, bool) {
	if start == 0 {
		return procOrigin{}, false
	}
	if w.origins == nil {
		w.origins = map[int]origin{}
	}
	o, known := w.origins[pid]
	if !known || o.start != start {
		po, ok := watchOrigin(pid)
		// Read at another start time: the pid was reused in between.
		o = origin{procOrigin: procOrigin{start: start, uniq: po.uniq, parent: po.parent}, ok: ok && po.start == start}
		w.origins[pid] = o
	}
	return o.procOrigin, o.ok && w.uniq[o.parent]
}

// Wait waits for the command to exit and, before reaping it (so its pid and
// session id stay its own), ends every process it left: its session's, its
// marker's, and every one followed. It returns only once they are gone, or
// says what could not be ended in left.
func (w *Watch) Wait() (left string, err error) {
	if w.root == 0 {
		return "", w.cmd.Wait()
	}
	exitErr := waitExited(w.root)
	close(w.stop)
	<-w.polled
	w.mu.Lock()
	w.exited = exitErr == nil
	w.mu.Unlock()
	var notes []string
	if exitErr == nil && w.leader.ok {
		if r := w.leader.sweep(); r != "" {
			notes = append(notes, r)
		}
	}
	if r := w.endFollowed(); r != "" {
		notes = append(notes, r)
	}
	// The marker narrows what is proven, never widens it: one carrying it
	// that was never followed nor is in the session is reported, not ended.
	if m := markerIn(w.cmd.Env); m != "" {
		if r := unprovenNote(endMarked(m, w.rootStart, w.owns)); r != "" {
			notes = append(notes, r)
		}
	}
	err = w.cmd.Wait()
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
	runningWatches.Delete(w.cmd)
	forgetCommand(w.cmd)
	return strings.Join(notes, "; "), err
}

// endFollowed stops every followed process still the one seen at its pid,
// and any child a stopped one forked meanwhile, then kills them all and waits
// for them to go. Only processes proven to descend from the command are ever
// signalled: each was seen with a followed parent (see signal).
func (w *Watch) endFollowed() string {
	still := func(pid int) bool {
		w.mu.Lock()
		st, ok := w.seen[pid]
		w.mu.Unlock()
		cur, live := watchStart(pid)
		return ok && live && cur == st
	}
	stopped := map[int]bool{}
	for range maxSweepPasses {
		fresh := 0
		w.mu.Lock()
		pids := make([]int, 0, len(w.seen))
		for pid := range w.seen {
			if pid != w.root && !stopped[pid] {
				pids = append(pids, pid)
			}
		}
		w.mu.Unlock()
		for _, pid := range pids {
			// signal reads the start time again once stopped, and continues a
			// pid freed and taken meanwhile.
			if !still(pid) || w.signal(pid, stopSignal) != nil {
				continue
			}
			stopped[pid] = true
			fresh++
			if name := watchName(pid); name != "" {
				w.mu.Lock()
				w.ended = append(w.ended, name)
				w.mu.Unlock()
			}
		}
		// A child a stopped member forked before it stopped descends from it.
		w.mu.Lock()
		for pid, ppid := range watchParents() {
			if _, known := w.seen[pid]; known || !stopped[ppid] {
				continue
			}
			if st, ok := watchStart(pid); ok {
				w.seen[pid] = st
				fresh++
			}
		}
		w.mu.Unlock()
		if fresh == 0 {
			break
		}
	}
	deadline := time.Now().Add(sweepGrace)
	for {
		live := watchParents()
		left := 0
		for pid := range stopped {
			if _, ok := live[pid]; !ok || !still(pid) {
				continue
			}
			left++
			_ = w.signal(pid, killSignal)
		}
		if left == 0 {
			return ""
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("%d processes the command started were still running after it was ended", left)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// signal sends sig to pid only if pid is a process this Watch followed from
// the command's root (never the root itself here, never pid 1 or this
// process) and is still the process seen at that pid. A stop is checked
// again once in effect, and undone if the pid was taken meanwhile.
func (w *Watch) signal(pid int, sig syscall.Signal) error {
	w.mu.Lock()
	st, followed := w.seen[pid]
	w.mu.Unlock()
	if !followed || pid <= 1 || pid == w.root || pid == os.Getpid() || pid == os.Getppid() {
		return syscall.EPERM
	}
	send, release, err := watchPin(pid)
	if err != nil {
		return syscall.ESRCH
	}
	defer release()
	if cur, ok := watchStart(pid); !ok || cur != st {
		return syscall.ESRCH
	}
	if err := send(sig); err != nil {
		return err
	}
	if sig == stopSignal {
		if cur, ok := watchStart(pid); !ok || cur != st {
			_ = send(syscall.SIGCONT)
			return syscall.ESRCH
		}
	}
	return nil
}

// owns signals pid only if it is proven the command's own: followed from the
// command's start (see signal), or a member of the session the command
// created, while the command is known exited and unreaped, so the session id
// is still its own (Wait).
func (w *Watch) owns(pid int, sig syscall.Signal) bool {
	if w.signal(pid, sig) == nil {
		return true
	}
	w.mu.Lock()
	exited := w.exited
	w.mu.Unlock()
	return w.leader.ok && exited && inSession(w.root)(pid, sig)
}

// EndedNote names what the command started that was ended with it, and how
// to keep such a program running, or "" when nothing was.
func (w *Watch) EndedNote() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return endedNote(w.ended)
}

// endedNote is EndedNote's text for names.
func endedNote(names []string) string {
	if len(names) == 0 {
		return ""
	}
	counts := map[string]int{}
	var order []string
	for _, n := range names {
		if counts[n] == 0 {
			order = append(order, n)
		}
		counts[n]++
	}
	var parts []string
	for i, n := range order {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(order)-5))
			break
		}
		if counts[n] > 1 {
			n = fmt.Sprintf("%s ×%d", n, counts[n])
		}
		parts = append(parts, n)
	}
	return fmt.Sprintf("ended %d processes the command started (%s); to keep a program running, start it with run_in_background, or an app with `open -a`",
		len(names), strings.Join(parts, ", "))
}

// end stops the command and everything it started, for a sandbox closing
// while the command still runs.
func (w *Watch) end() {
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done {
		return
	}
	// The root is this process's own child, unreaped until Wait: its pid is
	// still its own.
	if st, ok := watchStart(w.root); ok && w.rootStart != 0 && st == w.rootStart {
		endTree(w.root)
		_ = watchKill(w.root, killSignal)
	}
	w.poll()
	_ = w.endFollowed()
	// What is left in the session, and what the marker finds unproven, is
	// Wait's to end or report once the root has exited.
}

// commandSet is the commands one sandbox made, ended when it closes. It
// holds them weakly: a command nothing else holds has ended and leaves the
// set, so it never needs to read a command's state, which the command's own
// Wait writes.
type commandSet struct {
	mu   sync.Mutex
	cmds map[weak.Pointer[exec.Cmd]]struct{}
}

// owners maps a command, weakly, to the set that holds it, so its end
// removes it.
var owners sync.Map // weak.Pointer[exec.Cmd] -> *commandSet

func (c *commandSet) add(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	k := weak.Make(cmd)
	c.mu.Lock()
	if c.cmds == nil {
		c.cmds = map[weak.Pointer[exec.Cmd]]struct{}{}
	}
	// One collected is done with.
	for old := range c.cmds {
		if old.Value() == nil {
			delete(c.cmds, old)
			owners.Delete(old)
		}
	}
	c.cmds[k] = struct{}{}
	c.mu.Unlock()
	owners.Store(k, c)
}

func forgetCommand(cmd *exec.Cmd) {
	k := weak.Make(cmd)
	if v, ok := owners.LoadAndDelete(k); ok {
		c := v.(*commandSet)
		c.mu.Lock()
		delete(c.cmds, k)
		c.mu.Unlock()
	}
}

// end ends every followed command of the set still running, with what it
// was seen to start. A command without a Watch is left to its own cancel: a
// marker alone would not prove anything it found the command's own.
func (c *commandSet) end() {
	c.mu.Lock()
	cmds := make([]*exec.Cmd, 0, len(c.cmds))
	for k := range c.cmds {
		owners.Delete(k)
		if cmd := k.Value(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	c.cmds = nil
	c.mu.Unlock()
	for _, cmd := range cmds {
		if v, ok := runningWatches.Load(cmd); ok {
			v.(*Watch).end()
		}
	}
}
