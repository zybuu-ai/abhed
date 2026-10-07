package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests below need a delegated cgroup and run only with
// ABHED_REQUIRE_FENCE=1, which also turns a missing delegation into a
// failure instead of a skip, for example:
//
//	systemd-run --user --scope -p Delegate=yes env ABHED_REQUIRE_FENCE=1 go test ./internal/fence/cgroup

// helperEnv makes the test binary act as a helper process instead of testing.
const helperEnv = "ABHED_CGROUP_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "hog":
		hog()
	case "count":
		count(os.Getenv("ABHED_CGROUP_COUNT_FILE"))
	}
	os.Exit(m.Run())
}

// hog takes memory a mebibyte at a time, touching every page, up to 4 GiB.
func hog() {
	var held [][]byte
	for range 4096 {
		b := make([]byte, 1<<20)
		for i := range b {
			if i%4096 == 0 {
				b[i] = 1
			}
		}
		held = append(held, b)
	}
	fmt.Println("hog survived", len(held))
	os.Exit(3)
}

// count writes a growing number to name, in place, every millisecond.
func count(name string) {
	f, err := os.Create(name) // #nosec G304 -- a test's temp file
	if err != nil {
		os.Exit(2)
	}
	for n := 1; ; n++ {
		_, _ = f.WriteAt(fmt.Appendf(nil, "%020d", n), 0)
		time.Sleep(time.Millisecond)
	}
}

func requireBase(t *testing.T) *Base {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("needs a delegated cgroup; set ABHED_REQUIRE_FENCE=1 to run")
	}
	b, err := Discover()
	if err != nil {
		t.Fatalf("ABHED_REQUIRE_FENCE=1 but %v", err)
	}
	return b
}

func newSandbox(t *testing.T, l Limits) *Sandbox {
	t.Helper()
	b := requireBase(t)
	id := strings.Map(func(r rune) rune {
		if validName(string(r)) {
			return r
		}
		return '-'
	}, fmt.Sprintf("t%d-%s", os.Getpid(), t.Name()))
	id = id[:min(len(id), 64)]
	s, err := b.NewSandbox(id, l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.Remove(ctx); err != nil {
			t.Errorf("Remove: %v", err)
		}
		if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
			t.Errorf("sandbox %s still there after Remove: %v", s.Path(), err)
		}
	})
	return s
}

func newLeaf(t *testing.T, s *Sandbox, id string) *Leaf {
	t.Helper()
	l, err := s.NewLeaf(id, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// start runs cmd placed in leaf, and kills the leaf and reaps cmd at the end.
func start(t *testing.T, leaf *Leaf, cmd *exec.Cmd) {
	t.Helper()
	if err := leaf.Place(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = leaf.Kill(ctx)
		_ = cmd.Wait()
	})
}

func readInt(t *testing.T, name string) int64 {
	t.Helper()
	s := strings.TrimSpace(readFile(name))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q", name, s)
	}
	return n
}

func eventually(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if poll(ctx, ok) != nil {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestPlacedAtStart(t *testing.T) {
	s := newSandbox(t, Limits{PidsMax: 16})
	leaf := newLeaf(t, s, "place")
	out := &strings.Builder{}
	cmd := exec.Command("cat", "/proc/self/cgroup")
	cmd.Stdout = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	start(t, leaf, cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	want := "/" + filepath.Base(s.Path()) + "/" + leafPrefix + "place\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Errorf("child's cgroup = %q; want it to end %q", out.String(), want)
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("Place replaced SysProcAttr instead of merging into it")
	}
}

func TestForkBombHeldAtPidsMax(t *testing.T) {
	const limit = 48
	s := newSandbox(t, Limits{PidsMax: limit, MemoryMax: 256 << 20, OOMGroup: true})
	if got := strings.TrimSpace(readFile(filepath.Join(s.Path(), "pids.max"))); got != strconv.Itoa(limit) {
		t.Fatalf("pids.max = %q before the bomb; refusing to start it", got)
	}
	leaf := newLeaf(t, s, "bomb")
	cmd := exec.Command("sh", "-c", "b() { b | b & }; b; sleep 600")
	start(t, leaf, cmd)

	eventually(t, "a refused fork", 20*time.Second, func() bool {
		ev, err := s.Events()
		return err == nil && ev.PidsMax > 0
	})
	var peak int64
	for range 100 {
		if n := readInt(t, filepath.Join(s.Path(), "pids.current")); n > peak {
			peak = n
		}
		time.Sleep(10 * time.Millisecond)
	}
	// pids.current can read one over while a refused clone is unwound (seen on arm64 CI).
	if peak > limit+1 {
		t.Fatalf("pids.current reached %d past pids.max %d", peak, limit)
	}
	ev, _ := s.Events()
	t.Logf("fork bomb held: peak %d of %d pids, %d forks refused", peak, limit, ev.PidsMax)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := leaf.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	if leaf.populated() {
		t.Fatal("leaf still populated after Kill")
	}
	// A pid is uncharged when it is reaped, not when it exits: reap ours.
	_ = cmd.Wait()
	eventually(t, "pids.current to drop to 0", 5*time.Second, func() bool {
		return readInt(t, filepath.Join(s.Path(), "pids.current")) == 0
	})
}

func TestMemoryHogOOMKilled(t *testing.T) {
	s := newSandbox(t, Limits{MemoryMax: 64 << 20, PidsMax: 64, OOMGroup: true})
	leaf := newLeaf(t, s, "hog")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$") // #nosec G204 -- this test binary as a helper
	cmd.Env = append(os.Environ(), helperEnv+"=hog")
	out := &strings.Builder{}
	cmd.Stdout = out
	start(t, leaf, cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the memory hog was not stopped")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("hog ended with %v, output %q; want SIGKILL", err, out.String())
	}
	ev, err := leaf.Events()
	if err != nil {
		t.Fatal(err)
	}
	if ev.OOMKill == 0 {
		t.Errorf("leaf events %+v: no OOM kill counted", ev)
	}
	sev, _ := s.Events()
	if sev.OOM == 0 || sev.MemoryMax == 0 {
		t.Errorf("sandbox events %+v: memory.max not counted", sev)
	}
	t.Logf("memory hog OOM-killed: leaf %+v sandbox %+v", ev, sev)
}

func TestFreezeAndThaw(t *testing.T) {
	s := newSandbox(t, Limits{PidsMax: 32})
	leaf := newLeaf(t, s, "freeze")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "count")
	cmd := exec.Command(exe, "-test.run=^$") // #nosec G204 -- this test binary as a helper
	cmd.Env = append(os.Environ(), helperEnv+"=count", "ABHED_CGROUP_COUNT_FILE="+file)
	start(t, leaf, cmd)
	value := func() int64 {
		n, _ := strconv.ParseInt(strings.TrimSpace(readFile(file)), 10, 64)
		return n
	}
	eventually(t, "the counter to count", 10*time.Second, func() bool { return value() > 10 })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := leaf.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	frozenAt := value()
	time.Sleep(300 * time.Millisecond)
	if v := value(); v != frozenAt {
		t.Fatalf("counter moved from %d to %d while frozen", frozenAt, v)
	}
	if err := leaf.Thaw(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the counter to resume", 10*time.Second, func() bool { return value() > frozenAt+10 })
	t.Logf("frozen at %d for 300ms; resumed to %d", frozenAt, value())
}

func TestKillEndsTree(t *testing.T) {
	for _, viaFile := range []bool{true, false} {
		t.Run(fmt.Sprintf("killfile=%v", viaFile), func(t *testing.T) {
			if !viaFile {
				useKillFile = false
				t.Cleanup(func() { useKillFile = true })
			}
			s := newSandbox(t, Limits{PidsMax: 64})
			leaf := newLeaf(t, s, "tree")
			// A child, a double-forked orphan, a new session, and a grandchild.
			cmd := exec.Command("sh", "-c",
				"sleep 600 & (sleep 600 &) ; setsid sleep 600 & sh -c 'sleep 600 & wait' & wait")
			start(t, leaf, cmd)
			var tree []int
			eventually(t, "the tree to start", 10*time.Second, func() bool {
				tree = leaf.allPids()
				return len(tree) >= 6
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := leaf.Kill(ctx); err != nil {
				t.Fatal(err)
			}
			if leaf.populated() {
				t.Fatal("leaf still populated after Kill")
			}
			_ = cmd.Wait()
			for _, p := range tree {
				if alive(p) {
					t.Errorf("process %d of the tree outlived Kill", p)
				}
			}
			t.Logf("killed a tree of %d processes", len(tree))
		})
	}
}

// alive reports whether pid runs: not gone and not a zombie.
func alive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	_, rest, _ := strings.Cut(string(stat), ") ")
	return !strings.HasPrefix(rest, "Z") && !strings.HasPrefix(rest, "X")
}

func TestRemoveCleansUp(t *testing.T) {
	b := requireBase(t)
	s, err := b.NewSandbox(fmt.Sprintf("t%d-remove", os.Getpid()), Limits{PidsMax: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Remove(context.Background()) }) // a second Remove is harmless
	leaf := newLeaf(t, s, "one")
	cmd := exec.Command("sleep", "600")
	if err := leaf.Place(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewLeaf("one", Limits{}); err == nil {
		t.Error("a second leaf with the same call id was made")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for _, p := range []string{leaf.Path(), s.Path()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
	if err := leaf.Place(exec.Command("true")); err == nil {
		t.Error("Place on a removed leaf succeeded")
	}
	if _, err := s.NewLeaf("two", Limits{}); err == nil {
		t.Error("NewLeaf on a removed sandbox succeeded")
	}
}

func TestLimitsWritten(t *testing.T) {
	s := newSandbox(t, Limits{CPUPercent: 50, MemoryMax: 128 << 20, PidsMax: 99, OOMGroup: true})
	want := map[string]string{
		"cpu.max":          "50000 100000",
		"memory.max":       "134217728",
		"memory.swap.max":  "0",
		"pids.max":         "99",
		"memory.oom.group": "1",
	}
	for f, v := range want {
		if got := strings.TrimSpace(readFile(filepath.Join(s.Path(), f))); got != v {
			t.Errorf("%s = %q; want %q", f, got, v)
		}
	}
	leaf, err := s.NewLeaf("budget", Limits{PidsMax: 5, MemoryMax: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(filepath.Join(leaf.Path(), "pids.max"))); got != "5" {
		t.Errorf("leaf pids.max = %q", got)
	}
}

func TestDelegatedRefuses(t *testing.T) {
	root := t.TempDir()
	if err := delegated(root); !errors.Is(err, ErrNotDelegated) || !strings.Contains(err.Error(), "Delegate=yes") {
		t.Errorf("a directory without cgroup.type = %v; want ErrNotDelegated naming how to delegate", err)
	}
	if os.Getuid() == 0 {
		return // root may write anything; the permission case needs a user
	}
	dir := fakeCgroup(t, map[string]string{"cgroup.type": "domain", "cgroup.procs": "", "cgroup.subtree_control": ""})
	if err := os.Chmod(filepath.Join(dir, "cgroup.procs"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := delegated(dir); !errors.Is(err, ErrNotDelegated) {
		t.Errorf("a read-only cgroup.procs = %v; want ErrNotDelegated", err)
	}
}

// In the delegated scope these tests run in, the manager confirms it.
func TestConfirmDelegatedInADelegatedScope(t *testing.T) {
	b := requireBase(t)
	if err := b.ConfirmDelegated(context.Background()); err != nil {
		t.Fatalf("a delegated scope: %v", err)
	}
}
