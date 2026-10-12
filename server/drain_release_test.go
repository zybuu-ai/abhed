package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// heldRig opens n workbench sessions on one server, drains it, and holds
// them all for workbench work on a second server over the same store.
func heldRig(t *testing.T, n int) (*durableMem, *Server, []string) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	dir := t.TempDir()
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	opts := Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{}, Store: st,
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{})}
	first := New(opts)
	wb := &workbench{t: t, h: first.Handler(), workspace: dir}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = wb.openIdle("acme")
	}
	first.drain()
	second := New(opts)
	wb.h = second.Handler()
	for _, id := range ids {
		wb.session = id
		if rec := wb.send("acme", "POST", "exec", execRequest{Command: "echo held"}); rec.Code != http.StatusOK {
			t.Fatalf("exec: %d %s", rec.Code, rec.Body)
		}
	}
	return st, second, ids
}

// A shutdown gives back its workbench holds together, not one after another,
// so a slow store costs the shutdown one wait rather than one per session.
func TestDrainReleasesHeldClaimsTogether(t *testing.T) {
	st, s, ids := heldRig(t, 4)
	st.releaseDelay = 700 * time.Millisecond
	start := time.Now()
	s.drain()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("drain took %v for four slow releases", took)
	}
	for _, id := range ids {
		if got := st.holderOf(id); got != "" {
			t.Fatalf("%s still held by %q after the drain", id, got)
		}
	}
}

// A release the store fails at first is tried again, rather than leaving the
// session held until the claim goes stale.
func TestReleaseIsRetried(t *testing.T) {
	st, s, ids := heldRig(t, 1)
	st.releaseFails.Store(2)
	s.drain()
	if got := st.holderOf(ids[0]); got != "" {
		t.Fatalf("still held by %q after two failed releases", got)
	}
}

// A workbench write's claim has a deadline, and one still claiming as the
// drain begins has its hold released by that drain.
func TestDrainReleasesAHoldClaimedAsItBegan(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	dir := t.TempDir()
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	opts := Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{}, Store: st,
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{})}
	first := New(opts)
	wb := &workbench{t: t, h: first.Handler(), workspace: dir}
	wb.session = wb.openIdle("acme")
	first.drain()
	second := New(opts)
	wb.h = second.Handler()
	st.claimReached, st.claimGate = make(chan struct{}), make(chan struct{})
	wrote := make(chan int, 1)
	go func() { wrote <- wb.send("acme", "POST", "exec", execRequest{Command: "echo late"}).Code }()
	<-st.claimReached
	if !st.claimDeadline.Load() {
		t.Error("the write's claim had no deadline")
	}
	drained := make(chan struct{})
	go func() { second.drain(); close(drained) }()
	time.Sleep(100 * time.Millisecond)
	close(st.claimGate)
	<-wrote
	<-drained
	if got := st.holderOf(wb.session); got != "" {
		t.Fatalf("a hold claimed as the drain began is still held by %q", got)
	}
}
