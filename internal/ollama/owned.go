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
	// lastMark is when the shared use file was last touched.
	lastMark time.Time
}

var keep = &keeper{servers: map[int]Owned{}}

// Seams for tests.
var (
	nowFunc  = time.Now
	stopFunc = stopLaunched
	// tickFor is how often the idle check runs for an idle time.
	tickFor = func(idle time.Duration) time.Duration {
		return min(max(idle/4, time.Second), 30*time.Second)
	}
	stopGrace = 10 * time.Second
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

// stopAll stops every owned server and says so.
func (k *keeper) stopAll(why string) []int {
	k.mu.Lock()
	servers := make([]Owned, 0, len(k.servers))
	for _, o := range k.servers {
		servers = append(servers, o)
	}
	notify := k.notify
	k.mu.Unlock()
	var stopped []int
	for _, o := range servers {
		ok, err := stopFunc(o.PID, stopGrace)
		k.mu.Lock()
		delete(k.servers, o.PID)
		k.mu.Unlock()
		switch {
		case err != nil:
			say(notify, fmt.Sprintf("Abhed could not stop the Ollama it started (pid %d): %v", o.PID, err))
		case ok:
			stopped = append(stopped, o.PID)
			say(notify, fmt.Sprintf("Abhed stopped the Ollama it started (pid %d): %s.", o.PID, why))
		}
	}
	return stopped
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
