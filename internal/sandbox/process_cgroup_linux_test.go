package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// With a delegated cgroup, each bubblewrap command runs in a leaf of its own
// bounded by max_procs and max_memory_mb, gone once the command ends.
func TestProcessCommandRunsInABoundedLeaf(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to run")
	}
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.MaxProcs, p.MaxMemoryMB = 40, 128
	s := NewProcess(p)
	available(t, s)
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := s.Command(ctx, ws, "sleep 3")
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if s.cg == nil || s.cg.sb == nil {
		t.Fatal("no cgroup was made")
	}
	leaves, _ := filepath.Glob(filepath.Join(s.cg.sb.Path(), "*", "pids.max"))
	if len(leaves) != 1 {
		t.Fatalf("leaves %v", leaves)
	}
	pids, _ := os.ReadFile(leaves[0])
	mem, _ := os.ReadFile(filepath.Join(filepath.Dir(leaves[0]), "memory.max"))
	if strings.TrimSpace(string(pids)) != "40" || strings.TrimSpace(string(mem)) != "134217728" {
		t.Fatalf("pids.max %q memory.max %q", pids, mem)
	}
	procs, _ := os.ReadFile(filepath.Join(filepath.Dir(leaves[0]), "cgroup.procs"))
	if strings.TrimSpace(string(procs)) == "" {
		t.Fatal("the command is not in its leaf")
	}
	_ = cmd.Wait()
	cancel()
	for range 50 {
		if left, _ := filepath.Glob(filepath.Join(s.cg.sb.Path(), "*", "pids.max")); len(left) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the leaf outlived its command")
}

// Without a delegated cgroup, a memory bound set in a file refuses the tier
// rather than run unbounded; the defaults alone do not.
func TestProcessMemoryBoundNeedsACgroup(t *testing.T) {
	ws := workspace(t)
	p := DefaultPolicy(ws)
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skip(why)
	}
	if s.cgroups().sb != nil {
		_ = s.Close()
		t.Skip("this host delegates a cgroup")
	}
	p.MemoryRequired = true
	if ok, why := NewProcess(p).Available(); ok || !strings.Contains(why, "max_memory_mb") {
		t.Fatalf("available %v: %s", ok, why)
	}
}
