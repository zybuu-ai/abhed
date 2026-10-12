//go:build unix

package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// failingList is an account store whose List fails while broken is set.
type failingList struct {
	*auth.MemoryUserStore
	broken bool
}

func (f *failingList) List(ctx context.Context) ([]*auth.User, error) {
	if f.broken {
		return nil, errors.New("store down")
	}
	return f.MemoryUserStore.List(ctx)
}

// A local account removed by another process, or removed and made again,
// has its background work here stopped by the sweep; a store that cannot be
// read stops nothing, and another account's work is left alone.
func TestRemovedAccountsWorkIsStopped(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		ctx := context.Background()
		users := &failingList{MemoryUserStore: auth.NewMemoryUserStore()}
		local := auth.NewLocalAuth(users, time.Hour, false)
		for _, n := range []string{"bob", "carol"} {
			if err := local.CreateUser(ctx, auth.User{Username: n}, "correct-horse-1"); err != nil {
				t.Fatal(err)
			}
		}
		cfg := config.Default()
		cfg.Auth.Mode = "local"
		cfg.Subagents.Wake = "auto"
		cfg.Permissions.Allow = []string{"bash(sleep *)"}
		st := agent.NewMemStore()
		s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: &shellAdapter{calls: map[string]int{}},
			Registry: tools.NewRegistry(tools.Read{}, tools.Bash{}), Store: st,
			Auth: &auth.Middleware{Providers: []auth.Provider{local}}})
		b := &bgServer{t: t, s: s, h: s.Handler(), store: st}
		start := func(owner string) string {
			id, err := s.StartSession(ctx, StartSpec{Prompt: "sh:" + owner, Tenant: "default", User: owner})
			if err != nil {
				t.Fatal(err)
			}
			waitUntil(t, owner+"'s shell", func() bool { return b.state(id) == "background" && b.live(id).Loop.Background.Live() == 1 })
			return id
		}
		bob, carol := start(auth.LocalOwner("bob")), start(auth.LocalOwner("carol"))
		t.Cleanup(func() { b.live(carol).Loop.Background.CancelAll(agent.TermSessionClosed) })

		if n := s.stopRemovedOwners(ctx); n != 0 {
			t.Fatalf("stopped %d with every owner present", n)
		}
		time.Sleep(5 * time.Millisecond)
		if _, err := auth.RemoveUser(ctx, users, "bob"); err != nil {
			t.Fatal(err)
		}
		if recreate {
			if err := auth.NewLocalAuth(users, time.Hour, false).CreateUser(ctx, auth.User{Username: "bob"}, "another-horse-2"); err != nil {
				t.Fatal(err)
			}
		}
		users.broken = true
		if n := s.stopRemovedOwners(ctx); n != 0 {
			t.Fatalf("recreate=%v: stopped %d while the store could not be read", recreate, n)
		}
		users.broken = false
		s.stopRemovedOwners(ctx)
		waitUntil(t, "bob's shell ended", func() bool { return countType(b.events(bob), agent.EvShellEnded) == 1 })
		// A removed owner's idle check may get there first, as owner_inactive;
		// one made again passes that check, so only the sweep stops it.
		e := payloadsOf(b.events(bob), agent.EvShellEnded)[0]
		if e["reason"] != string(agent.TermOwnerRevoked) && (recreate || e["reason"] != string(agent.TermOwnerInactive)) {
			t.Fatalf("recreate=%v: bob's shell ended %v", recreate, e)
		}
		if got := b.live(carol).Loop.Background.Live(); got != 1 {
			t.Fatalf("carol's shell stopped too: %d live", got)
		}
	}
}

// Taking administrator rights away stops the work the person started with them,
// and the audit counts what was stopped.
func TestDemotionStopsTheAdministratorsWork(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var audited map[string]any
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	for _, u := range []auth.User{
		{Username: "alice", Groups: []string{DefaultAdminGroup}},
		{Username: "bob", Groups: []string{DefaultAdminGroup}},
	} {
		if err := local.CreateUser(ctx, u, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Permissions.Allow = []string{"bash(sleep *)"}
	st := agent.NewMemStore()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: &shellAdapter{calls: map[string]int{}},
		Registry: tools.NewRegistry(tools.Read{}, tools.Bash{}), Store: st,
		AdminAudit: func(_ context.Context, action, _ string, detail map[string]any) {
			if action == "user.admin_revoked" {
				mu.Lock()
				audited = detail
				mu.Unlock()
			}
		},
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}})
	b := &bgServer{t: t, s: s, h: s.Handler(), store: st}
	id, err := s.StartSession(ctx, StartSpec{Prompt: "sh:bob", Tenant: "default", User: auth.LocalOwner("bob")})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "bob's shell", func() bool { return b.state(id) == "background" && b.live(id).Loop.Background.Live() == 1 })
	g := &adminRig{h: b.h, local: local}
	if rec := g.setAdmin(g.signIn(t, "alice"), "bob", false); rec.Code != 204 {
		t.Fatalf("demote bob = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "bob's shell ended", func() bool { return countType(b.events(id), agent.EvShellEnded) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if n, _ := audited["work_stopped"].(int); n != 1 {
		t.Fatalf("audit detail = %v; want work_stopped 1", audited)
	}
}

// One server runs one removed-owner sweep: a second returns at once instead
// of registering another listener that stops the same work again.
func TestOneRemovedOwnerSweepPerServer(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: agent.NewMemStore(),
		Auth: &auth.Middleware{Providers: []auth.Provider{local}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.sweepRemovedOwners(ctx, time.Hour)
	waitUntil(t, "the first sweep", s.ownerSweep.Load)
	done := make(chan struct{})
	go func() { s.sweepRemovedOwners(ctx, time.Hour); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second sweep ran alongside the first")
	}
}

// An account removed by another process, or removed and made again, is
// noticed first by a request carrying the old cookie: releaseStale lets the
// session go, and the sweep it pokes still stops the work that session runs.
func TestRemovedAccountsWorkStopsOnTheOldCookie(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		users := auth.NewMemoryUserStore()
		local := auth.NewLocalAuth(users, time.Hour, false)
		for _, n := range []string{"bob", "carol"} {
			if err := local.CreateUser(ctx, auth.User{Username: n}, "correct-horse-1"); err != nil {
				t.Fatal(err)
			}
		}
		cfg := config.Default()
		cfg.Auth.Mode = "local"
		cfg.Subagents.Wake = "auto"
		cfg.Permissions.Allow = []string{"bash(sleep *)"}
		st := agent.NewMemStore()
		s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: &shellAdapter{calls: map[string]int{}},
			Registry: tools.NewRegistry(tools.Read{}, tools.Bash{}), Store: st,
			Auth: &auth.Middleware{Providers: []auth.Provider{local},
				PublicPaths: append(PublicPaths(), local.PublicPaths()...)}})
		b := &bgServer{t: t, s: s, h: s.Handler(), store: st}
		g := &adminRig{h: b.h, local: local}
		cookie := g.signIn(t, "bob")
		start := func(owner string) string {
			id, err := s.StartSession(ctx, StartSpec{Prompt: "sh:" + owner, Tenant: "default", User: owner})
			if err != nil {
				t.Fatal(err)
			}
			waitUntil(t, owner+"'s shell", func() bool { return b.state(id) == "background" && b.live(id).Loop.Background.Live() == 1 })
			return id
		}
		bob, carol := start(auth.LocalOwner("bob")), start(auth.LocalOwner("carol"))
		// Only the poke can run it within the test.
		go s.sweepRemovedOwners(ctx, time.Hour)
		waitUntil(t, "the sweep", s.ownerSweep.Load)

		time.Sleep(5 * time.Millisecond)
		// Another process: this server's LocalAuth is not told.
		if _, err := auth.RemoveUser(ctx, users, "bob"); err != nil {
			t.Fatal(err)
		}
		if recreate {
			if err := auth.NewLocalAuth(users, time.Hour, false).CreateUser(ctx, auth.User{Username: "bob"}, "another-horse-2"); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest("GET", "/v1/sessions", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		b.h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("recreate=%v: old cookie = %d", recreate, rec.Code)
		}
		waitUntil(t, "bob's shell ended", func() bool { return countType(b.events(bob), agent.EvShellEnded) == 1 })
		e := payloadsOf(b.events(bob), agent.EvShellEnded)[0]
		if e["reason"] != string(agent.TermOwnerRevoked) {
			t.Fatalf("recreate=%v: bob's shell ended %v", recreate, e)
		}
		if got := b.live(carol).Loop.Background.Live(); got != 1 {
			t.Fatalf("carol's shell stopped too: %d live", got)
		}
		b.live(carol).Loop.Background.CancelAll(agent.TermSessionClosed)
		cancel()
	}
}

// An account removed and made again keeps the work it began since: the sweep
// stops only the earlier account's session, released or not, and stops
// nothing more on later sweeps.
func TestRemovedOwnerSweepSparesTheNewAccount(t *testing.T) {
	for _, released := range []bool{false, true} {
		ctx := context.Background()
		users := auth.NewMemoryUserStore()
		local := auth.NewLocalAuth(users, time.Hour, false)
		if err := local.CreateUser(ctx, auth.User{Username: "bob"}, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.Auth.Mode = "local"
		cfg.Permissions.Allow = []string{"bash(sleep *)"}
		st := agent.NewMemStore()
		s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: &shellAdapter{calls: map[string]int{}},
			Registry: tools.NewRegistry(tools.Read{}, tools.Bash{}), Store: st,
			Auth: &auth.Middleware{Providers: []auth.Provider{local}}})
		b := &bgServer{t: t, s: s, h: s.Handler(), store: st}
		start := func() string {
			id, err := s.StartSession(ctx, StartSpec{Prompt: "sh:bob", Tenant: "default", User: auth.LocalOwner("bob")})
			if err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "bob's shell", func() bool { return b.state(id) == "background" && b.live(id).Loop.Background.Live() == 1 })
			return id
		}
		old := start()
		time.Sleep(5 * time.Millisecond)
		if _, err := auth.RemoveUser(ctx, users, "bob"); err != nil {
			t.Fatal(err)
		}
		if err := auth.NewLocalAuth(users, time.Hour, false).CreateUser(ctx, auth.User{Username: "bob"}, "another-horse-2"); err != nil {
			t.Fatal(err)
		}
		if released {
			s.releaseStale(local, "bob")
		}
		time.Sleep(5 * time.Millisecond)
		fresh := start()
		t.Cleanup(func() { b.live(fresh).Loop.Background.CancelAll(agent.TermSessionClosed) })

		if n := s.stopRemovedOwners(ctx); n != 1 {
			t.Fatalf("released=%v: first sweep stopped %d; want 1", released, n)
		}
		waitUntil(t, "the old shell ended", func() bool { return countType(b.events(old), agent.EvShellEnded) == 1 })
		if n := s.stopRemovedOwners(ctx); n != 0 {
			t.Fatalf("released=%v: second sweep stopped %d", released, n)
		}
		if got := b.live(fresh).Loop.Background.Live(); got != 1 {
			t.Fatalf("released=%v: the new account's shell stopped: %d live, events %v",
				released, got, payloadsOf(b.events(fresh), agent.EvShellEnded))
		}
	}
}
