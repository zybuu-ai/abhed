package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// listener is one TCP socket listening on the Mac, with its process.
type listener struct {
	port, pid int
}

// listenRefresh is how often the listeners are read again.
const listenRefresh = 2 * time.Second

var listeners struct {
	mu  sync.Mutex
	at  time.Time
	ok  bool
	all []listener
}

// readListeners are the TCP sockets this user's processes listen on now,
// any address; another user's the kernel itself keeps a command from
// taking. ok is false when they cannot be read: a listener of our own,
// opened first, must be among them.
func readListeners() ([]listener, bool) { return readListenersWithin(listenRefresh) }

// readListenersWithin is readListeners, reusing a read no older than age.
func readListenersWithin(age time.Duration) ([]listener, bool) {
	listeners.mu.Lock()
	defer listeners.mu.Unlock()
	if time.Since(listeners.at) < age && listeners.ok {
		return listeners.all, true
	}
	own, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, false
	}
	defer func() { _ = own.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpn").Output()
	if err != nil {
		listeners.ok = false
		return nil, false
	}
	all := parseListeners(string(out))
	ownPort := own.Addr().(*net.TCPAddr).Port
	if !slices.ContainsFunc(all, func(l listener) bool { return l.port == ownPort }) {
		listeners.ok = false
		return nil, false
	}
	listeners.all, listeners.ok, listeners.at = all, true, time.Now()
	return all, true
}

// hostListeners are the ports in readListeners, sorted, each once. A
// command binding loopback on one would take the connections meant for it
// (a wildcard listener leaves 127.0.0.1 free to bind), so none is bindable.
func hostListeners() ([]int, bool) {
	all, ok := readListeners()
	if !ok {
		return nil, false
	}
	var ports []int
	for _, l := range all {
		if !slices.Contains(ports, l.port) {
			ports = append(ports, l.port)
		}
	}
	sort.Ints(ports)
	return ports, true
}

// ListenersReadable reports whether the Mac's listeners can be read now.
func ListenersReadable() bool {
	_, ok := hostListeners()
	return ok
}

// parseListeners reads lsof -Fpn: a p line names the process, and its n
// lines the addresses, n*:5432, n127.0.0.1:8080, n[::1]:8080.
func parseListeners(out string) []listener {
	var all []listener
	pid := 0
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n"):
			i := strings.LastIndexByte(line, ':')
			if i < 0 {
				continue
			}
			p, err := strconv.Atoi(line[i+1:])
			if err != nil || p <= 0 || p > 65535 {
				continue
			}
			l := listener{port: p, pid: pid}
			if !slices.Contains(all, l) {
				all = append(all, l)
			}
		}
	}
	return all
}

// HostHolds reports whether a program that is no command of a watched
// Abhed sandbox listens on port now, or the listeners cannot be read: a
// loopback address there is never a sandboxed server's. It reads afresh.
func HostHolds(port int) bool {
	all, ok := readListenersWithin(0)
	if !ok {
		return true
	}
	for _, l := range all {
		if l.port == port && markedSandboxOf(l.pid) == nil {
			return true
		}
	}
	return false
}

// listenMark is a sandbox's mark, which proves a process runs under one of
// its profiles: a folder no command may write, holding two files that are
// there for as long as the sandbox is. Every profile that lets commands
// listen denies reading mark and says nothing of open. No other profile
// names either, so a process denied mark but allowed open is this
// sandbox's, whatever its program or the file it was started from.
type listenMark struct {
	dir, mark, open string
}

// newListenMark makes a mark in the temp folder, under a name of its own.
func newListenMark() (*listenMark, error) {
	dir, err := os.MkdirTemp("", "abhed-listen-")
	if err != nil {
		return nil, err
	}
	m := &listenMark{dir: dir, mark: filepath.Join(dir, "mark"), open: filepath.Join(dir, "open")}
	for _, f := range []string{m.mark, m.open} {
		if err := os.WriteFile(f, nil, 0o644); err != nil { // #nosec G306 -- empty; read only by the sandbox's own check
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	return m, nil
}

// rules are the mark's lines in a profile, as Seatbelt's real path forms.
func (m *listenMark) rules() string {
	var b strings.Builder
	for _, d := range PathForms(m.dir) {
		fmt.Fprintf(&b, "(deny file-read* (literal %q))\n", filepath.Join(d, "mark"))
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", d)
	}
	return b.String()
}

// owns reports whether pid runs under a profile with this mark.
func (m *listenMark) owns(pid int) bool {
	if m == nil || pid <= 1 || pid == os.Getpid() {
		return false
	}
	denied, known := pathDenied(pid, m.mark)
	if !known || !denied {
		return false
	}
	denied, known = pathDenied(pid, m.open)
	return known && !denied
}

// markedSandboxOf is the watched sandbox pid runs in, or nil.
func markedSandboxOf(pid int) *Process {
	watching.mu.Lock()
	procs := make([]*Process, 0, len(watching.procs))
	for p := range watching.procs {
		procs = append(procs, p)
	}
	watching.mu.Unlock()
	for _, p := range procs {
		if p.listenMark().owns(pid) {
			return p
		}
	}
	return nil
}

// Listening processes are watched: a program of the person's that starts
// listening on a port a sandboxed command already holds (a wildcard bind
// with SO_REUSEADDR succeeds beside it) would lose its loopback connections
// to the command. At each refresh every process of that command's sandbox
// is ended, so nothing in it can listen again on the port, and only its
// session is told. A process no watched mark proves is never touched.

var watching struct {
	mu    sync.Mutex
	procs map[*Process]bool
	once  sync.Once
}

// watchListeners puts p among the sandboxes whose listening is watched.
func watchListeners(p *Process) {
	watching.mu.Lock()
	if watching.procs == nil {
		watching.procs = map[*Process]bool{}
	}
	watching.procs[p] = true
	watching.mu.Unlock()
	watching.once.Do(func() {
		go func() {
			for range time.Tick(listenRefresh) {
				sweepListeners()
			}
		}()
	})
}

// unwatchListeners takes p out, as it closes.
func unwatchListeners(p *Process) {
	watching.mu.Lock()
	delete(watching.procs, p)
	watching.mu.Unlock()
}

// ListenerEnded is what a sandbox's owner is told of a takeover ended: the
// port, the listener's process, and how many of the sandbox's processes
// were ended with it.
type ListenerEnded struct {
	Port, PID, Ended int
}

// sweepListeners ends the sandbox of each marked listener on a listen port
// a program outside it now listens on too.
func sweepListeners() {
	watching.mu.Lock()
	var procs []*Process
	ports := map[int]bool{}
	for p := range watching.procs {
		if p.listening() {
			procs = append(procs, p)
			for _, port := range p.policy.ListenPorts {
				ports[port] = true
			}
		}
	}
	watching.mu.Unlock()
	if len(procs) == 0 {
		return
	}
	all, ok := readListenersWithin(0)
	if !ok {
		for _, p := range procs {
			p.listenWatchLost("the Mac's listeners could not be read (lsof)")
		}
		return
	}
	for _, p := range procs {
		p.listenWatchFound()
	}
	for _, e := range takenOver(all, ports, func(pid int) bool { return markedSandboxOf(pid) != nil }) {
		endTakeover(e)
	}
}

// endTakeover ends every process of the sandbox that holds e, once a fresh
// read shows e still there and its mark still proves whose it is, and
// tells only that sandbox's owner, only if the listener was ended.
func endTakeover(e listener) {
	all, ok := readListenersWithin(0)
	if !ok || !slices.Contains(all, e) {
		return
	}
	owner := markedSandboxOf(e.pid)
	if owner == nil {
		return
	}
	m := owner.listenMark()
	ended, listenerEnded := 0, false
	for _, pid := range userPIDs() {
		if !m.owns(pid) {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			ended++
			listenerEnded = listenerEnded || pid == e.pid
		}
	}
	if !listenerEnded {
		return
	}
	if f := owner.onListenerEnded.Load(); f != nil {
		(*f)(ListenerEnded{Port: e.port, PID: e.pid, Ended: ended})
	}
}

// takenOver are the marked listeners on a watched port that a program no
// mark proves listens on too.
func takenOver(all []listener, ports map[int]bool, confinedPID func(int) bool) []listener {
	byPort := map[int][]int{}
	for _, l := range all {
		if ports[l.port] && !slices.Contains(byPort[l.port], l.pid) {
			byPort[l.port] = append(byPort[l.port], l.pid)
		}
	}
	var out []listener
	for port, pids := range byPort {
		if len(pids) < 2 {
			continue
		}
		var inside []int
		host := false
		for _, pid := range pids {
			if confinedPID(pid) {
				inside = append(inside, pid)
			} else {
				host = true
			}
		}
		if host {
			for _, pid := range inside {
				out = append(out, listener{port: port, pid: pid})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].port < out[j].port || out[i].port == out[j].port && out[i].pid < out[j].pid
	})
	return out
}
