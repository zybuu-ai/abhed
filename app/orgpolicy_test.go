package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/orgpolicy"
	"github.com/zybuu-ai/abhed/profiles"
)

// orgServer serves keys and a signed policy, as an org server does.
type orgServer struct {
	*httptest.Server
	mu      sync.Mutex
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	epoch   int64
	policy  string
	subject string
	down    bool
	tokens  []string
}

func newOrgServer(t *testing.T) *orgServer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	o := &orgServer{pub: pub, priv: priv, epoch: 1, policy: `{"limits":{"max_turns":9}}`, subject: "alice"}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.tokens = append(o.tokens, r.Header.Get("Authorization"))
		if o.down {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case orgPolicyKeysPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []string{orgpolicy.EncodeKey(o.pub)}})
		case orgPolicyPath:
			env, _ := orgpolicy.Sign(orgpolicy.Statement{V: 1, Tenant: "acme", Subject: o.subject, Name: "default",
				Version: o.epoch, Epoch: o.epoch, IssuedAt: time.Now().UTC(), MaxAgeSeconds: 3600,
				Keys: []string{orgpolicy.EncodeKey(o.pub)}, Policy: json.RawMessage(o.policy)}, o.priv)
			_ = json.NewEncoder(w).Encode(map[string]any{"envelope": env})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(o.Close)
	return o
}

// orgClient is a signed-in profile for srv in a throwaway home.
func orgClient(t *testing.T, srv *orgServer) (string, *profiles.Profile) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	pf := filepath.Join(home, ".abhed", "profiles.json")
	t.Setenv(profiles.EnvFile, pf)
	old := managed.OrgStateFile
	managed.OrgStateFile = filepath.Join(home, ".abhed", "org-policy.json")
	t.Cleanup(func() { managed.OrgStateFile = old })
	oldM := managed.ConfigFile
	managed.ConfigFile = filepath.Join(home, "no-managed", "config.json")
	t.Cleanup(func() { managed.ConfigFile = oldM })
	p := &profiles.Profile{URL: srv.URL, Token: "tok-1", Username: "alice", AuthMode: "local"}
	reg := &profiles.File{Profiles: map[string]*profiles.Profile{"org": p}, Current: "org"}
	if err := profiles.Save(pf, reg); err != nil {
		t.Fatal(err)
	}
	return pf, p
}

func loadOrg(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadWith(t.TempDir(), config.LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestLoginEnrolsRefreshesAndFailsClosed(t *testing.T) {
	srv := newOrgServer(t)
	pf, p := orgClient(t, srv)
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{accept: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Pinned") || !strings.Contains(out.String(), orgpolicy.KeyID(srv.pub)) {
		t.Errorf("the pinned key was not shown: %s", out.String())
	}
	cfg := loadOrg(t)
	if cfg.OrgPolicy.Status != orgpolicy.StatusApplied || cfg.Limits.MaxTurns != 9 {
		t.Fatalf("after login: %+v turns %d", cfg.OrgPolicy, cfg.Limits.MaxTurns)
	}
	for _, tok := range srv.tokens {
		if tok != "Bearer tok-1" {
			t.Errorf("fetched without the sign-in token: %q", tok)
		}
	}
	// A new version is fetched and reported changed.
	srv.mu.Lock()
	srv.epoch, srv.policy = 2, `{"limits":{"max_turns":4}}`
	srv.mu.Unlock()
	res, err := refreshOrgPolicy(context.Background(), true)
	if err != nil || !res.Changed || res.Stmt.Version != 2 {
		t.Fatalf("refresh: %v %+v", err, res)
	}
	if cfg := loadOrg(t); cfg.Limits.MaxTurns != 4 {
		t.Errorf("the new version did not apply: %d", cfg.Limits.MaxTurns)
	}
	// Unreachable: the cached policy stays in force.
	srv.mu.Lock()
	srv.down = true
	srv.mu.Unlock()
	if _, err := refreshOrgPolicy(context.Background(), true); err == nil {
		t.Fatal("a failed fetch was not reported")
	}
	if cfg := loadOrg(t); cfg.OrgPolicy.Status != orgpolicy.StatusApplied || cfg.Limits.MaxTurns != 4 {
		t.Errorf("an unreachable server dropped the cached policy: %+v", cfg.OrgPolicy)
	}
	// A rollback is refused, and the fallback applies until a good one comes.
	srv.mu.Lock()
	srv.down, srv.epoch = false, 1
	srv.mu.Unlock()
	if _, err := refreshOrgPolicy(context.Background(), true); err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("rollback: %v", err)
	}
	if cfg := loadOrg(t); cfg.OrgPolicy.Status != orgpolicy.StatusFallback || cfg.Permissions.Mode != "plan" {
		t.Errorf("after a refused policy: %+v mode %q", cfg.OrgPolicy, cfg.Permissions.Mode)
	}
	// Signed by a key the client never pinned: refused too.
	srv.mu.Lock()
	srv.epoch = 3
	_, srv.priv, _ = ed25519.GenerateKey(rand.Reader)
	srv.mu.Unlock()
	if _, err := refreshOrgPolicy(context.Background(), true); err == nil {
		t.Fatal("a policy signed by an unpinned key was accepted")
	}
	if cfg := loadOrg(t); cfg.OrgPolicy.Status != orgpolicy.StatusFallback {
		t.Errorf("status %+v", cfg.OrgPolicy)
	}
}

func TestLoginRefusesAPolicyKeyTheServerDoesNotOffer(t *testing.T) {
	srv := newOrgServer(t)
	pf, p := orgClient(t, srv)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	var out, errOut bytes.Buffer
	err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{given: []string{orgpolicy.EncodeKey(other)}, accept: true}, &out, &errOut)
	if err == nil {
		t.Fatal("a key the server does not offer was pinned")
	}
	if st, _ := orgpolicy.LoadState(managed.OrgStateFile); st != nil {
		t.Error("a state was written for a refused enrolment")
	}
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{given: []string{orgpolicy.EncodeKey(srv.pub)}, accept: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyForAnotherSubjectIsRefused(t *testing.T) {
	srv := newOrgServer(t)
	srv.subject = "bob"
	pf, p := orgClient(t, srv)
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{accept: true}, &out, &errOut); err == nil {
		t.Fatal("bob's policy was taken for alice")
	}
	if cfg := loadOrg(t); cfg.OrgPolicy.Status != orgpolicy.StatusFallback {
		t.Errorf("status %+v", cfg.OrgPolicy)
	}
}

func TestPolicyLeaveAndManagedLock(t *testing.T) {
	srv := newOrgServer(t)
	pf, p := orgClient(t, srv)
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{accept: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := policyCmd([]string{"status"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "applied") ||
		!strings.Contains(out.String(), "limits.max_turns") {
		t.Errorf("status %d: %s %s", code, out.String(), errOut.String())
	}
	// Under a managed file that names the org policy, leaving is refused.
	mdir := filepath.Dir(managed.ConfigFile)
	_ = os.MkdirAll(mdir, 0o755)
	if err := os.WriteFile(managed.ConfigFile, []byte(`{"org_policy":{"url":"`+srv.URL+`","required":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := policyCmd([]string{"leave"}, &out, &errOut); code == 0 {
		t.Fatal("leave was allowed under a managed org policy")
	}
	_ = os.Remove(managed.ConfigFile)
	if code := policyCmd([]string{"leave"}, &out, &errOut); code != 0 {
		t.Fatalf("leave: %s", errOut.String())
	}
	if cfg := loadOrg(t); cfg.OrgPolicy.Status != orgpolicy.StatusNone || cfg.Managed {
		t.Errorf("after leave: %+v", cfg.OrgPolicy)
	}
}

func TestManagedFileNamingAnotherServerIgnoresTheLogin(t *testing.T) {
	srv := newOrgServer(t)
	pf, p := orgClient(t, srv)
	_ = os.MkdirAll(filepath.Dir(managed.ConfigFile), 0o755)
	if err := os.WriteFile(managed.ConfigFile, []byte(`{"org_policy":{"url":"https://the-real-org.example"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{accept: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if st, _ := orgpolicy.LoadState(managed.OrgStateFile); st != nil {
		t.Error("enrolled with a server the managed file does not name")
	}
}

func TestChoosePins(t *testing.T) {
	a, _, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _ := ed25519.GenerateKey(rand.Reader)
	ka, kb := orgpolicy.EncodeKey(a), orgpolicy.EncodeKey(b)
	if got, _ := choosePins([]string{ka}, nil, []string{kb}, nil); got[0] != kb {
		t.Error("the machine's keys must win")
	}
	if got, _ := choosePins([]string{ka, kb}, nil, nil, &orgpolicy.State{Keys: []string{kb}}); len(got) != 1 || got[0] != kb {
		t.Error("a sign-in again must keep the pins, not take the server's again")
	}
	if got, _ := choosePins([]string{ka}, nil, nil, nil); got[0] != ka {
		t.Error("first sign-in pins what is offered")
	}
}

// A plain sign-in to a server that serves a policy does not enrol: it needs
// -org-policy or a yes, unless the machine's managed file names the server.
func TestEnrolmentIsExplicit(t *testing.T) {
	srv := newOrgServer(t)
	pf, p := orgClient(t, srv)
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if st, _ := orgpolicy.LoadState(managed.OrgStateFile); st != nil || !strings.Contains(out.String(), "not taken") {
		t.Fatalf("a plain login enrolled: %v %s", st, out.String())
	}
	asked := ""
	no := enrolOptions{confirm: func(p string) bool { asked = p; return false }}
	_ = enrollOrgPolicy(context.Background(), pf, "org", p, no, &out, &errOut)
	if st, _ := orgpolicy.LoadState(managed.OrgStateFile); st != nil || !strings.Contains(asked, srv.URL) ||
		!strings.Contains(asked, orgpolicy.KeyID(srv.pub)) || !strings.Contains(asked, "session records") {
		t.Fatalf("a no enrolled, or the question did not name the server, key and records: %q", asked)
	}
	yes := enrolOptions{confirm: func(string) bool { return true }}
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, yes, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if st, _ := orgpolicy.LoadState(managed.OrgStateFile); st == nil || st.Subject != "alice" || st.Tenant != "acme" {
		t.Fatalf("a yes did not enrol with the identity: %+v", st)
	}
}

func TestManagedServerNeedsNoConsent(t *testing.T) {
	srv := newOrgServer(t)
	pf, p := orgClient(t, srv)
	_ = os.MkdirAll(filepath.Dir(managed.ConfigFile), 0o755)
	if err := os.WriteFile(managed.ConfigFile, []byte(`{"org_policy":{"url":"`+srv.URL+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if st, _ := orgpolicy.LoadState(managed.OrgStateFile); st == nil {
		t.Fatal("the server the machine names was not taken")
	}
}

func TestPlainHTTPTrustOnFirstUseIsRefused(t *testing.T) {
	for u, want := range map[string]bool{"http://org.example": true, "http://127.0.0.1:8": false, "https://org.example": false, "http://localhost:9": false} {
		if plainRemote(u) != want {
			t.Errorf("plainRemote(%s) = %v", u, !want)
		}
	}
}

// record.ship from the machine's own file ships nowhere without an enrolment.
func TestRecordShipNeedsAnEnrolment(t *testing.T) {
	srv := newOrgServer(t)
	orgClient(t, srv)
	_ = os.MkdirAll(filepath.Dir(managed.ConfigFile), 0o755)
	_ = os.WriteFile(managed.ConfigFile, []byte(`{"record":{"ship":{"enabled":true}}}`), 0o644)
	cfg := loadOrg(t)
	if _, err := shipTargetFor(cfg); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("record.ship found a target without an enrolment: %v", err)
	}
}
