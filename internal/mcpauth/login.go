package mcpauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// LoginOptions is one sign-in to a server.
type LoginOptions struct {
	// Server is the configured name; URL its configured address.
	Server, URL string
	// ClientID and ClientSecret are a client registered ahead of time; with
	// none, one is registered dynamically where the server offers it.
	ClientID, ClientSecret string
	// Scopes asked for; none asks for what the server says it supports.
	Scopes []string
	// HTTP reaches the server and its authorization server: under the egress
	// guard and the upstream proxy, following no redirect.
	HTTP *http.Client
	// Open opens the authorization URL in the person's browser; nil or a
	// failure leaves them the printed URL.
	Open func(string) error
	// Out is told the URL to open and the outcome, never a token.
	Out io.Writer
	// Timeout bounds the wait for the browser; zero is five minutes.
	Timeout time.Duration
	now     func() time.Time
}

// random is n random bytes, URL-safe base64 without padding.
func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// callbackResult is what the loopback redirect brought back.
type callbackResult struct {
	code string
	err  error
}

// Login signs in to the server o names, through the person's browser, and
// keeps the tokens in store bound to the server's name and URL.
func Login(ctx context.Context, store *secrets.Store, o LoginOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	md, err := Discover(ctx, o.HTTP, o.URL)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for the sign-in's redirect: %w", err)
	}
	defer func() { _ = ln.Close() }()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	rec := Record{V: 1, Server: o.Server, URL: md.Resource, Issuer: md.Issuer, TokenEndpoint: md.TokenEndpoint,
		RevocationEndpoint: md.RevocationEndpoint, ClientID: o.ClientID, ClientSecret: o.ClientSecret}
	switch {
	case rec.ClientID != "":
		if rec.ClientSecret != "" {
			rec.AuthMethod = "client_secret_post"
			if slices.Contains(md.AuthMethods, "client_secret_basic") && !slices.Contains(md.AuthMethods, "client_secret_post") {
				rec.AuthMethod = "client_secret_basic"
			}
		}
	case md.RegistrationEndpoint != "":
		if err := register(ctx, o.HTTP, md, redirect, &rec); err != nil {
			return err
		}
	default:
		return fmt.Errorf("the authorization server %s offers no client registration; set oauth.client_id for MCP server %s", printable(md.Issuer), o.Server)
	}

	verifier, err := random(32)
	if err != nil {
		return err
	}
	state, err := random(32)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	authURL, err := url.Parse(md.AuthorizationEndpoint)
	if err != nil {
		return err
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", rec.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("resource", md.Resource)
	scopes := o.Scopes
	if len(scopes) == 0 {
		scopes = md.Scopes
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	authURL.RawQuery = q.Encode()

	results := make(chan callbackResult, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: callbackHandler(state, md, results)}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	fmt.Fprintf(o.Out, "Sign in to MCP server %s in your browser:\n  %s\n", o.Server, authURL.String())
	if o.Open != nil {
		if err := o.Open(authURL.String()); err != nil {
			fmt.Fprintln(o.Out, "Open that address yourself; the browser could not be started.")
		}
	}
	wait, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var res callbackResult
	select {
	case res = <-results:
	case <-wait.Done():
		return fmt.Errorf("no sign-in came back within %s", o.Timeout)
	}
	if res.err != nil {
		return res.err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {res.code}, "redirect_uri": {redirect},
		"code_verifier": {verifier}, "resource": {md.Resource}}
	tr, err := tokenRequest(ctx, o.HTTP, rec, form)
	if err != nil {
		return err
	}
	rec.apply(tr, o.now())
	if err := save(store, rec); err != nil {
		return fmt.Errorf("keep the login: %w", err)
	}
	fmt.Fprintf(o.Out, "Signed in to MCP server %s (%s).\n", o.Server, md.Issuer)
	return nil
}

// callbackHandler takes the one redirect that carries state; anything else
// is refused and leaves the wait going, so a stray request cannot end it.
func callbackHandler(state string, md Metadata, results chan<- callbackResult) http.Handler {
	page := func(w http.ResponseWriter, status int, msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<!doctype html><title>Abhed</title><p>%s</p>", html.EscapeString(msg))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			page(w, http.StatusBadRequest, "This sign-in is not the one Abhed started.")
			return
		}
		var res callbackResult
		switch {
		case md.ISSResponse && q.Get("iss") != md.Issuer:
			res.err = fmt.Errorf("the sign-in came back from %q, not %s", printable(q.Get("iss")), printable(md.Issuer))
		case q.Get("error") != "":
			res.err = fmt.Errorf("the authorization server refused the sign-in: %s %s", printable(q.Get("error")), printable(q.Get("error_description")))
		case q.Get("code") == "":
			res.err = errors.New("the sign-in came back with no code")
		default:
			res.code = q.Get("code")
		}
		select {
		case results <- res:
		default:
			page(w, http.StatusConflict, "This sign-in has already come back.")
			return
		}
		if res.err != nil {
			page(w, http.StatusOK, "The sign-in did not complete. Return to the terminal.")
			return
		}
		page(w, http.StatusOK, "Signed in. You can close this tab and return to the terminal.")
	})
}

// register registers Abhed as a public client (RFC 7591), with the
// loopback redirect, and takes the client it was given into rec.
func register(ctx context.Context, hc *http.Client, md Metadata, redirect string, rec *Record) error {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Abhed",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("client registration: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	var out struct {
		ClientID         string `json:"client_id"`
		ClientSecret     string `json:"client_secret"`
		AuthMethod       string `json:"token_endpoint_auth_method"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if len(data) > maxBody || json.Unmarshal(data, &out) != nil {
		return fmt.Errorf("client registration answered %s with no client", resp.Status)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK || out.ClientID == "" {
		return fmt.Errorf("client registration was refused (%s): %s %s", resp.Status, printable(out.Error), printable(out.ErrorDescription))
	}
	rec.ClientID, rec.ClientSecret, rec.AuthMethod = out.ClientID, out.ClientSecret, out.AuthMethod
	if rec.ClientSecret == "" {
		rec.AuthMethod = "none"
	} else if rec.AuthMethod != "client_secret_basic" {
		rec.AuthMethod = "client_secret_post"
	}
	return nil
}

// Logout revokes a server's login where its authorization server offers
// that, best effort, and forgets it; false when there was none.
func Logout(ctx context.Context, store *secrets.Store, server string, hc *http.Client) (bool, error) {
	rec, err := load(store, server)
	if errors.Is(err, ErrNoLogin) {
		return false, nil
	}
	if err == nil && rec.RevocationEndpoint != "" && hc != nil {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		for _, t := range []struct{ token, hint string }{{rec.RefreshToken, "refresh_token"}, {rec.AccessToken, "access_token"}} {
			if t.token == "" {
				continue
			}
			revoke(rctx, hc, rec, t.token, t.hint)
		}
		cancel()
	}
	return Remove(store, server)
}

// revoke asks the revocation endpoint to end token (RFC 7009); its answer is not needed.
func revoke(ctx context.Context, hc *http.Client, rec Record, token, hint string) {
	form := url.Values{"token": {token}, "token_type_hint": {hint}, "client_id": {rec.ClientID}}
	if rec.ClientSecret != "" && rec.AuthMethod != "client_secret_basic" {
		form.Set("client_secret", rec.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec.ClientSecret != "" && rec.AuthMethod == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(rec.ClientID), url.QueryEscape(rec.ClientSecret))
	}
	if resp, err := hc.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}
