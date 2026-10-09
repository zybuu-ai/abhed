package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A session's owner changes while it runs (an account removed elsewhere);
// every reader goes through s.mu, so go test -race finds nothing here.
func TestOwnerChangesWhileReadStress(t *testing.T) {
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry:    tools.NewRegistry(tools.Read{}),
		OwnerActive: func(_ context.Context, _, owner string) bool { return owner == "local:bob" }})
	live := &liveSession{ID: "s-1", Tenant: "default", User: "local:bob", allowed: map[string]bool{}}
	s.mu.Lock()
	s.running[live.ID] = live
	s.mu.Unlock()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.ReleaseSessions("default", "local:bob")
			s.mu.Lock()
			live.User = "local:bob"
			s.mu.Unlock()
		}
		close(stop)
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.ownerActive(live)
				_ = s.PendingAsks(func(string, string) bool { return true })
			}
		}()
	}
	wg.Wait()
	if got := s.ownerOf(live); got != "local:bob" {
		t.Fatalf("owner %q", got)
	}
}

// An account removed while its session's terminals open: startShell takes
// the owner under s.mu, so go test -race finds nothing here either.
func TestOwnerChangesWhileTerminalsOpenStress(t *testing.T) {
	wb := layerBench(t, func() (*PolicyLayer, error) {
		return &PolicyLayer{Name: "workspace pay", Deny: []string{"bash(curl*)"}}, nil
	})
	wb.s.mu.RLock()
	live := wb.s.running[wb.session]
	wb.s.mu.RUnlock()
	owner := wb.s.ownerOf(live)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			wb.s.mu.Lock()
			live.User = auth.UnclaimedOwner(owner)
			wb.s.mu.Unlock()
			wb.s.mu.Lock()
			live.User = owner
			wb.s.mu.Unlock()
		}
	}()
	opened := 0
	for range 200 {
		if code, got := ptyStart(t, wb, ptyStartRequest{Interactive: true}); code == http.StatusOK {
			if got.Interactive {
				t.Fatalf("a screening layer opened an interactive shell: %+v", got)
			}
			opened++
		}
	}
	close(stop)
	wg.Wait()
	if opened == 0 {
		t.Fatal("no terminal opened while the owner changed")
	}
}
