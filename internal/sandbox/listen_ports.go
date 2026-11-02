package sandbox

import (
	"context"
	"net"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// Listening processes are watched, and nothing more. A program of the
// person's that starts listening on a listen port a sandboxed command
// already holds (a wildcard bind with SO_REUSEADDR succeeds beside it) can
// lose its loopback connections to the command. Abhed cannot prove which
// of two listeners is a sandbox's, so it signals neither: it tells every
// sandbox that may listen on that port, once while the port stays
// contested, and a new command's profile leaves the port out, as it is
// busy then.

var watching struct {
	mu        sync.Mutex
	procs     map[*Process]bool
	contested map[int]bool
	once      sync.Once
}

// watchListeners puts p among the sandboxes whose listen ports are watched.
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

// PortContested is what a sandbox's owner is told: more than one program
// listens on one of its listen ports, so one may be taking the other's
// loopback connections.
type PortContested struct {
	Port      int
	Listeners int
}

// sweepListeners tells each listening sandbox of a listen port of its that
// more than one program listens on, once while it stays so. It reads and
// tells; it never signals a process.
func sweepListeners() {
	watching.mu.Lock()
	var procs []*Process
	for p := range watching.procs {
		if p.listening() {
			procs = append(procs, p)
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
	pids := map[int]map[int]bool{}
	for _, l := range all {
		if pids[l.port] == nil {
			pids[l.port] = map[int]bool{}
		}
		pids[l.port][l.pid] = true
	}
	watching.mu.Lock()
	if watching.contested == nil {
		watching.contested = map[int]bool{}
	}
	var fresh []PortContested
	for port := range watching.contested {
		if len(pids[port]) < 2 {
			delete(watching.contested, port)
		}
	}
	for port, ps := range pids {
		if len(ps) >= 2 && !watching.contested[port] {
			watching.contested[port] = true
			fresh = append(fresh, PortContested{Port: port, Listeners: len(ps)})
		}
	}
	watching.mu.Unlock()
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Port < fresh[j].Port })
	for _, p := range procs {
		p.listenWatchFound()
		for _, c := range fresh {
			if slices.Contains(p.policy.ListenPorts, c.Port) {
				if f := p.onPortContested.Load(); f != nil {
					(*f)(c)
				}
			}
		}
	}
}

// ListenersOn is how many processes listen on port now, read afresh; -1
// when the listeners cannot be read.
func ListenersOn(port int) int {
	all, ok := readListenersWithin(0)
	if !ok {
		return -1
	}
	seen := map[int]bool{}
	for _, l := range all {
		if l.port == port {
			seen[l.pid] = true
		}
	}
	return len(seen)
}
