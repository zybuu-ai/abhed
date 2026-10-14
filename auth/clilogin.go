package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Command-line sign-in for local accounts, in the two OAuth shapes a terminal
// can use (RFC 8628 device authorization, RFC 7636 PKCE with a loopback
// redirect). Either way a person already signed in to the console approves the
// request in the browser, and the command line receives a CLITokens token.
//
// Codes live in memory on the server that issued them: short-lived, single
// use, bounded in number. Behind several servers the sign-in must reach the
// same one for its few minutes, which a sticky route or a single node gives.

// CLILoginMethod describes one way `abhed login` can sign in, as the
// discovery document lists it. Endpoints are absolute paths on this server or
// absolute URLs, for a method an identity provider serves.
type CLILoginMethod struct {
	Name string `json:"name"`
	// DeviceAuthorizationEndpoint starts an RFC 8628 device authorization.
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint,omitempty"`
	// AuthorizationEndpoint is where the browser goes for an RFC 7636 code.
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint"`
	// RevocationEndpoint, when set, is told the token at logout.
	RevocationEndpoint string   `json:"revocation_endpoint,omitempty"`
	ClientID           string   `json:"client_id,omitempty"`
	Scopes             []string `json:"scopes,omitempty"`
	// Issuer names the identity provider, for a method that is not this server.
	Issuer string `json:"issuer,omitempty"`
}

// CLILoginAdvertiser is implemented by a Provider that offers command-line
// sign-in of its own, an identity provider's device flow for instance. Its
// methods are listed in the discovery document after this server's own; the
// tokens it leads to must be accepted by the middleware's Verifier.
type CLILoginAdvertiser interface {
	CLILoginMethods() []CLILoginMethod
}

// OAuth grant types and errors, as the RFCs spell them.
const (
	GrantDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"
	GrantAuthCode   = "authorization_code"

	errPending  = "authorization_pending"
	errSlowDown = "slow_down"
	errDenied   = "access_denied"
	errExpired  = "expired_token"
	errGrant    = "invalid_grant"
	errRequest  = "invalid_request"
)

// Lifetimes and bounds.
const (
	DeviceCodeTTL   = 10 * time.Minute
	AuthCodeTTL     = time.Minute
	DevicePollEvery = 5 * time.Second
	// maxPending bounds the codes awaiting approval, so an unauthenticated
	// caller cannot grow the table without limit.
	maxPending = 1024
)

// userCodeAlphabet has no vowels or look-alike characters: no words, no 0/O.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// CLILogin holds the sign-ins in progress for one server.
type CLILogin struct {
	Tokens *CLITokens
	// Interval is the polling interval a device sign-in is given; at least
	// a second, DevicePollEvery when zero.
	Interval time.Duration

	mu      sync.Mutex
	devices map[string]*deviceGrant // by device-code digest
	byUser  map[string]string       // user code -> device-code digest
	codes   map[string]*authCode    // by code digest
	now     func() time.Time
}

type deviceGrant struct {
	userCode  string
	client    string
	from      string
	expires   time.Time
	lastPoll  time.Time
	interval  time.Duration
	decided   bool
	allowed   bool
	approver  string
	createdAt time.Time
}

type authCode struct {
	challenge string
	redirect  string
	username  string
	expires   time.Time
}

// NewCLILogin serves command-line sign-in with tokens from t.
func NewCLILogin(t *CLITokens) *CLILogin {
	return &CLILogin{Tokens: t, devices: map[string]*deviceGrant{},
		byUser: map[string]string{}, codes: map[string]*authCode{}, now: time.Now}
}

// Paths this server answers for command-line sign-in.
const (
	CLIDevicePath    = "/v1/cli/device"
	CLITokenPath     = "/v1/cli/token" // #nosec G101 -- a URL path, not a credential
	CLIApprovePath   = "/v1/cli/approve"
	CLIAuthorizePath = "/v1/cli/authorize"
	CLIRevokePath    = "/v1/cli/logout"
	// CLIPagePath is the browser page where a request is approved.
	CLIPagePath = "/cli/login"
)

// Method is this server's own entry in the discovery document.
func (l *CLILogin) Method() CLILoginMethod {
	return CLILoginMethod{Name: "local", DeviceAuthorizationEndpoint: CLIDevicePath,
		AuthorizationEndpoint: CLIPagePath, TokenEndpoint: CLITokenPath, RevocationEndpoint: CLIRevokePath}
}

// PublicPaths answer before sign-in: starting a request and collecting the
// token. Approving one, and revoking a token, need a signed-in caller.
func (l *CLILogin) PublicPaths() []string { return []string{CLIDevicePath, CLITokenPath} }

// Routes registers the JSON endpoints. The approval page is the server's.
func (l *CLILogin) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+CLIDevicePath, l.startDevice)
	mux.HandleFunc("POST "+CLITokenPath, l.token)
	mux.HandleFunc("POST "+CLIApprovePath, l.approve)
	mux.HandleFunc("POST "+CLIAuthorizePath, l.authorize)
	mux.HandleFunc("POST "+CLIRevokePath, l.revoke)
	mux.HandleFunc("GET "+CLIApprovePath, l.describe)
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func newUserCode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("auth: system random source unavailable: " + err.Error())
	}
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		// 256 is not a multiple of 20; the bias is under 2% per character and
		// the code is single use, short-lived and needs a signed-in approver.
		out = append(out, userCodeAlphabet[int(c)%len(userCodeAlphabet)])
	}
	return string(out)
}

// NormalizeUserCode accepts a code as a person types it: any case, with or
// without the dash or spaces.
func NormalizeUserCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
	if len(s) != 8 {
		return ""
	}
	for _, c := range s {
		if !strings.ContainsRune(userCodeAlphabet, c) {
			return ""
		}
	}
	return s[:4] + "-" + s[4:]
}

// sweep drops what has expired; l.mu is held.
func (l *CLILogin) sweep(now time.Time) {
	for k, d := range l.devices {
		if now.After(d.expires) {
			delete(l.devices, k)
			delete(l.byUser, d.userCode)
		}
	}
	for k, c := range l.codes {
		if now.After(c.expires) {
			delete(l.codes, k)
		}
	}
}

var clientNameRe = regexp.MustCompile(`[^\pL\pN ._@()/-]`)

// cleanClient keeps a client's self-description printable and short; it is
// shown to the approver, so it must not be able to pass for page text.
func cleanClient(s string) string {
	s = clientNameRe.ReplaceAllString(strings.TrimSpace(s), "")
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64])
	}
	if s == "" {
		s = "abhed command line"
	}
	return s
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type deviceStart struct {
	Client string `json:"client_name"`
}

func (l *CLILogin) startDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceStart
	if r.ContentLength != 0 {
		if err := decodeForm(r, &req); err != nil {
			oauthError(w, http.StatusBadRequest, errRequest, err.Error())
			return
		}
	}
	deviceCode := randomToken()
	now := l.now()
	l.mu.Lock()
	l.sweep(now)
	if len(l.devices) >= maxPending {
		l.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many sign-ins waiting; try again in a minute")
		return
	}
	var userCode string
	for {
		userCode = newUserCode()
		if _, taken := l.byUser[userCode]; !taken {
			break
		}
	}
	interval := l.Interval
	if interval < time.Second {
		interval = DevicePollEvery
	}
	key := digest(deviceCode)
	l.devices[key] = &deviceGrant{userCode: userCode, client: cleanClient(req.Client),
		from: remoteHost(r), expires: now.Add(DeviceCodeTTL), interval: interval, createdAt: now}
	l.byUser[userCode] = key
	l.mu.Unlock()

	writeAuthJSON(w, http.StatusOK, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          CLIPagePath,
		"verification_uri_complete": CLIPagePath + "?user_code=" + url.QueryEscape(userCode),
		"expires_in":                int(DeviceCodeTTL.Seconds()),
		"interval":                  int(interval.Seconds()),
	})
}

// decodeForm reads a JSON body, or a form one as RFC 8628 and 6749 send it.
func decodeForm(r *http.Request, v any) error {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return errors.New("unreadable form")
		}
		m := map[string]string{}
		for k := range r.PostForm {
			m[k] = r.PostForm.Get(k)
		}
		b, _ := json.Marshal(m)
		return json.Unmarshal(b, v)
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.New("unreadable request")
	}
	return nil
}

func oauthError(w http.ResponseWriter, code int, kind, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeAuthJSON(w, code, map[string]string{"error": kind, "error_description": desc})
}

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	DeviceCode   string `json:"device_code"`
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

func (l *CLILogin) token(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := decodeForm(r, &req); err != nil {
		oauthError(w, http.StatusBadRequest, errRequest, err.Error())
		return
	}
	var username string
	switch req.GrantType {
	case GrantDeviceCode:
		u, code, kind, desc := l.pollDevice(req.DeviceCode)
		if kind != "" {
			oauthError(w, code, kind, desc)
			return
		}
		username = u
	case GrantAuthCode:
		u, kind, desc := l.exchange(req.Code, req.CodeVerifier, req.RedirectURI)
		if kind != "" {
			oauthError(w, http.StatusBadRequest, kind, desc)
			return
		}
		username = u
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use the device code or authorization code grant")
		return
	}
	tok, exp, err := l.Tokens.Issue(r.Context(), username)
	if err != nil {
		oauthError(w, http.StatusForbidden, errDenied, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "token_type": "Bearer",
		"expires_in": int(time.Until(exp).Seconds()), "username": username,
	})
}

// pollDevice answers one poll. An approved grant is consumed by the poll that
// collects it, so the device code cannot be used twice.
func (l *CLILogin) pollDevice(deviceCode string) (username string, status int, kind, desc string) {
	if deviceCode == "" {
		return "", http.StatusBadRequest, errRequest, "device_code is missing"
	}
	key := digest(deviceCode)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.devices[key]
	if !ok {
		return "", http.StatusBadRequest, errExpired, "this sign-in has expired or was already used; run abhed login again"
	}
	if now.After(d.expires) {
		delete(l.devices, key)
		delete(l.byUser, d.userCode)
		return "", http.StatusBadRequest, errExpired, "this sign-in has expired; run abhed login again"
	}
	if !d.decided {
		if !d.lastPoll.IsZero() && now.Sub(d.lastPoll) < d.interval {
			// RFC 8628 §3.5: a client polling too fast waits five seconds more.
			d.interval += 5 * time.Second
			d.lastPoll = now
			return "", http.StatusBadRequest, errSlowDown, "polling too often"
		}
		d.lastPoll = now
		return "", http.StatusBadRequest, errPending, "waiting for approval in the browser"
	}
	delete(l.devices, key)
	delete(l.byUser, d.userCode)
	if !d.allowed {
		return "", http.StatusBadRequest, errDenied, "the sign-in was denied in the browser"
	}
	return d.approver, 0, "", ""
}

// approver is the local account behind a browser request, or an error for a
// caller who may not approve: no session, a token, or another provider.
func approver(r *http.Request) (string, error) {
	if bearerToken(r) != "" {
		return "", errors.New("approve a sign-in from a browser session, not with a token")
	}
	id, ok := FromContext(r.Context())
	if !ok || id == nil || id.Provider != ProviderLocal || id.Subject == "" {
		return "", errors.New("command-line sign-in is for local accounts; sign in to the console with one first")
	}
	return id.Subject, nil
}

// requireJSON refuses a body a cross-site form could send: only script on a
// page of this origin can send application/json, and it needs the origin check.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
		writeAuthJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
		return false
	}
	return true
}

type approveRequest struct {
	UserCode string `json:"user_code"`
	Approve  bool   `json:"approve"`
}

func (l *CLILogin) approve(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	who, err := approver(r)
	if err != nil {
		writeAuthJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	code := NormalizeUserCode(req.UserCode)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.devices[l.byUser[code]]
	if code == "" || d == nil || now.After(d.expires) {
		writeAuthJSON(w, http.StatusNotFound, map[string]string{"error": "no sign-in is waiting with that code; it may have expired"})
		return
	}
	if d.decided {
		writeAuthJSON(w, http.StatusConflict, map[string]string{"error": "that sign-in was already answered"})
		return
	}
	d.decided, d.allowed, d.approver = true, req.Approve, who
	writeAuthJSON(w, http.StatusOK, map[string]any{"approved": req.Approve, "username": who})
}

// describe tells the approval page what is waiting behind a user code, so the
// person sees where the request came from before deciding.
func (l *CLILogin) describe(w http.ResponseWriter, r *http.Request) {
	if _, err := approver(r); err != nil {
		writeAuthJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	code := NormalizeUserCode(r.URL.Query().Get("user_code"))
	now := l.now()
	l.mu.Lock()
	d := l.devices[l.byUser[code]]
	var out map[string]any
	if code != "" && d != nil && now.Before(d.expires) && !d.decided {
		out = map[string]any{"user_code": d.userCode, "client_name": d.client, "from": d.from,
			"requested_at": d.createdAt.UTC().Format(time.RFC3339), "expires_in": int(d.expires.Sub(now).Seconds())}
	}
	l.mu.Unlock()
	if out == nil {
		writeAuthJSON(w, http.StatusNotFound, map[string]string{"error": "no sign-in is waiting with that code; it may have expired"})
		return
	}
	writeAuthJSON(w, http.StatusOK, out)
}

// LoopbackRedirect reports whether u is a redirect the command line can
// receive on this machine: plain http to a loopback address with a port.
// Anything else would hand the code to whoever runs that host.
func LoopbackRedirect(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "http" || p.User != nil || p.Port() == "" || p.Fragment != "" || p.RawQuery != "" {
		return false
	}
	switch p.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

type authorizeRequest struct {
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	RedirectURI         string `json:"redirect_uri"`
	State               string `json:"state"`
	Approve             bool   `json:"approve"`
}

var challengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// authorize issues a one-minute code for the signed-in person, bound to the
// PKCE challenge and the loopback redirect, and says where to send it.
func (l *CLILogin) authorize(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	who, err := approver(r)
	if err != nil {
		writeAuthJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	var req authorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if !LoopbackRedirect(req.RedirectURI) {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "redirect_uri must be http://127.0.0.1:PORT/... on this machine"})
		return
	}
	if req.CodeChallengeMethod != "S256" || !challengeRe.MatchString(req.CodeChallenge) {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "a S256 code_challenge is required"})
		return
	}
	if len(req.State) > 256 {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "state is too long"})
		return
	}
	q := url.Values{"state": {req.State}}
	if !req.Approve {
		q.Set("error", errDenied)
		writeAuthJSON(w, http.StatusOK, map[string]string{"redirect": req.RedirectURI + "?" + q.Encode()})
		return
	}
	code := randomToken()
	now := l.now()
	l.mu.Lock()
	l.sweep(now)
	if len(l.codes) >= maxPending {
		l.mu.Unlock()
		writeAuthJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many sign-ins waiting; try again in a minute"})
		return
	}
	l.codes[digest(code)] = &authCode{challenge: req.CodeChallenge, redirect: req.RedirectURI,
		username: who, expires: now.Add(AuthCodeTTL)}
	l.mu.Unlock()
	q.Set("code", code)
	writeAuthJSON(w, http.StatusOK, map[string]string{"redirect": req.RedirectURI + "?" + q.Encode()})
}

// exchange redeems a code once, with the verifier whose S256 is its challenge
// and the redirect it was issued for.
func (l *CLILogin) exchange(code, verifier, redirect string) (username, kind, desc string) {
	if code == "" || verifier == "" {
		return "", errRequest, "code and code_verifier are required"
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		return "", errGrant, "the code_verifier must be 43 to 128 characters"
	}
	key := digest(code)
	now := l.now()
	l.mu.Lock()
	c, ok := l.codes[key]
	// Single use: gone whether or not what follows succeeds.
	delete(l.codes, key)
	l.mu.Unlock()
	if !ok || now.After(c.expires) {
		return "", errGrant, "the code has expired or was already used"
	}
	sum := sha256.Sum256([]byte(verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(c.challenge)) != 1 {
		return "", errGrant, "the code_verifier does not match"
	}
	if redirect != c.redirect {
		return "", errGrant, "redirect_uri does not match"
	}
	return c.username, "", ""
}

// revoke ends the bearer token the request carries.
func (l *CLILogin) revoke(w http.ResponseWriter, r *http.Request) {
	tok := bearerToken(r)
	if tok == "" {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "send the token to end as the bearer"})
		return
	}
	if err := l.Tokens.Revoke(tok); err != nil {
		writeAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not record the sign-out"})
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]bool{"signed_out": true})
}
