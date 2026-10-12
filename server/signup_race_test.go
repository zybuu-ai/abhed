package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// releasingInvites gives a spent code back on Release.
type releasingInvites struct{ oneUseInvites }

func (r *releasingInvites) Release(_ context.Context, code, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes[code] = ""
	return nil
}

func (r *releasingInvites) spent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, u := range r.codes {
		if u != "" {
			n++
		}
	}
	return n
}

// takenFirst is an account store in which another process makes the name
// just before this one's create lands.
type takenFirst struct{ *auth.MemoryUserStore }

func (s takenFirst) Create(ctx context.Context, u *auth.User) error {
	_ = s.MemoryUserStore.Create(ctx, &auth.User{Username: u.Username, Tenant: "default"})
	return s.MemoryUserStore.Create(ctx, u)
}

func signupServer(t *testing.T, store auth.UserStore, inv InviteRedeemer) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(store, time.Hour, false)
	return New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Invites: inv,
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}}).Handler()
}

// Two sign-ups racing for one name spend one invite between them.
func TestRacingSignupsSpendOneInvite(t *testing.T) {
	for i := 0; i < 20; i++ {
		inv := &releasingInvites{oneUseInvites{codes: map[string]string{"code-1": "", "code-2": ""}}}
		h := signupServer(t, auth.NewMemoryUserStore(), &inv.oneUseInvites)
		var wg sync.WaitGroup
		for _, code := range []string{"code-1", "code-2"} {
			wg.Add(1)
			go func(code string) {
				defer wg.Done()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/signup",
					strings.NewReader(`{"username":"erin","password":"long-enough-pw","invite":"`+code+`"}`)))
			}(code)
		}
		wg.Wait()
		if n := inv.spent(); n != 1 {
			t.Fatalf("round %d: %d invites spent for one account", i, n)
		}
	}
}

// A code redeemed for an account another server made first goes back.
func TestSignupGivesTheInviteBackWhenTheNameWasTaken(t *testing.T) {
	inv := &releasingInvites{oneUseInvites{codes: map[string]string{"code-1": ""}}}
	h := signupServer(t, takenFirst{auth.NewMemoryUserStore()}, inv)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/signup",
		strings.NewReader(`{"username":"erin","password":"long-enough-pw","invite":"code-1"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("signup = %d %s, want 400 for a taken name", rec.Code, rec.Body)
	}
	if n := inv.spent(); n != 0 {
		t.Fatal("the invite stayed spent for an account that was never made")
	}
}
