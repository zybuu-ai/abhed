//go:build unix

package server

import (
	"context"
	"errors"
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
		if n := s.stopRemovedOwners(ctx); n != 1 {
			t.Fatalf("recreate=%v: stopped %d, want bob's one shell", recreate, n)
		}
		waitUntil(t, "bob's shell ended", func() bool { return countType(b.events(bob), agent.EvShellEnded) == 1 })
		if e := payloadsOf(b.events(bob), agent.EvShellEnded)[0]; e["reason"] != string(agent.TermOwnerRevoked) {
			t.Fatalf("bob's shell ended %v", e)
		}
		if got := b.live(carol).Loop.Background.Live(); got != 1 {
			t.Fatalf("carol's shell stopped too: %d live", got)
		}
	}
}

// Taking administrator rights away stops the work the person started with them.
func TestDemotionStopsTheAdministratorsWork(t *testing.T) {
	ctx := context.Background()
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
}
