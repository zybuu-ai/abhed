package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ownerRig is a local-accounts server where bob has no email and carol has
// carol@example.test, with two unused invites.
type ownerRig struct {
	*gateRig
	s *Server
}

func newOwnerRig(t *testing.T) *ownerRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	for _, u := range []auth.User{{Username: "bob"}, {Username: "carol", Email: "carol@example.test"}} {
		if err := local.CreateUser(context.Background(), u, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	inv := &oneUseInvites{codes: map[string]string{"code-1": "", "code-2": ""}}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Invites: inv,
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}})
	return &ownerRig{gateRig: &gateRig{h: s.Handler(), local: local, invites: inv}, s: s}
}

// legacy gives an existing account an email the checks now refuse, as an
// account made before them may hold.
func (g *ownerRig) legacy(t *testing.T, username, email string) {
	t.Helper()
	if err := g.local.CreateUser(context.Background(), auth.User{Username: username}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	u, err := g.local.Store.Get(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	u.Email = email
	if err := g.local.Store.Put(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func (g *ownerRig) open(t *testing.T, c *http.Cookie) string {
	t.Helper()
	rec := g.do(c, "POST", "/v1/sessions", `{"workbench":true}`)
	var created createResponse
	if json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.SessionID == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return created.SessionID
}

func (g *ownerRig) listed(t *testing.T, c *http.Cookie, id string) bool {
	t.Helper()
	rec := g.do(c, "GET", "/v1/sessions", "")
	var list []sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

// An invite registration may not take another account's username or email
// (in any case) as its email, and a refusal leaves the invite unspent.
func TestSignupRefusesAnotherAccountsNameOrEmail(t *testing.T) {
	g := newOwnerRig(t)
	for _, email := range []string{"bob", "BOB", "carol@example.test", "Carol@Example.TEST", " carol@example.test ",
		"Bob <x@example.test>"} {
		body := `{"username":"mallet","password":"long-enough-pw","invite":"code-1","email":` + jsonString(email) + `}`
		if rec := g.do(nil, "POST", "/v1/signup", body); rec.Code != http.StatusBadRequest {
			t.Errorf("signup with email %q = %d %s, want 400", email, rec.Code, rec.Body)
		}
	}
	if rec := g.do(nil, "POST", "/v1/signup",
		`{"username":"mallet","password":"long-enough-pw","invite":"code-1","email":"mallet@example.test"}`); rec.Code != http.StatusOK {
		t.Fatalf("the refused attempts spent the invite: %d %s", rec.Code, rec.Body)
	}
}

// An email never owns a session: an account holding another's username or
// email as its email sees none of that person's sessions.
func TestAnEmailNeverOwnsSessions(t *testing.T) {
	g := newOwnerRig(t)
	g.legacy(t, "mallet", "bob")
	g.legacy(t, "eve", "carol@example.test")
	bob, carol := g.signIn(t, "bob"), g.signIn(t, "carol")
	mallet, eve := g.signIn(t, "mallet"), g.signIn(t, "eve")
	bobs, carols := g.open(t, bob), g.open(t, carol)

	for _, tc := range []struct {
		who    string
		c      *http.Cookie
		victim string
	}{{"mallet", mallet, bobs}, {"eve", eve, carols}} {
		if g.listed(t, tc.c, tc.victim) {
			t.Errorf("%s lists %s", tc.who, tc.victim)
		}
		for _, path := range []string{"/replay", "/state", "/hawkeye"} {
			if rec := g.do(tc.c, "GET", "/v1/sessions/"+tc.victim+path, ""); rec.Code != http.StatusNotFound {
				t.Errorf("%s GET %s = %d, want 404", tc.who, path, rec.Code)
			}
		}
		if rec := g.do(tc.c, "POST", "/v1/sessions/"+tc.victim+"/interrupt", ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s interrupt = %d, want 404", tc.who, rec.Code)
		}
	}
	if !g.listed(t, bob, bobs) || !g.listed(t, carol, carols) {
		t.Fatal("an owner no longer lists their own session")
	}
}

// An account holding carol's email cannot answer carol's pending ask; carol
// can, and the record names her account.
func TestAnotherAccountCannotAnswerAPendingAsk(t *testing.T) {
	g := newOwnerRig(t)
	g.legacy(t, "frank", "carol@example.test")
	carol, frank := g.signIn(t, "carol"), g.signIn(t, "frank")
	id := g.open(t, carol)
	g.s.mu.RLock()
	live := g.s.running[id]
	g.s.mu.RUnlock()

	ctx, answer := agent.ExpectAnswer(agent.WithRequestID(context.Background(), "ev-c"))
	c := make(chan turnResult, 1)
	go func() {
		ok, err := live.Approve(ctx, "bash", nil, policy.Result{Decision: policy.Ask, Step: "default"})
		c <- turnResult{ok, err}
	}()
	<-waitAsked(live, "ev-c").ready

	if rec := g.do(frank, "POST", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"ev-c"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("frank's answer = %d %s, want 404", rec.Code, rec.Body)
	}
	silent(t, c, "an answer from another account")
	if rec := g.do(carol, "POST", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"ev-c"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("carol's answer = %d %s", rec.Code, rec.Body)
	}
	if res := <-c; !res.ok {
		t.Fatal("carol's approval was not taken")
	}
	if answer.Approver != "local:carol" {
		t.Fatalf("approver %q, want local:carol", answer.Approver)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Nothing else in the server decides who owns a session.
func TestOwnerComesFromOneFunction(t *testing.T) {
	src := readSource(t, "server.go")
	if strings.Contains(src, "user = id.Email") {
		t.Fatal("server.go derives an owner from an email again; use auth.Identity.Owner")
	}
	if !strings.Contains(src, "return id.Owner(), s.tenantFor(ctx, id)") {
		t.Fatal("callerOf no longer uses auth.Identity.Owner")
	}
}

// An owner that names nobody matches no session, not even an anonymous one.
func TestNobodyOwnsNoSession(t *testing.T) {
	for _, rec := range []string{"anonymous", "nobody:oidc", "local:bob"} {
		if ownsSession("default", rec, "default", "nobody:oidc") {
			t.Errorf("nobody:oidc owns a session of %q", rec)
		}
	}
}
