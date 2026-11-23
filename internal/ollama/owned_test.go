package ollama

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// freshKeep gives the test a keeper of its own and a clock it moves.
func freshKeep(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	oldKeep, oldNow := keep, nowFunc
	keep = &keeper{servers: map[int]Owned{}}
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { StopOwned("test end"); keep, nowFunc = oldKeep, oldNow })
	return &now
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	for range 200 {
		if !alive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still running", pid)
}

// stubServer launches a stub "ollama serve" as launch does, never the real one.
func stubServer(t *testing.T) int {
	t.Helper()
	bin := t.TempDir()
	fakeBin(t, bin, "ollama", "exec sleep 60")
	pid, _, err := launch(filepath.Join(bin, "ollama"), []string{"serve"}, []string{"PATH=/usr/bin:/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestStopOwnedStopsOnlyItsOwnServer(t *testing.T) {
	freshKeep(t)
	mine := stubServer(t)
	// A server this process did not launch: started here only to stand for
	// someone else's, and stopped by this test by its own pid afterwards.
	other := exec.Command("/bin/sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	var said []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); said = append(said, s); mu.Unlock() }
	Own(mine, "http://127.0.0.1:1", t.TempDir(), 0, note)
	Own(other.Process.Pid, "http://127.0.0.1:2", t.TempDir(), 0, note)
	stopped := StopOwned("exiting")
	if len(stopped) != 1 || stopped[0] != mine {
		t.Fatalf("stopped %v, want only %d", stopped, mine)
	}
	waitGone(t, mine)
	if !alive(other.Process.Pid) {
		t.Fatal("a server this process did not launch was stopped")
	}
	if len(said) != 1 || !strings.Contains(said[0], "exiting") {
		t.Fatalf("said %q", said)
	}
	if len(OwnedServers()) != 0 {
		t.Fatal("owned list not emptied")
	}
}

func TestIdleStopWaitsForUse(t *testing.T) {
	now := freshKeep(t)
	state := t.TempDir()
	pid := stubServer(t)
	Own(pid, "http://127.0.0.1:1", state, 10*time.Minute, func(string) {})
	*now = now.Add(9 * time.Minute)
	if !keep.check() || !alive(pid) {
		t.Fatal("stopped before the idle time")
	}
	NoteUse("http://127.0.0.1:1/v1")
	*now = now.Add(9 * time.Minute)
	if !keep.check() || !alive(pid) {
		t.Fatal("stopped though a session used it")
	}
	// Another process's use, through the shared file, also counts.
	*now = now.Add(5 * time.Minute)
	touch(useFile(state), *now)
	*now = now.Add(5 * time.Minute)
	if !keep.check() || !alive(pid) {
		t.Fatal("stopped though another Abhed used it")
	}
	*now = now.Add(11 * time.Minute)
	if keep.check() {
		t.Fatal("still owned after it went idle")
	}
	waitGone(t, pid)
}

func TestStartOwnsOnlyALaunchedChild(t *testing.T) {
	freshKeep(t)
	api := newFakeAPI(t)
	bin := t.TempDir()
	marker := filepath.Join(bin, "started")
	fakeBin(t, bin, "ollama", "touch "+marker+"\nexec sleep 30")
	env := testEnv(t, bin)
	go func() {
		for range 200 {
			if _, err := os.Stat(marker); err == nil {
				api.up.Store(true)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	tg := api.target("qwen3:8b")
	a := Assist{Env: env, Mode: ModeAsk, Admin: true, Surface: "acp", IdleStop: 10 * time.Minute}
	st := env.Check(context.Background(), tg)
	s, p := a.Start(context.Background(), tg, st, "asked")
	if p != nil {
		t.Fatal(p.Text())
	}
	if !s.Owned || s.StopAfterMinutes != 10 || !strings.Contains(s.OwnedNotice(), "10 minutes") {
		t.Fatalf("started %+v", s)
	}
	if got := OwnedServers(); len(got) != 1 || got[0].PID != s.PID {
		t.Fatalf("owned %+v", got)
	}
	// A server already answering is not started, so not owned.
	freshKeep(t)
	s2, _ := a.Start(context.Background(), tg, env.Check(context.Background(), tg), "asked")
	if s2.Owned || len(OwnedServers()) != 0 {
		t.Fatalf("owned one it did not start: %+v", s2)
	}
	killPID(t, s.PID)
}

func TestNoAutoStartIsNeverAlways(t *testing.T) {
	env := testEnv(t, t.TempDir())
	if err := env.SetStartAlways(true, "/usr/local/bin/ollama"); err != nil {
		t.Fatal(err)
	}
	a := Assist{Env: env}
	if !a.StartAlways("/usr/local/bin/ollama") {
		t.Fatal("always not honoured with auto start on")
	}
	a.NoAutoStart = true
	if a.StartAlways("/usr/local/bin/ollama") {
		t.Fatal("auto_start false still starts without asking")
	}
}

// A request still running counts as use however long it takes.
func TestIdleStopWaitsForARequestInFlight(t *testing.T) {
	now := freshKeep(t)
	pid := stubServer(t)
	Own(pid, "http://127.0.0.1:1", t.TempDir(), 10*time.Minute, func(string) {})
	end := Using("http://127.0.0.1:1/v1")
	*now = now.Add(30 * time.Minute)
	if !keep.check() || !alive(pid) {
		t.Fatal("stopped while a request was running")
	}
	end()
	*now = now.Add(9 * time.Minute)
	if !keep.check() || !alive(pid) {
		t.Fatal("stopped too soon after the request ended")
	}
	*now = now.Add(2 * time.Minute)
	if keep.check() {
		t.Fatal("not stopped once idle")
	}
	waitGone(t, pid)
}
