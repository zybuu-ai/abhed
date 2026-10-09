package profiles

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Method is one sign-in method a server offers, from its discovery document.
// The fields follow the server's auth.CLILoginMethod.
type Method struct {
	Name                        string   `json:"name"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint,omitempty"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint               string   `json:"token_endpoint"`
	RevocationEndpoint          string   `json:"revocation_endpoint,omitempty"`
	ClientID                    string   `json:"client_id,omitempty"`
	Scopes                      []string `json:"scopes,omitempty"`
	Issuer                      string   `json:"issuer,omitempty"`
}

// Discovery is what a server says about signing a command line in.
type Discovery struct {
	AuthMode string   `json:"auth_mode"`
	Methods  []Method `json:"methods"`
}

// DiscoveryPath is the server's discovery document.
const DiscoveryPath = "/v1/cli"

// Token is a successful token response.
type Token struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Username     string `json:"username,omitempty"`
}

// OAuthError is an error response from a token endpoint.
type OAuthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return e.Description
	}
	return e.Code
}

// Discover reads base's discovery document.
func Discover(ctx context.Context, c *http.Client, base string) (*Discovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+DiscoveryPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s does not offer command-line sign-in (no %s); it may run an older version", base, DiscoveryPath)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s%s answered %s", base, DiscoveryPath, resp.Status)
	}
	var d Discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, fmt.Errorf("%s%s: %w", base, DiscoveryPath, err)
	}
	return &d, nil
}

// Resolve makes an endpoint absolute against base. An absolute endpoint on
// another host is allowed only over https.
func Resolve(base, endpoint string) (string, error) {
	if endpoint == "" {
		return "", errors.New("the server named no endpoint for this step")
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	e, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if !e.IsAbs() {
		if !strings.HasPrefix(endpoint, "/") {
			return "", fmt.Errorf("endpoint %q is neither a path nor a URL", endpoint)
		}
		return strings.TrimRight(base, "/") + endpoint, nil
	}
	if e.Host != b.Host && e.Scheme != "https" {
		return "", fmt.Errorf("endpoint %s is on another host over plain http", endpoint)
	}
	return e.String(), nil
}

func postForm(ctx context.Context, c *http.Client, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return c.Do(req)
}

// requestToken posts form to a token endpoint and reads the answer.
func requestToken(ctx context.Context, c *http.Client, endpoint string, form url.Values) (*Token, error) {
	resp, err := postForm(ctx, c, endpoint, form)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return readToken(resp)
}

func readToken(resp *http.Response) (*Token, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e OAuthError
		if json.Unmarshal(b, &e) == nil && e.Code != "" {
			return nil, &e
		}
		return nil, fmt.Errorf("token endpoint answered %s", resp.Status)
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil || t.AccessToken == "" {
		return nil, errors.New("token endpoint gave no token")
	}
	return &t, nil
}

// DevicePrompt is told the code to enter and where, once the server has
// issued them.
type DevicePrompt func(userCode, verificationURI, verificationURIComplete string)

// DeviceLogin signs in by RFC 8628 device authorization: the person enters
// the code in a browser where they are signed in, and this polls until they
// approve, deny or the code expires.
func DeviceLogin(ctx context.Context, c *http.Client, base string, m Method, clientName string, prompt DevicePrompt) (*Token, error) {
	start, err := Resolve(base, m.DeviceAuthorizationEndpoint)
	if err != nil {
		return nil, err
	}
	tokenURL, err := Resolve(base, m.TokenEndpoint)
	if err != nil {
		return nil, err
	}
	form := url.Values{"client_name": {clientName}}
	if m.ClientID != "" {
		form.Set("client_id", m.ClientID)
	}
	if len(m.Scopes) > 0 {
		form.Set("scope", strings.Join(m.Scopes, " "))
	}
	resp, err := postForm(ctx, c, start, form)
	if err != nil {
		return nil, err
	}
	var da struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e OAuthError
		if json.Unmarshal(b, &e) == nil && e.Code != "" {
			return nil, &e
		}
		return nil, fmt.Errorf("starting the sign-in: %s", resp.Status)
	}
	if err := json.Unmarshal(b, &da); err != nil || da.DeviceCode == "" || da.UserCode == "" {
		return nil, errors.New("the server's answer to a sign-in start was not understood")
	}
	verify, err := Resolve(base, da.VerificationURI)
	if err != nil {
		return nil, err
	}
	complete := ""
	if da.VerificationURIComplete != "" {
		complete, _ = Resolve(base, da.VerificationURIComplete)
	}
	prompt(da.UserCode, verify, complete)

	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if da.ExpiresIn <= 0 {
		da.ExpiresIn = 600
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(da.ExpiresIn)*time.Second)
	defer cancel()
	poll := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {da.DeviceCode}}
	if m.ClientID != "" {
		poll.Set("client_id", m.ClientID)
	}
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errors.New("the code expired before it was approved; run abhed login again")
			}
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		t, err := requestToken(ctx, c, tokenURL, poll)
		var oe *OAuthError
		switch {
		case err == nil:
			return t, nil
		case errors.As(err, &oe) && oe.Code == "authorization_pending":
		case errors.As(err, &oe) && oe.Code == "slow_down":
			interval += 5 * time.Second
		default:
			return nil, err
		}
	}
}

// Opener opens a URL in the person's browser.
type Opener func(url string) error

// PKCELogin signs in by an authorization code with PKCE (RFC 7636): the
// browser approves on the server, which sends a one-minute code back to a
// listener on this machine's loopback address, exchanged with the verifier.
func PKCELogin(ctx context.Context, c *http.Client, base string, m Method, open Opener) (*Token, error) {
	authURL, err := Resolve(base, m.AuthorizationEndpoint)
	if err != nil {
		return nil, err
	}
	tokenURL, err := Resolve(base, m.TokenEndpoint)
	if err != nil {
		return nil, err
	}
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(24)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open a loopback port for the browser to return to: %w", err)
	}
	redirect := "http://" + ln.Addr().String() + "/callback"
	type result struct {
		code string
		err  error
	}
	got := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var res result
		switch {
		case q.Get("state") != state:
			// Not ours: another page cannot finish this sign-in.
			http.Error(w, "this sign-in was not started here", http.StatusBadRequest)
			return
		case q.Get("error") != "":
			res.err = fmt.Errorf("the sign-in was not approved (%s)", q.Get("error"))
		case q.Get("code") == "":
			res.err = errors.New("the server returned no code")
		default:
			res.code = q.Get("code")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if res.err != nil {
			_, _ = io.WriteString(w, "Sign-in not completed. You can close this tab.\n")
		} else {
			_, _ = io.WriteString(w, "Signed in. You can close this tab and return to the terminal.\n")
		}
		select {
		case got <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	q := url.Values{"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"redirect_uri": {redirect}, "state": {state}, "response_type": {"code"}}
	if m.ClientID != "" {
		q.Set("client_id", m.ClientID)
	}
	if len(m.Scopes) > 0 {
		q.Set("scope", strings.Join(m.Scopes, " "))
	}
	sep := "?"
	if strings.Contains(authURL, "?") {
		sep = "&"
	}
	if err := open(authURL + sep + q.Encode()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var res result
	select {
	case <-ctx.Done():
		return nil, errors.New("no answer from the browser; run abhed login again, or use -device")
	case res = <-got:
	}
	if res.err != nil {
		return nil, res.err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {res.code},
		"code_verifier": {verifier}, "redirect_uri": {redirect}}
	if m.ClientID != "" {
		form.Set("client_id", m.ClientID)
	}
	return requestToken(ctx, c, tokenURL, form)
}

// Refresh exchanges p's refresh token for a new access token, in place.
func Refresh(ctx context.Context, c *http.Client, p *Profile) error {
	if p.RefreshToken == "" || p.TokenEndpoint == "" {
		return errors.New("this sign-in cannot be refreshed; run abhed login again")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {p.RefreshToken}}
	if p.ClientID != "" {
		form.Set("client_id", p.ClientID)
	}
	t, err := requestToken(ctx, c, p.TokenEndpoint, form)
	if err != nil {
		return err
	}
	p.Apply(t, time.Now())
	return nil
}

// Apply stores a token response on the profile.
func (p *Profile) Apply(t *Token, now time.Time) {
	p.Token, p.TokenType, p.IssuedAt = t.AccessToken, t.TokenType, now
	if t.RefreshToken != "" {
		p.RefreshToken = t.RefreshToken
	}
	p.ExpiresAt = time.Time{}
	if t.ExpiresIn > 0 {
		p.ExpiresAt = now.Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	if t.Username != "" {
		p.Username = t.Username
	}
}

// Revoke tells the server to end p's token. A server that no longer knows it,
// or cannot be reached, leaves nothing to undo here.
func Revoke(ctx context.Context, c *http.Client, p *Profile) error {
	if p.Token == "" || p.RevocationEndpoint == "" {
		return nil
	}
	req, err := p.NewRequest(http.MethodPost, "", nil)
	if err != nil {
		return err
	}
	u, err := url.Parse(p.RevocationEndpoint)
	if err != nil {
		return err
	}
	req.URL = u
	req = req.WithContext(ctx)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("the server could not record the sign-out: %s", resp.Status)
	}
	return nil
}

// NearExpiry reports whether p's token has a tenth of its life or less left
// at now. A token with no expiry never is.
func (p *Profile) NearExpiry(now time.Time) bool {
	if p.Token == "" || p.ExpiresAt.IsZero() {
		return false
	}
	if p.IssuedAt.IsZero() || !p.IssuedAt.Before(p.ExpiresAt) {
		return !now.Before(p.ExpiresAt)
	}
	return !now.Before(p.ExpiresAt.Add(-p.ExpiresAt.Sub(p.IssuedAt) / 10))
}

// Do sends the request build makes with p's token. With a refresh token it
// refreshes first when the token is near its expiry, and once more if the
// server answers 401, sending the request again; saved, when set, is called
// after each refresh so the new tokens are kept. build is called once per
// attempt, so a request body can be read again.
func (p *Profile) Do(ctx context.Context, c *http.Client, build func() (*http.Request, error), saved func(*Profile) error) (*http.Response, error) {
	refresh := func() error {
		if err := Refresh(ctx, c, p); err != nil {
			return err
		}
		if saved != nil {
			return saved(p)
		}
		return nil
	}
	refreshed := false
	if p.RefreshToken != "" && p.NearExpiry(time.Now()) {
		// A failed refresh still sends the token it has; the server decides.
		refreshed = refresh() == nil
	}
	send := func() (*http.Response, error) {
		req, err := build()
		if err != nil {
			return nil, err
		}
		return c.Do(req.WithContext(ctx))
	}
	resp, err := send()
	if err != nil || resp.StatusCode != http.StatusUnauthorized || p.RefreshToken == "" || refreshed {
		return resp, err
	}
	if rerr := refresh(); rerr != nil {
		return resp, nil
	}
	_ = resp.Body.Close()
	return send()
}

// Whoami asks the server who p's token signs in, refreshing it as Do does;
// the caller saves p if it changed.
func Whoami(ctx context.Context, c *http.Client, p *Profile) (map[string]any, error) {
	resp, err := p.Do(ctx, c, func() (*http.Request, error) {
		return p.NewRequest(http.MethodGet, "/v1/cli/whoami", nil)
	}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		reason, _ := out["reason"].(string)
		if reason == "" {
			reason, _ = out["error"].(string)
		}
		return nil, fmt.Errorf("the server refused the token (%s): %s", resp.Status, reason)
	}
	return out, nil
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("profiles: system random source unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
