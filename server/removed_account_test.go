package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// bob is removed and made again by another process sharing the account
// store, while this server holds the old bob's sessions, one of them asking.
// The new bob reaches none of them, with session rows (as on Postgres, where
// the removal unclaims them) or without (the memory store).
func TestARecreatedAccountDoesNotInheritHeldSessions(t *testing.T) {
	for _, durable := range []bool{true, false} {
		name := "memory store"
		if durable {
			name = "session rows"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			users := auth.NewMemoryUserStore()
			local := auth.NewLocalAuth(users, time.Hour, false)
			if err := local.CreateUser(ctx, auth.User{Username: "bob"}, "correct-horse-1"); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Auth.Mode = "local"
			opts := Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
				Registry: tools.NewRegistry(tools.Read{}),
				Auth: &auth.Middleware{Providers: []auth.Provider{local},
					PublicPaths: append(PublicPaths(), local.PublicPaths()...)}}
			var rows *durableMem
			if durable {
				rows = &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
				opts.Store = rows
			}
			s := New(opts)
			g := &ownerRig{gateRig: &gateRig{h: s.Handler(), local: local}, s: s}
			old := g.signIn(t, "bob")
			id := g.open(t, old)
			s.mu.RLock()
			live := s.running[id]
			s.mu.RUnlock()
			actx, _ := agent.ExpectAnswer(agent.WithRequestID(ctx, "ev-b"))
			c := make(chan turnResult, 1)
			go func() {
				ok, err := live.Approve(actx, "bash", nil, policy.Result{Decision: policy.Ask, Step: "default"})
				c <- turnResult{ok, err}
			}()
			<-waitAsked(live, "ev-b").ready

			// Another process: remove bob, as Postgres does unclaim his rows,
			// then make a new bob.
			time.Sleep(2 * time.Millisecond)
			other := auth.NewLocalAuth(users, time.Hour, false)
			if _, err := auth.RemoveUser(ctx, users, "bob"); err != nil {
				t.Fatal(err)
			}
			if durable {
				rows.mu.Lock()
				r := rows.rows[id]
				r.User = auth.UnclaimedOwner(r.User)
				rows.rows[id] = r
				rows.mu.Unlock()
			}
			if err := other.CreateUser(ctx, auth.User{Username: "bob"}, "another-horse-2"); err != nil {
				t.Fatal(err)
			}

			nb := g.do(nil, "POST", "/v1/signin", `{"username":"bob","password":"another-horse-2"}`).Result().Cookies()
			if len(nb) == 0 {
				t.Fatal("the new bob could not sign in")
			}
			newBob := nb[0]
			for _, path := range []string{"/replay", "/state", "/hawkeye"} {
				if rec := g.do(newBob, "GET", "/v1/sessions/"+id+path, ""); rec.Code != http.StatusNotFound {
					t.Errorf("new bob GET %s = %d, want 404", path, rec.Code)
				}
			}
			if rec := g.do(newBob, "POST", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"ev-b"}`); rec.Code != http.StatusNotFound {
				t.Errorf("new bob approve = %d %s, want 404", rec.Code, rec.Body)
			}
			if rec := g.do(newBob, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"go on"}`); rec.Code != http.StatusNotFound {
				t.Errorf("new bob message = %d %s, want 404", rec.Code, rec.Body)
			}
			silent(t, c, "an answer from the new bob")
			answerWhenAsked(live, false, "")
			<-c
		})
	}
}

// A held session whose row now names another owner is not the holder's,
// whatever the account store says.
func TestHeldSessionFollowsItsStoredOwner(t *testing.T) {
	rows := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: rows})
	s.mu.Lock()
	s.running["s-held"] = &liveSession{ID: "s-held", Tenant: "default", User: "local:bob"}
	s.running["s-norow"] = &liveSession{ID: "s-norow", Tenant: "default", User: "local:bob"}
	s.mu.Unlock()
	rows.rows["s-held"] = store.SessionRecord{ID: "s-held", Tenant: "default", User: "local:bob"}
	if _, ok := s.session("s-held", "default", "local:bob"); !ok {
		t.Fatal("an owner agreeing with its row was refused")
	}
	rows.rows["s-held"] = store.SessionRecord{ID: "s-held", Tenant: "default", User: "unclaimed:local:bob"}
	if _, ok := s.session("s-held", "default", "local:bob"); ok {
		t.Fatal("a held session whose row was unclaimed is still bob's")
	}
	if got := s.running["s-held"].User; got != "unclaimed:local:bob" {
		t.Errorf("held owner %q, want the row's", got)
	}
	if _, ok := s.session("s-norow", "default", "local:bob"); !ok {
		t.Error("a session with no row yet was refused")
	}
	s.mu.Lock()
	delete(s.running, "s-held")
	delete(s.running, "s-norow")
	s.mu.Unlock()
}
