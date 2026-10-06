package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
)

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"":         0,
		"max":      Max,
		"MAX":      Max,
		"4096":     4096,
		"64K":      64 << 10,
		"64KB":     64 << 10,
		"64KiB":    64 << 10,
		"512M":     512 << 20,
		"512m":     512 << 20,
		"2G":       2 << 30,
		"1TiB":     1 << 40,
		"8388607T": 8388607 << 40,
	}
	for in, want := range good {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if got, err := ParseSize(" 8M\n"); err != nil || got != 8<<20 {
		t.Errorf("ParseSize with spaces = %d, %v", got, err)
	}
	for _, in := range []string{"-1", "1.5G", "M", "10X", "10iB", "10MM", "10 M", "8388608T", "99999999999999999999", "0x10"} {
		if got, err := ParseSize(in); !errors.Is(err, ErrInvalidLimits) {
			t.Errorf("ParseSize(%q) = %d, %v; want ErrInvalidLimits", in, got, err)
		}
	}
}

func TestParseIOMax(t *testing.T) {
	d, err := ParseIOMax("8:16 rbps=2M wbps=max riops=100")
	want := IOMax{Major: 8, Minor: 16, RBps: 2 << 20, WBps: Max, RIOps: 100}
	if err != nil || d != want {
		t.Fatalf("ParseIOMax = %+v, %v; want %+v", d, err, want)
	}
	if got := d.line(); got != "8:16 rbps=2097152 wbps=max riops=100" {
		t.Errorf("line = %q", got)
	}
	for _, in := range []string{"", "8:16", "8 rbps=1", "8:x rbps=1", "8:0 rbps", "8:0 rbps=0", "8:0 speed=1", "8:0 rbps=-5", "4294967296:0 rbps=1"} {
		if _, err := ParseIOMax(in); !errors.Is(err, ErrInvalidLimits) {
			t.Errorf("ParseIOMax(%q) err = %v; want ErrInvalidLimits", in, err)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []Limits{
		{},
		{CPUPercent: 50, MemoryMax: 64 << 20, PidsMax: 128, OOMGroup: true},
		{MemoryMax: Max, SwapMax: Max, PidsMax: Max},
		{IO: []IOMax{{Major: 8, WBps: 1 << 20}, {Major: 8, Minor: 1, RIOps: Max}}},
	}
	for _, l := range ok {
		if err := l.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", l, err)
		}
	}
	bad := []Limits{
		{CPUPercent: -1},
		{CPUPercent: maxCPUPercent + 1},
		{MemoryMax: 1024},
		{MemoryMax: -2},
		{SwapMax: -2},
		{PidsMax: -2},
		{IO: []IOMax{{Major: 8}}},
		{IO: []IOMax{{Major: 8, RBps: -3}}},
		{IO: []IOMax{{Major: 8, RBps: 1}, {Major: 8, WBps: 1}}},
	}
	for _, l := range bad {
		if err := l.Validate(); !errors.Is(err, ErrInvalidLimits) {
			t.Errorf("Validate(%+v) = %v; want ErrInvalidLimits", l, err)
		}
	}
}

func TestSettings(t *testing.T) {
	l := Limits{CPUPercent: 150, MemoryMax: 64 << 20, PidsMax: 32, OOMGroup: true,
		IO: []IOMax{{Major: 8, WBps: 1024}}}
	want := []setting{
		{"cpu.max", "150000 100000"},
		{"memory.oom.group", "1"},
		{"memory.max", "67108864"},
		{"memory.swap.max", "0"}, // a memory bound gets no swap unless asked
		{"pids.max", "32"},
		{"io.max", "8:0 wbps=1024"},
	}
	if got := l.settings(); !reflect.DeepEqual(got, want) {
		t.Errorf("settings =\n%v\nwant\n%v", got, want)
	}
	if got := l.controllers(); !reflect.DeepEqual(got, []string{"cpu", "memory", "pids", "io"}) {
		t.Errorf("controllers = %v", got)
	}
	// Unbounded memory leaves swap alone; an explicit swap bound is kept.
	if got := (Limits{MemoryMax: Max}).settings(); !reflect.DeepEqual(got, []setting{{"memory.max", "max"}}) {
		t.Errorf("max memory settings = %v", got)
	}
	if got := (Limits{SwapMax: 1 << 20}).settings(); !reflect.DeepEqual(got, []setting{{"memory.swap.max", "1048576"}}) {
		t.Errorf("swap-only settings = %v", got)
	}
	if got := (Limits{}).settings(); len(got) != 0 {
		t.Errorf("zero limits write %v", got)
	}
}

func TestEventsAndProcParsing(t *testing.T) {
	ev := eventsFrom("low 0\nhigh 2\nmax 7\noom 1\noom_kill 3\noom_group_kill 1\n", "max 12\n")
	want := Events{MemoryHigh: 2, MemoryMax: 7, OOM: 1, OOMKill: 3, OOMGroupKill: 1, PidsMax: 12}
	if ev != want {
		t.Errorf("events = %+v; want %+v", ev, want)
	}
	if ev := eventsFrom("", "garbage\nmax x\n"); ev != (Events{}) {
		t.Errorf("unreadable events = %+v", ev)
	}

	own, err := ownPath("12:pids:/old\n0::/user.slice/user-1000.slice/app.scope\n")
	if err != nil || own != "/user.slice/user-1000.slice/app.scope" {
		t.Errorf("ownPath = %q, %v", own, err)
	}
	if _, err := ownPath("12:pids:/old\n"); err == nil {
		t.Error("ownPath accepted a v1-only list")
	}

	mi := "24 1 0:22 / /proc rw - proc proc rw\n" +
		`35 24 0:30 /ns /sys/fs/cgroup\040x rw,nosuid - cgroup2 cgroup2 rw` + "\n"
	point, root, err := mountOf(mi)
	if err != nil || point != "/sys/fs/cgroup x" || root != "/ns" {
		t.Errorf("mountOf = %q %q %v", point, root, err)
	}
	if _, _, err := mountOf("24 1 0:22 / /proc rw - proc proc rw\n"); err == nil {
		t.Error("mountOf found cgroup2 where there is none")
	}

	cases := []struct{ point, root, own, want string }{
		{"/sys/fs/cgroup", "/", "/a/b", "/sys/fs/cgroup/a/b"},
		{"/sys/fs/cgroup", "/", "/", "/sys/fs/cgroup"},
		{"/c", "/ns", "/ns/a", "/c/a"},
		{"/c", "/ns", "/ns", "/c"},
	}
	for _, c := range cases {
		if got, err := dirOf(c.point, c.root, c.own); err != nil || got != c.want {
			t.Errorf("dirOf(%q,%q,%q) = %q, %v; want %q", c.point, c.root, c.own, got, err, c.want)
		}
	}
	for _, own := range []string{"/nsx/a", "/other"} {
		if _, err := dirOf("/c", "/ns", own); err == nil {
			t.Errorf("dirOf accepted %q outside the mount root", own)
		}
	}

	if got := pids("12\n\n7\nx\n0\n"); !reflect.DeepEqual(got, []int{12, 7}) {
		t.Errorf("pids = %v", got)
	}
}

func TestValidName(t *testing.T) {
	for _, id := range []string{"a", "call_01", "call-ABC.9", "x"} {
		if !validName(id) {
			t.Errorf("validName(%q) = false", id)
		}
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	for _, id := range []string{"", ".", "..", ".hidden", "a/b", "../x", "a b", "memory.max\n", string(long), "é"} {
		if validName(id) {
			t.Errorf("validName(%q) = true", id)
		}
	}
}

// fakeCgroup is a directory laid out like a cgroup with the given files.
func fakeCgroup(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMissingControllerFailsClosed(t *testing.T) {
	dir := fakeCgroup(t, map[string]string{
		"cgroup.controllers":     "memory pids\n",
		"cgroup.subtree_control": "",
	})
	b := &Base{dir: dir}
	_, err := b.NewSandbox("s1", Limits{CPUPercent: 50})
	if !errors.Is(err, ErrNotDelegated) {
		t.Fatalf("NewSandbox without cpu delegated = %v; want ErrNotDelegated", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sandboxPrefix+"s1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused sandbox left its directory: %v", err)
	}
	if _, err := b.NewSandbox("bad/id", Limits{}); err == nil {
		t.Error("NewSandbox accepted a path as its id")
	}
	if _, err := b.NewSandbox("s2", Limits{MemoryMax: 10}); !errors.Is(err, ErrInvalidLimits) {
		t.Errorf("NewSandbox with a tiny memory bound = %v", err)
	}
}

func TestMissingLimitFileFailsClosed(t *testing.T) {
	dir := fakeCgroup(t, map[string]string{"memory.max": "max\n"})
	err := apply(dir, Limits{MemoryMax: 64 << 20})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("apply with no memory.swap.max = %v; want a not-exist error", err)
	}
	if got := readFile(filepath.Join(dir, "memory.max")); got != "67108864" {
		t.Errorf("memory.max = %q", got)
	}
}

func TestDiscoverElsewhere(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux discovers for real")
	}
	if _, err := Discover(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Discover = %v; want ErrUnsupported", err)
	}
}

func TestLeafRefusedWhenControllerDropped(t *testing.T) {
	dir := fakeCgroup(t, map[string]string{"cgroup.controllers": "pids\n"})
	s := &Sandbox{node: node{dir}, ctrls: []string{"memory", "pids"}, leaves: map[string]*Leaf{}}
	if _, err := s.NewLeaf("c1", Limits{}); !errors.Is(err, ErrNotDelegated) {
		t.Fatalf("NewLeaf with memory turned off = %v; want ErrNotDelegated", err)
	}
	if _, err := os.Stat(filepath.Join(dir, leafPrefix+"c1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused leaf was made: %v", err)
	}
}

// A cgroup deleted while it is removed again is gone, not an error.
func TestGone(t *testing.T) {
	for _, err := range []error{fs.ErrNotExist, fmt.Errorf("kill: %w", syscall.ENODEV)} {
		if !gone(err) {
			t.Errorf("gone(%v) = false", err)
		}
	}
	if gone(syscall.EBUSY) {
		t.Error("EBUSY is not gone")
	}
}
