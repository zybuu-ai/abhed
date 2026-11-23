package ollama

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// An `ollama serve` this process started itself is its to stop, and only
// that one: it is stopped once no session has used a local model for the
// idle time, and when this process exits. A server someone else started, or
// the macOS app (which hands off and is not this process's child), is never
// stopped. Use is shared between Abhed processes through a file in StateDir,
// so one window's server is not stopped while another window uses it.

// Owned is a server this process started and will stop.
type Owned struct {
	PID     int       `json:"pid"`
	API     string    `json:"api"`
	Started time.Time `json:"started"`
	// StopAfter is the idle time; 0 when it is stopped only at exit.
	StopAfter time.Duration `json:"-"`
}

// keeper holds the servers this process owns.
type keeper struct {
	mu       sync.Mutex
	servers  map[int]Owned
	idle     time.Duration
	lastUse  time.Time
	stateDir string
	notify   func(string)
	running  bool
	// inflight counts requests to a local model still running.
	inflight int
	// lastMark is when the shared use file was last touched.
	lastMark time.Time
}

var keep = &keeper{servers: map[int]Owned{}}

// Seams for tests.
var (
	nowFunc  = time.Now
	termFunc = termLaunched
	killFunc = killLaunched
	// tickFor is how often the idle check runs for an idle time.
	tickFor = func(idle time.Duration) time.Duration {
		return min(max(idle/4, time.Second), 30*time.Second)
	}
	// stopGrace is how long TERM is given before KILL: well inside the 3
	// seconds Studio gives its engine to exit before it kills it.
	stopGrace = 1500 * time.Millisecond
)

// useFile is touched whenever a local model is used, by any Abhed process.
func useFile(stateDir string) string { return filepath.Join(stateDir, "ollama-used") }

// Own makes pid, a server launch started in this process for api, this
// process's to stop: after idle with no local model used (never when idle is
// 0), and at StopOwned. notify hears what is stopped and why.
func Own(pid int, api, stateDir string, idle time.Duration, notify func(string)) Owned {
	keep.mu.Lock()
	defer keep.mu.Unlock()
	o := Owned{PID: pid, API: api, Started: nowFunc(), StopAfter: idle}
	keep.servers[pid] = o
	keep.idle, keep.stateDir = idle, stateDir
	if notify != nil {
		keep.notify = notify
	}
	keep.lastUse = o.Started
	if idle > 0 && !keep.running {
		keep.running = true
		go keep.watch(tickFor(idle))
	}
	return o
}

// OwnedServers lists the servers this process started and has not stopped.
func OwnedServers() []Owned {
	keep.mu.Lock()
	defer keep.mu.Unlock()
	out := make([]Owned, 0, len(keep.servers))
	for _, o := range keep.servers {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Using records a request to a local model starting now, and returns what
// records its end: while it runs the server is in use, however long it takes.
func Using(baseURL string) (end func()) {
	keep.mu.Lock()
	keep.inflight++
	keep.mu.Unlock()
	NoteUse(baseURL)
	var once sync.Once
	return func() {
		once.Do(func() {
			keep.mu.Lock()
			keep.inflight--
			keep.mu.Unlock()
			NoteUse(baseURL)
		})
	}
}

// NoteUse records that a session used a local model now: in this process,
// and in the shared file at most every few seconds, for other processes.
func NoteUse(string) {
	now := nowFunc()
	keep.mu.Lock()
	keep.lastUse = now
	dir := keep.stateDir
	mark := now.Sub(keep.lastMark) >= markEvery
	if mark {
		keep.lastMark = now
	}
	keep.mu.Unlock()
	if dir == "" {
		dir = defaultStateDir()
	}
	if mark && dir != "" {
		touch(useFile(dir), now)
	}
}

// markEvery bounds how often the shared use file is written.
const markEvery = 15 * time.Second

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".abhed", "state")
}

func touch(path string, at time.Time) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- Abhed's own state folder
	if err != nil {
		return
	}
	_ = f.Close()
	_ = os.Chtimes(path, at, at)
}

// lastSharedUse is when any Abhed process last used a local model.
func lastSharedUse(stateDir string) time.Time {
	if stateDir == "" {
		return time.Time{}
	}
	fi, err := os.Stat(useFile(stateDir))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (k *keeper) watch(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		if !k.check() {
			return
		}
	}
}

// check stops the owned servers once idle; false when none is left.
func (k *keeper) check() bool {
	k.mu.Lock()
	if len(k.servers) == 0 || k.idle <= 0 {
		k.running = false
		k.mu.Unlock()
		return false
	}
	if k.inflight > 0 {
		k.mu.Unlock()
		return true
	}
	last := k.lastUse
	if s := lastSharedUse(k.stateDir); s.After(last) {
		last = s
	}
	idle := k.idle
	k.mu.Unlock()
	if nowFunc().Sub(last) < idle {
		return true
	}
	k.stopAll(fmt.Sprintf("no session used a local model for %s", idle.Round(time.Second)))
	k.mu.Lock()
	left := len(k.servers) > 0
	if !left {
		k.running = false
	}
	k.mu.Unlock()
	return left
}

// stopAll stops every owned server and says so: TERM to each process
// group at once, a shared wait of stopGrace, then KILL to any left.
func (k *keeper) stopAll(why string) []int {
	k.mu.Lock()
	servers := make([]Owned, 0, len(k.servers))
	for _, o := range k.servers {
		servers = append(servers, o)
	}
	k.servers = map[int]Owned{}
	notify := k.notify
	k.mu.Unlock()
	var signalled []Owned
	for _, o := range servers {
		if termFunc(o.PID) {
			signalled = append(signalled, o)
		}
	}
	deadline := nowReal().Add(stopGrace)
	var stopped []int
	for _, o := range signalled {
		if !waitLaunched(o.PID, deadline) {
			killFunc(o.PID)
			waitLaunched(o.PID, nowReal().Add(time.Second))
		}
		stopped = append(stopped, o.PID)
		say(notify, fmt.Sprintf("Abhed stopped the Ollama it started (pid %d): %s.", o.PID, why))
	}
	return stopped
}

// nowReal is the wall clock, for waits, whatever nowFunc says.
func nowReal() time.Time { return time.Now() }

// TermOwned sends TERM to every server this process started, without
// waiting: the first thing done on a stop signal, so a kill that follows
// soon after still leaves no server running.
func TermOwned() {
	for _, o := range OwnedServers() {
		termFunc(o.PID)
	}
}

func say(notify func(string), msg string) {
	if notify != nil {
		notify(msg)
		return
	}
	fmt.Fprintln(os.Stderr, "abhed: "+msg)
}

// StopOwned stops every server this process started, as it exits; why is
// said with each. It returns their process ids.
func StopOwned(why string) []int {
	keep.mu.Lock()
	n := len(keep.servers)
	keep.mu.Unlock()
	if n == 0 {
		return nil
	}
	return keep.stopAll(why)
}

// OwnLaunchedForTests starts path as Start does and makes it this process's
// to stop. Test-only: it lets another package's test stand a stub in for
// ollama on the exit paths; nothing else calls it.
func OwnLaunchedForTests(path string, args []string, api, stateDir string, idle time.Duration) (int, error) {
	pid, _, err := launch(path, args, MinimalEnv(os.Environ()), nil)
	if err != nil {
		return 0, err
	}
	Own(pid, api, stateDir, idle, nil)
	return pid, nil
}
