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

// HostHolds reports whether a program outside every sandbox listens on
// port now, or the listeners cannot be read: a loopback address there is
// never a sandboxed server's. It reads them afresh.
func HostHolds(port int) bool {
	all, ok := readListenersWithin(0)
	if !ok {
		return true
	}
	for _, l := range all {
		if l.port == port && !confined(l.pid) {
			return true
		}
	}
	return false
}

// confined reports whether pid runs under a sandbox-exec profile: sandboxed,
// but not an App Store app's own sandbox.
func confined(pid int) bool {
	sandboxed, known := processSandboxed(pid)
	return known && sandboxed && !appSandboxed(pid)
}

// appSandboxed reports whether pid's program is signed into the App
// Sandbox, so it is an app of the person's, not a command of Abhed's.
func appSandboxed(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-d", "--entitlements", "-", "--xml", strconv.Itoa(pid)).Output() // #nosec G204 -- a process number
	return err == nil && strings.Contains(string(out), "<key>com.apple.security.app-sandbox</key><true/>")
}

// Listening processes are watched: a program of the person's that starts
// listening on a port a sandboxed command already holds (a wildcard bind
// with SO_REUSEADDR succeeds beside it) would lose its loopback connections
// to the command, so at each refresh such a command's listener is ended
// and the sandboxes watching are told.

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

// sweepListeners ends each confined listener on a port a host program now
// listens on too, within a watched sandbox's listen ports.
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
	all, ok := readListeners()
	if !ok {
		return
	}
	for _, e := range takenOver(all, ports, confined) {
		_ = syscall.Kill(e.pid, syscall.SIGKILL)
		for _, p := range procs {
			if f := p.onListenerEnded.Load(); f != nil {
				(*f)(e.port, e.pid)
			}
		}
	}
	// Read again next time: what was ended is gone.
	listeners.mu.Lock()
	listeners.at = time.Time{}
	listeners.mu.Unlock()
}

// takenOver are the confined listeners on a watched port that a host
// program listens on too.
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
