package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// listener is one TCP socket listening on the Mac, as one process holds
// it: sock is the kernel's address for the socket, which every process
// that inherited it shares (a server that forks workers).
type listener struct {
	port, pid int
	sock      string
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
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpnd").Output()
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

// parseListeners reads lsof -Fpnd: a p line names the process, an f line
// starts one of its files, whose d line is the socket's address and whose
// n lines are its addresses, n*:5432, n127.0.0.1:8080, n[::1]:8080.
func parseListeners(out string) []listener {
	var all []listener
	pid, sock := 0, ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
			sock = ""
		case strings.HasPrefix(line, "f"):
			sock = ""
		case strings.HasPrefix(line, "d"):
			sock = line[1:]
		case strings.HasPrefix(line, "n"):
			i := strings.LastIndexByte(line, ':')
			if i < 0 {
				continue
			}
			p, err := strconv.Atoi(line[i+1:])
			if err != nil || p <= 0 || p > 65535 {
				continue
			}
			l := listener{port: p, pid: pid, sock: sock}
			if !slices.Contains(all, l) {
				all = append(all, l)
			}
		}
	}
	return all
}

// servers counts the programs listening on each port, as programs groups
// them.
func servers(all []listener) map[int]int {
	out := map[int]int{}
	for port, ps := range programs(all) {
		out[port] = len(ps)
	}
	return out
}

// programs groups the listeners on each port into programs: sockets joined
// into one when a process holds both (a server on 127.0.0.1 and ::1) or one
// socket is held by several processes (a server and the workers it
// forked). Two programs, each with sockets of its own, are two. Sockets
// bound apart with SO_REUSEPORT by one program's unrelated processes cannot
// be told from two programs. Each program is named by its sorted sockets
// and processes, so the same program keeps its name from one read to the
// next and a new one, even on a reused socket address, gets another.
func programs(all []listener) map[int][]string {
	byPort := map[int][]listener{}
	for _, l := range all {
		byPort[l.port] = append(byPort[l.port], l)
	}
	out := map[int][]string{}
	for port, ls := range byPort {
		parent := map[string]string{}
		var find func(string) string
		find = func(x string) string {
			if parent[x] == "" || parent[x] == x {
				parent[x] = x
				return x
			}
			r := find(parent[x])
			parent[x] = r
			return r
		}
		union := func(a, b string) { parent[find(a)] = find(b) }
		for _, l := range ls {
			pidKey := "p" + strconv.Itoa(l.pid)
			if l.sock == "" {
				find(pidKey)
				continue
			}
			union("s"+l.sock, pidKey)
		}
		members := map[string][]string{}
		for x := range parent {
			r := find(x)
			members[r] = append(members[r], x)
		}
		var names []string
		for _, m := range members {
			sort.Strings(m)
			names = append(names, strings.Join(m, ","))
		}
		sort.Strings(names)
		out[port] = names
	}
	return out
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
	mu    sync.Mutex
	procs map[*Process]bool
	once  sync.Once
}

// listenNoticeCooldown is the least time between two notices of one port to
// one sandbox, so a contest that comes and goes does not flood the records.
var listenNoticeCooldown = time.Minute

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
// more than one program listens on. It tells within one sweep whenever a
// program it has not told of joins the port, whatever was told before;
// only the same programs coming back after the port cleared wait for
// listenNoticeCooldown. It reads afresh and tells; it never signals a
// process.
func sweepListeners() {
	watching.mu.Lock()
	var procs []*Process
	for p := range watching.procs {
		if p.listening() {
			procs = append(procs, p)
		} else {
			// Off, it starts afresh: turned on again, it is told again.
			p.notices = nil
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
	byPort := programs(all)
	type notice struct {
		p *Process
		c PortContested
	}
	var tell []notice
	now := time.Now()
	watching.mu.Lock()
	for _, p := range procs {
		if p.notices == nil {
			p.notices = map[int]*portNotice{}
		}
		for _, port := range p.policy.ListenPorts {
			progs := byPort[port]
			st := p.notices[port]
			if len(progs) < 2 {
				if st != nil && st.contested {
					st.contested, st.clearedAt = false, now
				}
				if st != nil && !st.contested && now.Sub(st.clearedAt) >= listenNoticeCooldown {
					delete(p.notices, port)
				}
				continue
			}
			if st == nil {
				st = &portNotice{told: map[string]bool{}}
				p.notices[port] = st
			}
			fresh := false
			for _, prog := range progs {
				fresh = fresh || !st.told[prog]
			}
			switch {
			case fresh:
				// A program not told of: told now, whatever the cooldown.
			case st.contested:
				continue
			case now.Sub(st.toldAt) < listenNoticeCooldown:
				// The same programs back too soon: told once the cooldown ends, if still so.
				continue
			}
			for _, prog := range progs {
				st.told[prog] = true
			}
			st.contested, st.toldAt = true, now
			tell = append(tell, notice{p, PortContested{Port: port, Listeners: len(progs)}})
		}
	}
	watching.mu.Unlock()
	for _, p := range procs {
		p.listenWatchFound()
	}
	for _, t := range tell {
		if f := t.p.onPortContested.Load(); f != nil {
			(*f)(t.c)
		}
	}
}

// portNotice is what a sandbox was told of one listen port: the programs
// told of, whether it is contested now, when it was last told, and when it
// last cleared.
type portNotice struct {
	told              map[string]bool
	contested         bool
	toldAt, clearedAt time.Time
}

// ListenersOn is how many programs listen on port now, read afresh: a
// server and the workers it forked count once. -1 when the listeners cannot
// be read.
func ListenersOn(port int) int {
	all, ok := readListenersWithin(0)
	if !ok {
		return -1
	}
	return servers(all)[port]
}

// ownTree reports whether every listener on port is a process this Abhed
// started or a descendant of one, by parent links; a process that left the
// tree counts as not.
func ownTree(all []listener, port int, parents map[int]int) bool {
	self, found := os.Getpid(), false
	for _, l := range all {
		if l.port != port {
			continue
		}
		found = true
		inside := false
		for pid, i := l.pid, 0; pid > 1 && i < 64; i++ {
			pp, ok := parents[pid]
			if !ok {
				break
			}
			if pp == self {
				inside = true
				break
			}
			pid = pp
		}
		if !inside {
			return false
		}
	}
	return found
}
