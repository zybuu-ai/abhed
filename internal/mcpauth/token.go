package mcpauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// Record is one server's login as the store keeps it.
type Record struct {
	V int `json:"v"`
	// Server and URL bind the login: it is used only for a server of this
	// name configured at this canonical URL.
	Server             string `json:"server"`
	URL                string `json:"url"`
	Issuer             string `json:"issuer"`
	TokenEndpoint      string `json:"token_endpoint"`
	RevocationEndpoint string `json:"revocation_endpoint,omitempty"`
	ClientID           string `json:"client_id"`
	ClientSecret       string `json:"client_secret,omitempty"`
	// AuthMethod is how the client authenticates at the token endpoint:
	// none, client_secret_post or client_secret_basic.
	AuthMethod   string `json:"auth_method,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// Expiry is when the access token ends, in Unix seconds; 0 when unsaid.
	Expiry int64  `json:"expires_at,omitempty"`
	Scope  string `json:"scope,omitempty"`
}

// ErrNoLogin is a server no one has signed in to.
var ErrNoLogin = errors.New("not signed in")

// names are the store names of a server's login: the record, and the tokens
// and client secret on their own, so the record's redaction covers each.
func names(server string) (record, access, refresh, secret string) {
	sum := sha256.Sum256([]byte(server))
	base := secrets.HeldPrefix + strings.ToUpper(hex.EncodeToString(sum[:12]))
	return base, base + "_AT", base + "_RT", base + "_CS"
}

// save writes rec in one step, under the login's lock.
func save(ctx context.Context, store *secrets.Store, rec Record) error {
	r, _, _, _ := names(rec.Server)
	unlock, err := store.HeldLock(ctx, r)
	if err != nil {
		return err
	}
	defer unlock()
	return saveLocked(store, rec)
}

// saveLocked is save for a caller holding the login's lock.
func saveLocked(store *secrets.Store, rec Record) error {
	data, err := json.Marshal(rec) // #nosec G117 -- the login is written to the secret store, which is where it belongs
	if err != nil {
		return err
	}
	r, a, rt, cs := names(rec.Server)
	return store.UpdateHeld(func(func(string) (string, bool)) (map[string]string, []string, error) {
		set := map[string]string{r: string(data), a: rec.AccessToken}
		var remove []string
		if rec.RefreshToken != "" {
			set[rt] = rec.RefreshToken
		} else {
			remove = append(remove, rt)
		}
		if rec.ClientSecret != "" {
			set[cs] = rec.ClientSecret
		} else {
			remove = append(remove, cs)
		}
		return set, remove, nil
	})
}

// load reads a server's login; ErrNoLogin when there is none.
func load(store *secrets.Store, server string) (Record, error) {
	r, _, _, _ := names(server)
	v, ok, err := store.HeldValue(r)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, ErrNoLogin
	}
	var rec Record
	if err := json.Unmarshal([]byte(v), &rec); err != nil || rec.Server != server || rec.AccessToken == "" {
		return Record{}, fmt.Errorf("the stored login for MCP server %s does not read; run abhed mcp login %s again", server, server)
	}
	return rec, nil
}

// Remove forgets a server's login; false when there was none.
func Remove(ctx context.Context, store *secrets.Store, server string) (bool, error) {
	r, a, rt, cs := names(server)
	unlock, err := store.HeldLock(ctx, r)
	if err != nil {
		return false, err
	}
	defer unlock()
	found := false
	err = store.UpdateHeld(func(get func(string) (string, bool)) (map[string]string, []string, error) {
		_, found = get(r)
		return nil, []string{r, a, rt, cs}, nil
	})
	return found, err
}

// Status says whether a server has a login bound to url, without its tokens.
func Status(store *secrets.Store, server, serverURL string) (signedIn bool, issuer string, expiry time.Time, err error) {
	rec, err := load(store, server)
	if errors.Is(err, ErrNoLogin) {
		return false, "", time.Time{}, nil
	}
	if err != nil {
		return false, "", time.Time{}, err
	}
	if err := rec.bound(serverURL); err != nil {
		return false, rec.Issuer, time.Time{}, err
	}
	if rec.Expiry > 0 {
		expiry = time.Unix(rec.Expiry, 0)
	}
	return true, rec.Issuer, expiry, nil
}

// bound refuses a login made for another URL than the server's now.
func (r Record) bound(serverURL string) error {
	c, err := Canonical(serverURL)
	if err != nil {
		return err
	}
	if c != r.URL {
		return fmt.Errorf("the login for MCP server %s was made for %s, but it is configured at %s; run abhed mcp login %s again",
			r.Server, printable(r.URL), printable(c), r.Server)
	}
	return nil
}

// tokenResponse is a token endpoint's answer.
type tokenResponse struct {
	AccessToken      string      `json:"access_token"`
	TokenType        string      `json:"token_type"`
	ExpiresIn        json.Number `json:"expires_in"`
	RefreshToken     string      `json:"refresh_token"`
	Scope            string      `json:"scope"`
	Error            string      `json:"error"`
	ErrorDescription string      `json:"error_description"`
}

// errInvalidGrant is the token endpoint refusing a code or refresh token.
var errInvalidGrant = errors.New("invalid_grant")

// maxToken bounds a token, far beyond any real one.
const maxToken = 16 << 10

// tokenRequest posts form to the token endpoint as the client rec names.
func tokenRequest(ctx context.Context, hc *http.Client, rec Record, form url.Values) (tokenResponse, error) {
	form.Set("client_id", rec.ClientID)
	if rec.ClientSecret != "" && rec.AuthMethod != "client_secret_basic" {
		form.Set("client_secret", rec.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if rec.ClientSecret != "" && rec.AuthMethod == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(rec.ClientID), url.QueryEscape(rec.ClientSecret))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("the token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return tokenResponse{}, err
	}
	var tr tokenResponse
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if len(body) > maxBody || dec.Decode(&tr) != nil {
		return tokenResponse{}, fmt.Errorf("the token endpoint answered %s with no token", resp.Status)
	}
	if resp.StatusCode != http.StatusOK || tr.Error != "" {
		if tr.Error == "invalid_grant" {
			return tr, fmt.Errorf("%w: %s", errInvalidGrant, printable(tr.ErrorDescription))
		}
		return tr, fmt.Errorf("the token endpoint refused (%s): %s %s", resp.Status, printable(tr.Error), printable(tr.ErrorDescription))
	}
	switch {
	case tr.AccessToken == "":
		return tr, errors.New("the token endpoint gave no access token")
	case len(tr.AccessToken) > maxToken || len(tr.RefreshToken) > maxToken:
		return tr, errors.New("the token endpoint gave a token over 16 KB")
	case !strings.EqualFold(tr.TokenType, "bearer"):
		return tr, fmt.Errorf("the token endpoint gave a %q token; only Bearer is sent", printable(tr.TokenType))
	case strings.ContainsAny(tr.AccessToken, "\r\n\x00 "):
		return tr, errors.New("the token endpoint gave an access token that cannot be sent in a header")
	}
	return tr, nil
}

// apply takes a token answer into rec.
func (r *Record) apply(tr tokenResponse, now time.Time) {
	r.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		r.RefreshToken = tr.RefreshToken
	}
	if tr.Scope != "" {
		r.Scope = tr.Scope
	}
	r.Expiry = 0
	if n, err := tr.ExpiresIn.Int64(); err == nil && n > 0 {
		r.Expiry = now.Add(time.Duration(n) * time.Second).Unix()
	}
}

// Authorizer gives the access token for one server's login, refreshing it.
type Authorizer struct {
	store  *secrets.Store
	server string
	hc     *http.Client
	now    func() time.Time

	mu  sync.Mutex
	rec Record
	// out is set once the login was found removed or replaced: it is then
	// never used or written again.
	out error
	// checked is when the store was last read for a logout or another
	// process's refresh.
	checked time.Time
}

// storeCheck is how often Token reads the store, so a logout reaches a
// running session within it.
const storeCheck = 5 * time.Second

// checkStore reads the login again: removed or replaced, it is never used
// again; refreshed by another process, its tokens are taken up. Under a.mu.
func (a *Authorizer) checkStore() error {
	stored, err := load(a.store, a.server)
	if err == nil || errors.Is(err, ErrNoLogin) {
		// Only a read that worked counts: an unreadable store fails every call until it reads.
		a.checked = a.now()
	}
	switch {
	case errors.Is(err, ErrNoLogin):
		a.out = fmt.Errorf("MCP server %s was signed out; run abhed mcp login %s", a.server, a.server)
	case err != nil:
		return err
	case stored.URL != a.rec.URL || stored.ClientID != a.rec.ClientID || stored.TokenEndpoint != a.rec.TokenEndpoint:
		a.out = fmt.Errorf("the login for MCP server %s changed while this ran; /mcp restart %s takes up the new one", a.server, a.server)
	case stored.AccessToken != a.rec.AccessToken && a.live(stored):
		a.rec = stored
	}
	if a.out != nil {
		a.rec = Record{Server: a.rec.Server, URL: a.rec.URL}
	}
	return a.out
}

// Load is the login for server, configured at serverURL, as an Authorizer;
// nil and nil when no one has signed in to it, and an error when the login
// was made for another URL. hc reaches the token endpoint: under the egress
// guard and the upstream proxy, following no redirect.
func Load(store *secrets.Store, server, serverURL string, hc *http.Client) (*Authorizer, error) {
	rec, err := load(store, server)
	if errors.Is(err, ErrNoLogin) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := rec.bound(serverURL); err != nil {
		return nil, err
	}
	return &Authorizer{store: store, server: server, hc: hc, now: time.Now, rec: rec}, nil
}

// expiryMargin is how early a token is refreshed before it ends.
const expiryMargin = time.Minute

// Token is the access token, refreshed first when it ends within a minute.
func (a *Authorizer) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.out != nil {
		return "", a.out
	}
	if a.now().Sub(a.checked) >= storeCheck {
		if err := a.checkStore(); err != nil {
			return "", err
		}
	}
	if a.rec.Expiry == 0 || a.now().Add(expiryMargin).Before(time.Unix(a.rec.Expiry, 0)) || a.rec.RefreshToken == "" {
		return a.rec.AccessToken, nil
	}
	if err := a.refresh(ctx); err != nil {
		return "", err
	}
	return a.rec.AccessToken, nil
}

// Refresh gets a new access token after the server refused failed; when
// another call already replaced failed, the new one is given without asking.
func (a *Authorizer) Refresh(ctx context.Context, failed string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.out != nil {
		return "", a.out
	}
	if a.rec.AccessToken != failed {
		return a.rec.AccessToken, nil
	}
	if err := a.refresh(ctx); err != nil {
		return "", err
	}
	return a.rec.AccessToken, nil
}

// live reports a record whose access token does not end within the margin.
func (a *Authorizer) live(r Record) bool {
	return r.Expiry == 0 || a.now().Add(expiryMargin).Before(time.Unix(r.Expiry, 0))
}

// refresh replaces the tokens; under a.mu, and under the login's lock across
// processes, so two never spend one refresh token and a removed login is
// never written back.
func (a *Authorizer) refresh(ctx context.Context) error {
	if a.out != nil {
		return a.out
	}
	r, _, _, _ := names(a.server)
	unlock, err := a.store.HeldLock(ctx, r)
	if err != nil {
		return fmt.Errorf("lock the login for MCP server %s: %w", a.server, err)
	}
	defer unlock()
	stored, err := load(a.store, a.server)
	switch {
	case errors.Is(err, ErrNoLogin):
		a.out = fmt.Errorf("MCP server %s was signed out; run abhed mcp login %s", a.server, a.server)
	case err != nil:
		return err
	case stored.URL != a.rec.URL || stored.ClientID != a.rec.ClientID || stored.TokenEndpoint != a.rec.TokenEndpoint:
		a.out = fmt.Errorf("the login for MCP server %s changed while this ran; /mcp restart %s takes up the new one", a.server, a.server)
	}
	if a.out != nil {
		a.rec = Record{Server: a.rec.Server, URL: a.rec.URL}
		return a.out
	}
	// Another process refreshed: its tokens are the live ones.
	if stored.AccessToken != a.rec.AccessToken {
		a.rec = stored
		if a.live(stored) {
			return nil
		}
	}
	if a.rec.RefreshToken == "" {
		return fmt.Errorf("the login for MCP server %s has ended and cannot be refreshed; run abhed mcp login %s", a.server, a.server)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.rec.RefreshToken}, "resource": {a.rec.URL}}
	tr, err := tokenRequest(ctx, a.hc, a.rec, form)
	if errors.Is(err, errInvalidGrant) {
		return fmt.Errorf("the login for MCP server %s was refused at refresh; run abhed mcp login %s", a.server, a.server)
	}
	if err != nil {
		return err
	}
	next := a.rec
	next.apply(tr, a.now())
	if err := saveLocked(a.store, next); err != nil {
		return fmt.Errorf("keep the refreshed login: %w", err)
	}
	a.rec = next
	return nil
}
