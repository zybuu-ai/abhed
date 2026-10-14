package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ClientCertAuth signs a local account in from a verified TLS client
// certificate: the certificate's common name is the account's username.
//
// Only a chain the TLS layer verified against the configured client CA
// counts; an offered but unverified certificate never reaches here. The
// account is read again, briefly cached, so removing it ends the access.
// A certificate cannot be signed out: revoke it in server.tls.client_revoked_file,
// remove the account, or replace the client CA.
type ClientCertAuth struct {
	// Local holds the accounts, and its Admit hook is asked as at a
	// password sign-in.
	Local *LocalAuth

	mu    sync.Mutex
	cache map[string]certEntry
}

type certEntry struct {
	id *Identity
	at time.Time
}

// certRecheck bounds how long an account read is trusted for a certificate.
const certRecheck = 2 * time.Second

// NewClientCertAuth maps client certificates to local's accounts.
func NewClientCertAuth(local *LocalAuth) *ClientCertAuth {
	return &ClientCertAuth{Local: local, cache: map[string]certEntry{}}
}

// ErrCertNoAccount is why a verified certificate signed no one in.
var ErrCertNoAccount = errors.New("the client certificate names no account here")

// CertSubject is the account a request's verified client certificate names,
// or "" when it carries none.
func CertSubject(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return ""
	}
	return strings.TrimSpace(r.TLS.VerifiedChains[0][0].Subject.CommonName)
}

func (c *ClientCertAuth) Name() string { return "certificate" }

func (c *ClientCertAuth) Identify(r *http.Request) (*Identity, bool) {
	sub := CertSubject(r)
	if sub == "" || c.Local == nil || c.Local.Store == nil || !validUsername.MatchString(sub) {
		return nil, false
	}
	id, err := c.lookup(r.Context(), sub)
	if err != nil {
		return nil, false
	}
	return id, true
}

func (c *ClientCertAuth) lookup(ctx context.Context, sub string) (*Identity, error) {
	key := strings.ToLower(sub)
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Sub(e.at) < certRecheck {
		c.mu.Unlock()
		cp := *e.id
		cp.Groups = slices.Clone(e.id.Groups)
		return &cp, nil
	}
	c.mu.Unlock()
	u, err := c.Local.Store.Get(ctx, sub)
	if err == nil && u == nil {
		err = ErrCertNoAccount
	}
	if err == nil && c.Local.Admit != nil {
		err = c.Local.Admit(ctx, u)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.cache, key)
		c.mu.Unlock()
		return nil, err
	}
	// Same provider as a password sign-in, so the person owns the same sessions.
	id := &Identity{Subject: u.Username, Email: u.Email, Name: u.Name, Tenant: u.Tenant,
		Groups: slices.Clone(u.Groups), Provider: ProviderLocal, IssuedAt: now.Unix()}
	c.mu.Lock()
	if len(c.cache) > 4096 {
		clear(c.cache)
	}
	c.cache[key] = certEntry{id: id, at: now}
	c.mu.Unlock()
	cp := *id
	cp.Groups = slices.Clone(id.Groups)
	return &cp, nil
}

// Routes registers nothing: the TLS handshake is the whole sign-in.
func (c *ClientCertAuth) Routes(*http.ServeMux) {}

func (c *ClientCertAuth) PublicPaths() []string { return nil }

func (c *ClientCertAuth) SignIn() (url, label string) { return "", "" }

// SignOut cannot end a certificate; the browser presents it again.
func (c *ClientCertAuth) SignOut(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

// CookieSecure: a certificate only arrives over TLS.
func (c *ClientCertAuth) CookieSecure() bool { return true }
