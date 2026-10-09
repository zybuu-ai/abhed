package profiles

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// tokenServer accepts one access token at a time and rotates it on refresh.
type tokenServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	current  string
	refresh  string
	refreshs int
	serial   int
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	ts := &tokenServer{current: "at-0", refresh: "rt-0"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.mu.Lock()
		defer ts.mu.Unlock()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != ts.refresh {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		ts.refreshs++
		ts.serial++
		ts.current = "at-" + string(rune('0'+ts.serial))
		ts.refresh = "rt-" + string(rune('0'+ts.serial))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": ts.current, "token_type": "Bearer",
			"expires_in": 3600, "refresh_token": ts.refresh})
	})
	mux.HandleFunc("GET /v1/cli/whoami", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ok := r.Header.Get("Authorization") == "Bearer "+ts.current
		ts.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"subject": "ada"})
	})
	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) profile(token string, issued, expires time.Time) *Profile {
	return &Profile{URL: ts.srv.URL, Token: token, RefreshToken: "rt-0", TokenEndpoint: ts.srv.URL + "/token",
		IssuedAt: issued, ExpiresAt: expires}
}

func TestNearExpiry(t *testing.T) {
	now := time.Now()
	p := &Profile{Token: "x", IssuedAt: now.Add(-9 * time.Hour), ExpiresAt: now.Add(time.Hour + time.Minute)}
	if p.NearExpiry(now) {
		t.Error("more than a tenth left counts as near")
	}
	p.ExpiresAt = now.Add(59 * time.Minute)
	if !p.NearExpiry(now) {
		t.Error("less than a tenth left is not near")
	}
	if (&Profile{Token: "x"}).NearExpiry(now) {
		t.Error("a token with no expiry is near")
	}
	if !(&Profile{Token: "x", ExpiresAt: now.Add(-time.Second)}).NearExpiry(now) {
		t.Error("an expired token with no issue time is not near")
	}
}

// Near expiry, the token is refreshed before the request and the new tokens
// are saved; well within its life, nothing is refreshed.
func TestDoRefreshesNearExpiry(t *testing.T) {
	ts := newTokenServer(t)
	now := time.Now()
	saves := 0
	save := func(*Profile) error { saves++; return nil }

	fresh := ts.profile("at-0", now, now.Add(time.Hour))
	resp, err := fresh.Do(context.Background(), http.DefaultClient, func() (*http.Request, error) {
		return fresh.NewRequest(http.MethodGet, "/v1/cli/whoami", nil)
	}, save)
	if err != nil || resp.StatusCode != 200 || ts.refreshs != 0 || saves != 0 {
		t.Fatalf("a fresh token: %v %v refreshes %d saves %d", resp, err, ts.refreshs, saves)
	}
	_ = resp.Body.Close()

	old := ts.profile("at-0", now.Add(-10*time.Hour), now.Add(30*time.Minute))
	resp, err = old.Do(context.Background(), http.DefaultClient, func() (*http.Request, error) {
		return old.NewRequest(http.MethodGet, "/v1/cli/whoami", nil)
	}, save)
	if err != nil || resp.StatusCode != 200 || ts.refreshs != 1 || saves != 1 || old.Token != "at-1" || old.RefreshToken != "rt-1" {
		t.Fatalf("near expiry: %v %v refreshes %d saves %d token %s", resp, err, ts.refreshs, saves, old.Token)
	}
	_ = resp.Body.Close()
}

// A 401 refreshes once and sends again; a second 401 is the answer.
func TestDoRefreshesOnceOn401(t *testing.T) {
	ts := newTokenServer(t)
	now := time.Now()
	p := ts.profile("revoked-elsewhere", now, now.Add(time.Hour))
	me, err := Whoami(context.Background(), http.DefaultClient, p)
	if err != nil || me["subject"] != "ada" || ts.refreshs != 1 || p.Token != "at-1" {
		t.Fatalf("after a 401: %v %v refreshes %d", me, err, ts.refreshs)
	}
	// A refresh token the server no longer takes: one try, then the 401 stands.
	p.Token, p.RefreshToken = "stale", "rt-gone"
	if _, err := Whoami(context.Background(), http.DefaultClient, p); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("with a dead refresh token: %v", err)
	}
	if ts.refreshs != 1 {
		t.Errorf("refreshed %d times", ts.refreshs)
	}
	// Without a refresh token nothing is tried.
	q := &Profile{URL: ts.srv.URL, Token: "stale"}
	if _, err := Whoami(context.Background(), http.DefaultClient, q); err == nil {
		t.Fatal("a stale token without a refresh token was accepted")
	}
}

func TestApplyRecordsIssueTime(t *testing.T) {
	now := time.Now()
	p := &Profile{}
	p.Apply(&Token{AccessToken: "a", ExpiresIn: 100}, now)
	if !p.IssuedAt.Equal(now) || !p.ExpiresAt.Equal(now.Add(100*time.Second)) {
		t.Fatalf("%v %v", p.IssuedAt, p.ExpiresAt)
	}
}
