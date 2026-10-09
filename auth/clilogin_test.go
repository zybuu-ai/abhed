package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type loginRig struct {
	t      *testing.T
	local  *LocalAuth
	tokens *CLITokens
	login  *CLILogin
	h      http.Handler
	cookie *http.Cookie
}

func newLoginRig(t *testing.T) *loginRig {
	t.Helper()
	local, tokens, _ := newTokens(t)
	login := NewCLILogin(tokens)
	mux := http.NewServeMux()
	local.Routes(mux)
	login.Routes(mux)
	mw := Middleware{Providers: []Provider{local}, Verifier: tokens,
		PublicPaths: append(local.PublicPaths(), login.PublicPaths()...)}
	r := &loginRig{t: t, local: local, tokens: tokens, login: login, h: mw.Wrap(mux)}
	rec := r.do("POST", "/v1/signin", `{"username":"ada","password":"correct-horse-battery"}`, "application/json", nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == local.CookieName {
			r.cookie = c
		}
	}
	if r.cookie == nil {
		t.Fatal("no session cookie")
	}
	return r
}

func (r *loginRig) do(method, path, body, ct string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *loginRig) signedIn(req *http.Request) { req.AddCookie(r.cookie) }

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func (r *loginRig) startDevice() (deviceCode, userCode string) {
	r.t.Helper()
	rec := r.do("POST", CLIDevicePath, `client_name=laptop`, "application/x-www-form-urlencoded", nil)
	if rec.Code != 200 {
		r.t.Fatalf("device start = %d %s", rec.Code, rec.Body)
	}
	m := decode(r.t, rec)
	if m["verification_uri"] != CLIPagePath || m["interval"].(float64) < 1 {
		r.t.Errorf("device start = %v", m)
	}
	return m["device_code"].(string), m["user_code"].(string)
}

func (r *loginRig) poll(deviceCode string) (int, map[string]any) {
	r.t.Helper()
	form := url.Values{"grant_type": {GrantDeviceCode}, "device_code": {deviceCode}}.Encode()
	rec := r.do("POST", CLITokenPath, form, "application/x-www-form-urlencoded", nil)
	return rec.Code, decode(r.t, rec)
}

func TestDeviceFlowApproveCollectOnce(t *testing.T) {
	r := newLoginRig(t)
	clock := time.Now()
	r.login.now = func() time.Time { return clock }
	dc, uc := r.startDevice()

	if code, m := r.poll(dc); code != 400 || m["error"] != errPending {
		t.Fatalf("first poll = %d %v", code, m)
	}
	if _, m := r.poll(dc); m["error"] != errSlowDown {
		t.Fatalf("an immediate second poll = %v, want slow_down", m)
	}
	// The approver sees where the request came from.
	rec := r.do("GET", CLIApprovePath+"?user_code="+strings.ToLower(strings.ReplaceAll(uc, "-", "")), "", "", r.signedIn)
	if m := decode(t, rec); rec.Code != 200 || m["client_name"] != "laptop" || m["from"] == "" {
		t.Fatalf("describe = %d %v", rec.Code, m)
	}
	rec = r.do("POST", CLIApprovePath, `{"user_code":"`+uc+`","approve":true}`, "application/json", r.signedIn)
	if rec.Code != 200 {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	rec = r.do("POST", CLIApprovePath, `{"user_code":"`+uc+`","approve":false}`, "application/json", r.signedIn)
	if rec.Code == 200 {
		t.Fatal("an answered sign-in was answered again")
	}
	clock = clock.Add(20 * time.Second)
	code, m := r.poll(dc)
	if code != 200 || m["username"] != "ada" || !strings.HasPrefix(m["access_token"].(string), CLITokenPrefix) {
		t.Fatalf("poll after approval = %d %v", code, m)
	}
	tok := m["access_token"].(string)
	// The token works as a bearer.
	if id, err := r.tokens.Verify(context.Background(), tok); err != nil || id.Subject != "ada" {
		t.Fatalf("issued token: %v %v", id, err)
	}
	// Replay: the device code is spent.
	if _, m := r.poll(dc); m["error"] != errExpired {
		t.Fatalf("replayed device code = %v", m)
	}
}

func TestDeviceFlowDenyAndExpiry(t *testing.T) {
	r := newLoginRig(t)
	clock := time.Now()
	r.login.now = func() time.Time { return clock }
	dc, uc := r.startDevice()
	r.do("POST", CLIApprovePath, `{"user_code":"`+uc+`","approve":false}`, "application/json", r.signedIn)
	if _, m := r.poll(dc); m["error"] != errDenied {
		t.Fatalf("denied = %v", m)
	}
	if _, m := r.poll(dc); m["error"] != errExpired {
		t.Fatalf("after denial = %v", m)
	}

	dc, uc = r.startDevice()
	clock = clock.Add(DeviceCodeTTL + time.Second)
	rec := r.do("POST", CLIApprovePath, `{"user_code":"`+uc+`","approve":true}`, "application/json", r.signedIn)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("approving an expired code = %d", rec.Code)
	}
	if _, m := r.poll(dc); m["error"] != errExpired {
		t.Fatalf("expired device code = %v", m)
	}
	if _, m := r.poll("never-issued"); m["error"] != errExpired {
		t.Fatalf("unknown device code = %v", m)
	}
}

func TestApprovalNeedsABrowserSessionAndJSON(t *testing.T) {
	r := newLoginRig(t)
	_, uc := r.startDevice()
	body := `{"user_code":"` + uc + `","approve":true}`
	if rec := r.do("POST", CLIApprovePath, body, "application/json", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated approve = %d", rec.Code)
	}
	// A form post is what a cross-site page can send without a preflight.
	for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", "multipart/form-data; boundary=x", ""} {
		if rec := r.do("POST", CLIApprovePath, body, ct, r.signedIn); rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("approve as %q = %d", ct, rec.Code)
		}
	}
	// A token cannot approve a sign-in that would mint another.
	tok, _, _ := r.tokens.Issue(context.Background(), "ada")
	rec := r.do("POST", CLIApprovePath, body, "application/json", func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+tok)
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("approve with a bearer token = %d", rec.Code)
	}
	// Another provider's identity (an outer router's) cannot approve.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", CLIApprovePath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(WithIdentity(req.Context(), &Identity{Subject: "ada", Provider: "oidc"}))
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("approve as an oidc identity = %d", rec.Code)
	}
}

func pkcePair() (verifier, challenge string) {
	verifier = strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (r *loginRig) authorize(challenge, redirect string, approve bool) *httptest.ResponseRecorder {
	b, _ := json.Marshal(authorizeRequest{CodeChallenge: challenge, CodeChallengeMethod: "S256",
		RedirectURI: redirect, State: "st", Approve: approve})
	return r.do("POST", CLIAuthorizePath, string(b), "application/json", r.signedIn)
}

func (r *loginRig) exchange(code, verifier, redirect string) (int, map[string]any) {
	form := url.Values{"grant_type": {GrantAuthCode}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}}
	rec := r.do("POST", CLITokenPath, form.Encode(), "application/x-www-form-urlencoded", nil)
	return rec.Code, decode(r.t, rec)
}

func TestPKCEFlow(t *testing.T) {
	r := newLoginRig(t)
	verifier, challenge := pkcePair()
	redirect := "http://127.0.0.1:43123/callback"
	rec := r.authorize(challenge, redirect, true)
	if rec.Code != 200 {
		t.Fatalf("authorize = %d %s", rec.Code, rec.Body)
	}
	to, _ := url.Parse(decode(t, rec)["redirect"].(string))
	if to.Host != "127.0.0.1:43123" || to.Query().Get("state") != "st" || to.Query().Get("code") == "" {
		t.Fatalf("redirect %s", to)
	}
	code := to.Query().Get("code")
	if status, m := r.exchange(code, strings.Repeat("w", 50), redirect); status == 200 || m["error"] != errGrant {
		t.Fatalf("wrong verifier = %d %v", status, m)
	}
	// The failed attempt spent the code: no second guess.
	if status, _ := r.exchange(code, verifier, redirect); status == 200 {
		t.Fatal("a code was redeemed after a failed attempt")
	}

	rec = r.authorize(challenge, redirect, true)
	to, _ = url.Parse(decode(t, rec)["redirect"].(string))
	code = to.Query().Get("code")
	if status, m := r.exchange(code, verifier, "http://127.0.0.1:9/callback"); status == 200 || m["error"] != errGrant {
		t.Fatalf("other redirect = %d %v", status, m)
	}
	rec = r.authorize(challenge, redirect, true)
	to, _ = url.Parse(decode(t, rec)["redirect"].(string))
	code = to.Query().Get("code")
	status, m := r.exchange(code, verifier, redirect)
	if status != 200 || m["username"] != "ada" {
		t.Fatalf("good exchange = %d %v", status, m)
	}
	if status, _ := r.exchange(code, verifier, redirect); status == 200 {
		t.Fatal("a code was redeemed twice")
	}

	// Expiry.
	rec = r.authorize(challenge, redirect, true)
	to, _ = url.Parse(decode(t, rec)["redirect"].(string))
	r.login.now = func() time.Time { return time.Now().Add(AuthCodeTTL + time.Second) }
	if status, _ := r.exchange(to.Query().Get("code"), verifier, redirect); status == 200 {
		t.Fatal("an expired code was redeemed")
	}
	r.login.now = time.Now

	// Denied: the redirect carries the error and no code.
	rec = r.authorize(challenge, redirect, false)
	to, _ = url.Parse(decode(t, rec)["redirect"].(string))
	if to.Query().Get("error") != errDenied || to.Query().Get("code") != "" {
		t.Fatalf("deny redirect %s", to)
	}
}

func TestPKCERefusesRemoteRedirectsAndPlainChallenges(t *testing.T) {
	r := newLoginRig(t)
	_, challenge := pkcePair()
	for _, u := range []string{
		"https://evil.example/cb", "http://evil.example:80/cb", "http://127.0.0.1/cb",
		"http://127.0.0.1:8/cb?x=1", "http://user@127.0.0.1:8/cb", "http://127.0.0.1.evil.example:8/cb",
		"javascript:alert(1)", "",
	} {
		if rec := r.authorize(challenge, u, true); rec.Code != http.StatusBadRequest {
			t.Errorf("redirect %q = %d", u, rec.Code)
		}
	}
	b, _ := json.Marshal(authorizeRequest{CodeChallenge: "short", CodeChallengeMethod: "plain",
		RedirectURI: "http://127.0.0.1:5/cb", Approve: true})
	if rec := r.do("POST", CLIAuthorizePath, string(b), "application/json", r.signedIn); rec.Code != http.StatusBadRequest {
		t.Errorf("plain challenge = %d", rec.Code)
	}
	for _, u := range []string{"http://127.0.0.1:5/cb", "http://[::1]:5/cb", "http://localhost:5/cb"} {
		if !LoopbackRedirect(u) {
			t.Errorf("%s refused", u)
		}
	}
}

func TestPendingSignInsAreBounded(t *testing.T) {
	r := newLoginRig(t)
	for range maxPending {
		r.startDevice()
	}
	rec := r.do("POST", CLIDevicePath, "", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("start past the bound = %d", rec.Code)
	}
}

func TestUserCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c := newUserCode()
		if NormalizeUserCode(c) != c || len(c) != 9 || c[4] != '-' {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 195 {
		t.Errorf("only %d distinct codes in 200", len(seen))
	}
	for in, want := range map[string]string{"\tbcdf ghjk\n": "BCDF-GHJK", "bcdfghjk": "BCDF-GHJK", "BCDF-GHJA": "", "BCD": ""} {
		if got := NormalizeUserCode(in); got != want {
			t.Errorf("NormalizeUserCode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cleanClient(`<script>alert("x")</script>` + strings.Repeat("y", 100)); strings.ContainsAny(got, `<>"`) || len([]rune(got)) > 64 {
		t.Errorf("client name %q", got)
	}
}
