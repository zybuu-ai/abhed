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
// be told from two programs. Each program is named by its sorted sockets,
// or by its process where lsof gives no socket, and carries its processes:
// a told program is the same one only with the same sockets and a process
// in common, so a server recycling the workers that share its socket keeps
// its master and stays one, and a new program on a reused socket address
// is new.
func programs(all []listener) map[int][]program {
	byPort := map[int][]listener{}
	for _, l := range all {
		byPort[l.port] = append(byPort[l.port], l)
	}
	out := map[int][]program{}
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
		var progs []program
		for _, m := range members {
			var socks []string
			var pids []int
			for _, x := range m {
				if strings.HasPrefix(x, "s") {
					socks = append(socks, x)
				} else if pid, err := strconv.Atoi(x[1:]); err == nil {
					pids = append(pids, pid)
				}
			}
			if len(socks) == 0 {
				socks = m
			}
			sort.Strings(socks)
			sort.Ints(pids)
			progs = append(progs, program{name: strings.Join(socks, ","), pids: pids})
		}
		sort.Slice(progs, func(i, j int) bool { return progs[i].name < progs[j].name })
		out[port] = progs
	}
	return out
}

// program is one program listening on a port: its name, from its sockets,
// and its processes.
type program struct {
	name string
	pids []int
}

// Listening processes are watched, and nothing more. A program of the
// person's that starts listening on a listen port a sandboxed command
// already holds (a wildcard bind with SO_REUSEADDR succeeds beside it) can
// lose its loopback connections to the command. Abhed cannot prove which
// of two listeners is a sandbox's, so it signals neither: it tells every
// sandbox that may listen on that port of each program that joins, the
// record and the log under a per-port bound, and a new command's profile
// leaves the port out, as it is busy then.

var watching struct {
	mu    sync.Mutex
	procs map[*Process]bool
	once  sync.Once
	// sweep is held by a sweep and by a flush, so a flush waits for a sweep
	// in flight and nothing it held back is left after. The owner callbacks
	// run under it, so a callback must never close or flush a sandbox: that
	// would wait on itself.
	sweep sync.Mutex
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

// unwatchListeners takes p out, as it closes, recording first what its
// windows still hold.
func unwatchListeners(p *Process) {
	flushListenRecords(p)
}

// FlushListenRecords records now, whatever the windows' age, the joins
// sb's windows still hold, and stops watching sb: as its session ends,
// before the end is recorded.
func FlushListenRecords(sb Sandbox) {
	if p, ok := sb.(*Process); ok {
		flushListenRecords(p)
	}
}

// flushListenRecords waits for a sweep in flight, takes p out of the watch
// and records what its windows hold.
func flushListenRecords(p *Process) {
	watching.sweep.Lock()
	defer watching.sweep.Unlock()
	watching.mu.Lock()
	delete(watching.procs, p)
	var out []notice
	now := time.Now()
	for port, w := range p.windows {
		if w.pending == 0 {
			continue
		}
		out = append(out, notice{p, PortContested{Port: port, Listeners: w.listeners, Joined: w.pending, At: w.at, Record: true}})
		w.last, w.pending = now, 0
	}
	watching.mu.Unlock()
	for _, n := range out {
		if f := n.p.onPortContested.Load(); f != nil {
			(*f)(n.c)
		}
	}
}

// PortContested is what a sandbox's owner is told: more than one program
// listens on one of its listen ports, so one may be taking the other's
// loopback connections.
type PortContested struct {
	Port      int
	Listeners int
	// Notify is set when programs joined in this read: the person is told
	// now. Joined counts the programs joined since the port was last
	// recorded, starting again after each record, and At the latest join.
	// Listeners are the programs listening at that latest join, which a
	// record at a window's end keeps though the port may have cleared.
	Notify bool
	Joined int
	At     time.Time
	// Record is set when the notice is to be recorded and logged now: the
	// port's first, then at most once per listenRecordWindow, with Joined.
	Record bool
}

// listenRecordWindow is the least time between two records of one port's
// contest to one sandbox. The person is told of every new program at once;
// only the copies in the record and the log are bounded, and a window's
// joins are recorded when it ends, even if the port has cleared.
const listenRecordWindow = 30 * time.Second

// recordWindow is one port's record bound for one sandbox, kept across the
// port clearing, so clearing and joining again cannot reset it.
type recordWindow struct {
	last      time.Time // when the port was last recorded; zero for never
	pending   int       // programs joined since
	at        time.Time // the latest of them
	listeners int       // the programs listening at the latest join
}

// join takes fresh programs joining at now, with listeners listening, and
// says whether to record now, and how many joined since the last record.
func (w *recordWindow) join(fresh, listeners int, now time.Time) (bool, int) {
	w.pending += fresh
	w.at, w.listeners = now, listeners
	if !w.last.IsZero() && now.Sub(w.last) < listenRecordWindow {
		return false, w.pending
	}
	joined := w.pending
	w.last, w.pending = now, 0
	return true, joined
}

// due says whether a window has ended with joins not yet recorded, and
// takes them.
func (w *recordWindow) due(now time.Time) (bool, int, time.Time) {
	if w.pending == 0 || now.Sub(w.last) < listenRecordWindow {
		return false, 0, time.Time{}
	}
	joined, at := w.pending, w.at
	w.last, w.pending = now, 0
	return true, joined, at
}

// recordAt is the notice a window's end records: its joins, with the
// programs listening at the latest of them, as the port may have cleared.
func (w *recordWindow) recordAt(port, joined int, at time.Time) PortContested {
	return PortContested{Port: port, Listeners: w.listeners, Joined: joined, At: at, Record: true}
}

// spent reports a window with nothing pending that has ended: forgotten.
func (w *recordWindow) spent(now time.Time) bool {
	return w.pending == 0 && now.Sub(w.last) >= listenRecordWindow
}

// sweepListeners tells each listening sandbox of a listen port of its that
// more than one program listens on: within one sweep whenever a program it
// has not told of joins the port, at most one notice per port per sweep,
// which the person sees at once. The record and the log take the port's
// first notice at once and then at most one per listenRecordWindow, with
// the joins in between, so a command that keeps starting listeners cannot
// flood them. It reads afresh and tells; it never signals a process.
func sweepListeners() {
	watching.sweep.Lock()
	defer watching.sweep.Unlock()
	watching.mu.Lock()
	var procs []*Process
	var idle []notice
	now := time.Now()
	for p := range watching.procs {
		if p.listening() {
			procs = append(procs, p)
			continue
		}
		// Off, it starts afresh: turned on again, it is told again. Its
		// windows still end, and record what they hold.
		p.notices = nil
		for port, w := range p.windows {
			if due, joined, at := w.due(now); due {
				idle = append(idle, notice{p, w.recordAt(port, joined, at)})
			} else if w.spent(now) {
				delete(p.windows, port)
			}
		}
	}
	watching.mu.Unlock()
	for _, n := range idle {
		if f := n.p.onPortContested.Load(); f != nil {
			(*f)(n.c)
		}
	}
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
	var tell []notice
	watching.mu.Lock()
	for _, p := range procs {
		if !watching.procs[p] {
			continue // closed while the listeners were read
		}
		if p.notices == nil {
			p.notices = map[int]*portNotice{}
		}
		if p.windows == nil {
			p.windows = map[int]*recordWindow{}
		}
		for _, port := range p.policy.ListenPorts {
			st, fresh := noteContest(p.notices[port], byPort[port])
			if st == nil {
				delete(p.notices, port)
			} else {
				p.notices[port] = st
			}
			w := p.windows[port]
			if fresh > 0 {
				if w == nil {
					w = &recordWindow{}
					p.windows[port] = w
				}
				record, joined := w.join(fresh, len(byPort[port]), now)
				tell = append(tell, notice{p, PortContested{Port: port, Listeners: len(byPort[port]), Notify: true, Joined: joined, At: now, Record: record}})
				continue
			}
			if w == nil {
				continue
			}
			if due, joined, at := w.due(now); due {
				// The window's joins, recorded at its end, cleared or not.
				tell = append(tell, notice{p, w.recordAt(port, joined, at)})
			} else if w.spent(now) {
				delete(p.windows, port)
			}
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

// notice is one callback to a sandbox's owner, made after the lock.
type notice struct {
	p *Process
	c PortContested
}

// portNotice is what a sandbox was told of one listen port: the programs
// told of, by name with their processes.
type portNotice struct {
	told map[string][]int
}

// noteContest takes one read of a port's programs into what a sandbox was
// told, and says how many programs to tell it of now: those on a contested
// port it was not told of; a nil result forgets the port.
// A program is told of only if one of that name with a process in common
// was: the told processes follow it from read to read, so a server that
// recycles its workers stays told, and a new program on a socket address
// another left is new. A program gone is forgotten.
func noteContest(st *portNotice, progs []program) (*portNotice, int) {
	known := func(p program) bool {
		for _, pid := range st.told[p.name] {
			if slices.Contains(p.pids, pid) {
				return true
			}
		}
		return false
	}
	if st != nil {
		kept := map[string][]int{}
		for _, p := range progs {
			if known(p) {
				kept[p.name] = p.pids
			}
		}
		st.told = kept
	}
	if len(progs) < 2 {
		if st != nil && len(st.told) == 0 {
			return nil, 0
		}
		return st, 0
	}
	if st == nil {
		st = &portNotice{told: map[string][]int{}}
	}
	fresh := 0
	for _, p := range progs {
		if !known(p) {
			fresh++
		}
	}
	if fresh == 0 {
		return st, 0
	}
	for _, p := range progs {
		st.told[p.name] = p.pids
	}
	return st, fresh
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
