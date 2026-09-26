package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Local accounts: username and password held by Abhed itself.
//
// OIDC is the right answer for an organisation that already has an identity
// provider, and it stays the recommended mode. But it delegates, and a
// deployment with no IdP — a pilot, an air-gapped enclave, a single team
// standing this up before central IT is involved — was left with no way to
// have users at all. That is a real gap, not a configuration preference.
//
// Passwords are bcrypt-hashed. The cost is deliberately left at the library
// default rather than tuned down: sign-in happens once per session, so a few
// hundred milliseconds is invisible to a person and expensive to an attacker
// with the hash file.

var (
	ErrBadCredentials = errors.New("incorrect username or password")
	ErrUserExists     = errors.New("that username is already taken")
	ErrWeakPassword   = errors.New("password must be at least 10 characters")
	ErrNoSuchUser     = errors.New("no such user")
)

// User is a local account.
type User struct {
	Username  string    `json:"username"`
	Email     string    `json:"email,omitempty"`
	Name      string    `json:"name,omitempty"`
	Tenant    string    `json:"tenant"`
	Groups    []string  `json:"groups,omitempty"`
	Hash      string    `json:"-"` // never serialized
	CreatedAt time.Time `json:"created_at"`
	// MustChange forces a password change at next sign-in, used for the
	// bootstrap admin so a generated password cannot become permanent.
	MustChange bool `json:"must_change_password,omitempty"`
}

// UserStore persists local accounts.
type UserStore interface {
	Get(ctx context.Context, username string) (*User, error)
	Put(ctx context.Context, u *User) error
	List(ctx context.Context) ([]*User, error)
	Delete(ctx context.Context, username string) error
}

// VersionedUserStore is a UserStore that can say cheaply whether any account
// changed, so a live session re-reads its account only when one did.
type VersionedUserStore interface {
	Version() (string, error)
}

// MemoryUserStore keeps accounts in memory. Adequate for a single-process
// pilot; a durable deployment should use the Postgres store so accounts
// survive a restart.
type MemoryUserStore struct {
	mu    sync.RWMutex
	users map[string]*User
	gen   atomic.Uint64
}

func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{users: map[string]*User{}}
}

func (m *MemoryUserStore) Get(_ context.Context, username string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, found := m.users[strings.ToLower(username)]
	if !found {
		return nil, ErrNoSuchUser
	}
	copy := *u
	return &copy, nil
}

func (m *MemoryUserStore) Put(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copy := *u
	m.users[strings.ToLower(u.Username)] = &copy
	m.gen.Add(1)
	return nil
}

func (m *MemoryUserStore) List(_ context.Context) ([]*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*User, 0, len(m.users))
	for _, u := range m.users {
		copy := *u
		out = append(out, &copy)
	}
	return out, nil
}

func (m *MemoryUserStore) Delete(_ context.Context, username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := strings.ToLower(username)
	if _, found := m.users[key]; !found {
		return ErrNoSuchUser
	}
	delete(m.users, key)
	m.gen.Add(1)
	return nil
}

// Version changes with every write.
func (m *MemoryUserStore) Version() (string, error) {
	return strconv.FormatUint(m.gen.Load(), 10), nil
}

// LocalAuth handles username/password sign-in.
type LocalAuth struct {
	Store      UserStore
	CookieName string
	SessionTTL time.Duration
	Secure     bool
	// Admit, when set, is asked after the password checks out and before a
	// session is issued; its error is shown to the person with a 403. Local
	// accounts only: other providers meet Middleware.Check on their first request.
	Admit func(ctx context.Context, u *User) error

	mu       sync.RWMutex
	sessions map[string]*browserSession
}

func NewLocalAuth(store UserStore, ttl time.Duration, secure bool) *LocalAuth {
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	l := &LocalAuth{
		Store: store, CookieName: "abhed_session",
		SessionTTL: ttl, Secure: secure,
		sessions: map[string]*browserSession{},
	}
	go l.reap()
	return l
}

func (l *LocalAuth) reap() {
	for range time.Tick(10 * time.Minute) {
		now := time.Now()
		l.mu.Lock()
		for k, s := range l.sessions {
			if now.After(s.Expires) {
				delete(l.sessions, k)
			}
		}
		l.mu.Unlock()
	}
}

var validUsername = regexp.MustCompile(`^[a-zA-Z0-9._-]{2,64}$`)

// CheckNewUser reports why CreateUser would refuse this username and
// password, without creating anything.
func (l *LocalAuth) CheckNewUser(ctx context.Context, username, password string) error {
	if !validUsername.MatchString(username) {
		return fmt.Errorf("username must be 2-64 characters of letters, digits, dot, dash or underscore")
	}
	// Ten characters is a deliberate floor: short enough that people will not
	// write it down, long enough that bcrypt's cost actually matters.
	if len(password) < 10 {
		return ErrWeakPassword
	}
	if existing, _ := l.Store.Get(ctx, username); existing != nil {
		return ErrUserExists
	}
	return nil
}

// CreateUser adds an account.
func (l *LocalAuth) CreateUser(ctx context.Context, u User, password string) error {
	if err := l.CheckNewUser(ctx, u.Username, password); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	u.Hash = string(hash)
	u.Username = strings.ToLower(u.Username)
	if u.Tenant == "" {
		u.Tenant = "default"
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	return l.Store.Put(ctx, &u)
}

// Authenticate verifies credentials.
func (l *LocalAuth) Authenticate(ctx context.Context, username, password string) (*User, error) {
	u, err := l.Store.Get(ctx, username)
	if err != nil {
		// Hash anyway so a missing user takes the same time as a wrong
		// password: otherwise response timing enumerates valid usernames.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"),
			[]byte(password))
		return nil, ErrBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password)); err != nil {
		return nil, ErrBadCredentials
	}
	return u, nil
}

// ChangePassword updates a user's password after verifying the current one.
func (l *LocalAuth) ChangePassword(ctx context.Context, username, current, next string) error {
	u, err := l.Authenticate(ctx, username, current)
	if err != nil {
		return err
	}
	if len(next) < 10 {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Hash = string(hash)
	u.MustChange = false
	if err := l.Store.Put(ctx, u); err != nil {
		return err
	}
	l.markMustChange(u.Username, false)
	return nil
}

// markMustChange sets the password-change flag on every live session of a user.
func (l *LocalAuth) markMustChange(username string, on bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.sessions {
		if s.Identity != nil && strings.EqualFold(s.Identity.Subject, username) {
			s.mustChange.Store(on)
		}
	}
}

// issue creates a browser session for a signed-in user.
func (l *LocalAuth) issue(w http.ResponseWriter, u *User) {
	sid := randomToken()
	now := time.Now()
	s := &browserSession{
		Identity: &Identity{
			Subject: u.Username, Email: u.Email, Name: u.Name,
			Tenant: u.Tenant, Groups: u.Groups,
			IssuedAt: now.Unix(), Expires: now.Add(l.SessionTTL).Unix(),
		},
		Created: now,
		Expires: now.Add(l.SessionTTL),
	}
	s.mustChange.Store(u.MustChange)
	s.lastSeen.Store(now.UnixNano())
	l.mu.Lock()
	l.sessions[sid] = s
	l.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: l.CookieName, Value: sid, Path: "/",
		HttpOnly: true, Secure: l.Secure, SameSite: http.SameSiteLaxMode,
		Expires: time.Now().Add(l.SessionTTL),
	})
}

// FromCookie resolves a session cookie to an identity, as the account stands
// now rather than as it stood at sign-in.
func (l *LocalAuth) FromCookie(r *http.Request) (*Identity, bool) {
	sid, s, ok := l.lookup(r)
	if !ok {
		return nil, false
	}
	id, err := l.current(r.Context(), sid, s)
	if err != nil {
		return nil, false
	}
	s.lastSeen.Store(time.Now().UnixNano())
	return id, true
}

// ErrAccountUnchecked is Verify's answer when the account store cannot be read.
var ErrAccountUnchecked = errors.New("could not check your sign-in; try again shortly")

// errSessionGone is current's answer for a session that has ended.
var errSessionGone = errors.New("session ended")

// Verify re-reads the account behind the request's session, if it names one.
// It returns ErrAccountUnchecked when the store cannot answer, so a caller can
// tell an outage from a sign-in that ended; any other outcome is nil.
func (l *LocalAuth) Verify(r *http.Request) error {
	sid, s, ok := l.lookup(r)
	if !ok {
		return nil
	}
	if _, err := l.current(r.Context(), sid, s); errors.Is(err, ErrAccountUnchecked) {
		return err
	}
	return nil
}

// HasSession reports whether the request's cookie names a live session,
// without reading its account.
func (l *LocalAuth) HasSession(r *http.Request) bool {
	_, _, ok := l.lookup(r)
	return ok
}

// How long a session trusts its last reading of its account; a versioned store
// is re-read as soon as it changes, and after the longer bound regardless.
const (
	accountRecheck          = 2 * time.Second
	accountRecheckVersioned = 30 * time.Second
)

// accountCheck is when a session last read its account, and the store
// version it read.
type accountCheck struct {
	version string
	at      time.Time
}

// current re-reads a session's account when it may have changed, so a change
// made anywhere applies on the next request; a removed account ends the session.
func (l *LocalAuth) current(ctx context.Context, sid string, s *browserSession) (*Identity, error) {
	l.mu.RLock()
	old := s.Identity
	l.mu.RUnlock()
	if old == nil {
		return nil, errSessionGone
	}
	if l.Store == nil {
		return old, nil
	}
	now := time.Now()
	version, maxAge := "", accountRecheck
	if v, ok := l.Store.(VersionedUserStore); ok {
		if got, err := v.Version(); err == nil {
			version, maxAge = got, accountRecheckVersioned
		}
	}
	if c := s.checked.Load(); c != nil && c.version == version && now.Sub(c.at) < maxAge {
		return old, nil
	}
	epoch := s.epoch.Load()
	u, err := l.Store.Get(ctx, old.Subject)
	if errors.Is(err, ErrNoSuchUser) || (err == nil && u == nil) {
		l.mu.Lock()
		delete(l.sessions, sid)
		l.mu.Unlock()
		return nil, errSessionGone
	}
	if err != nil {
		// Refused, not ended: a store that cannot answer has not removed anyone.
		return nil, ErrAccountUnchecked
	}
	id := &Identity{
		Subject: u.Username, Email: u.Email, Name: u.Name,
		Tenant: u.Tenant, Groups: slices.Clone(u.Groups),
		IssuedAt: old.IssuedAt, Expires: old.Expires,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, live := l.sessions[sid]; !live {
		return nil, errSessionGone
	}
	s.Identity = id
	s.mustChange.Store(u.MustChange)
	// Not trusted if forget ran during the read: that read may predate the change.
	if s.epoch.Load() == epoch {
		s.checked.Store(&accountCheck{version: version, at: now})
	}
	return id, nil
}

// session finds the live session a request's cookie names.
func (l *LocalAuth) session(r *http.Request) (*browserSession, bool) {
	_, s, ok := l.lookup(r)
	return s, ok
}

// lookup finds the live session a request's cookie names, and its key.
func (l *LocalAuth) lookup(r *http.Request) (string, *browserSession, bool) {
	c, err := r.Cookie(l.CookieName)
	if err != nil {
		return "", nil, false
	}
	l.mu.RLock()
	s, found := l.sessions[c.Value]
	l.mu.RUnlock()
	if !found || time.Now().After(s.Expires) {
		return "", nil, false
	}
	return c.Value, s, true
}

// forget makes every live session of username re-read its account on its
// next request, for a change made through this process.
func (l *LocalAuth) forget(username string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.sessions {
		if s.Identity != nil && strings.EqualFold(s.Identity.Subject, username) {
			s.epoch.Add(1)
			s.checked.Store(nil)
		}
	}
}

// MustChangePassword reports whether the request's session belongs to an
// account whose password was set for it and has not yet been changed.
func (l *LocalAuth) MustChangePassword(r *http.Request) bool {
	s, ok := l.session(r)
	return ok && s.mustChange.Load()
}

// Sessions lists the live local sessions, newest first, without their cookies.
func (l *LocalAuth) Sessions() []SessionInfo {
	now := time.Now()
	l.mu.RLock()
	out := make([]SessionInfo, 0, len(l.sessions))
	for sid, s := range l.sessions {
		if now.After(s.Expires) || s.Identity == nil {
			continue
		}
		out = append(out, SessionInfo{
			ID: sessionDigest(sid), Provider: l.Name(),
			Subject: s.Identity.Subject, Email: s.Identity.Email, Name: s.Identity.Name,
			Created: s.Created, LastSeen: time.Unix(0, s.lastSeen.Load()),
			Expires: s.Expires, MustChange: s.mustChange.Load(),
		})
	}
	l.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// EndSession ends the session whose SessionInfo.ID is id, reporting whether
// one was found.
func (l *LocalAuth) EndSession(id string) bool {
	if id == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for sid := range l.sessions {
		if sessionDigest(sid) == id {
			delete(l.sessions, sid)
			return true
		}
	}
	return false
}

// EndRequestSession ends the session the request carries and expires its
// cookie, without answering the request.
func (l *LocalAuth) EndRequestSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(l.CookieName); err == nil {
		l.mu.Lock()
		delete(l.sessions, c.Value)
		l.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: l.CookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: l.Secure, MaxAge: -1,
		SameSite: http.SameSiteLaxMode,
	})
}

type signInRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// SignInHandler handles a credential POST from the sign-in form.
func (l *LocalAuth) SignInHandler(w http.ResponseWriter, r *http.Request) {
	var req signInRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	u, err := l.Authenticate(r.Context(), req.Username, req.Password)
	if err != nil {
		// One message for both wrong-user and wrong-password: distinguishing
		// them tells an attacker which usernames exist.
		writeAuthJSON(w, http.StatusUnauthorized,
			map[string]string{"error": ErrBadCredentials.Error()})
		return
	}
	// Asked only after the password checks out, so a refusal says nothing
	// about accounts to someone who does not hold one.
	if l.Admit != nil {
		if err := l.Admit(r.Context(), u); err != nil {
			writeAuthJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
	}

	l.issue(w, u)
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"ok": true, "username": u.Username, "tenant": u.Tenant,
		"must_change_password": u.MustChange,
	})
}

// SignOut clears the session.
func (l *LocalAuth) SignOut(w http.ResponseWriter, r *http.Request) {
	l.EndRequestSession(w, r)
	http.Redirect(w, r, "/", http.StatusFound)
}

type changePasswordRequest struct {
	Current string `json:"current_password"`
	Next    string `json:"new_password"`
}

// ChangePasswordHandler lets a signed-in user rotate their own password.
func (l *LocalAuth) ChangePasswordHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := l.FromCookie(r)
	if !ok {
		writeAuthJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if err := l.ChangePassword(r.Context(), id.Subject, req.Current, req.Next); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Whoami reports the signed-in user.
//
// Deprecated: the server answers /v1/whoami for every provider and never
// registers this; it will be removed in 2.0.
func (l *LocalAuth) Whoami(w http.ResponseWriter, r *http.Request) {
	id, ok := l.FromCookie(r)
	if !ok {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"authenticated": false, "auth_mode": "local"})
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "auth_mode": "local",
		"subject": id.Subject, "email": id.Email, "name": id.Name,
		"tenant": id.Tenant, "groups": id.Groups,
		"switch_url": "/logout", "password_url": "/account", "sign_out_url": "/logout",
		"must_change_password": l.MustChangePassword(r),
	})
}

func writeAuthJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// CreateUserOrReset sets a password without knowing the current one. This is
// the administrative reset path — a user who has forgotten their password
// cannot use ChangePassword, which requires it.
func (l *LocalAuth) CreateUserOrReset(ctx context.Context, u *User, password string) error {
	if len(password) < 10 {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Hash = string(hash)
	// An administratively reset password is temporary by definition.
	u.MustChange = true
	if err := l.Store.Put(ctx, u); err != nil {
		return err
	}
	l.markMustChange(u.Username, true)
	return nil
}

// ListUsers returns every local account, for an administrator's user list.
//
// Hashes are never included: User.Hash is json:"-" and the store's own wrapper
// type exists to keep it that way, so a handler that marshals this cannot leak
// one by accident.
func (l *LocalAuth) ListUsers(ctx context.Context) ([]*User, error) {
	return l.Store.List(ctx)
}

// SetGroups adds or removes one group on an account.
//
// Scoped to a single named group rather than replacing the whole list: an
// admin toggle that overwrote Groups would silently discard whatever else an
// OIDC deployment or an operator had put there.
//
// Live sessions pick the change up on their next request (see current); a
// caller removing rights should also end them with RevokeUser.
func (l *LocalAuth) SetGroups(ctx context.Context, username, group string, member bool) error {
	u, err := l.Store.Get(ctx, username)
	if err != nil || u == nil {
		return ErrNoSuchUser
	}
	has := false
	out := make([]string, 0, len(u.Groups)+1)
	for _, g := range u.Groups {
		if g == group {
			has = true
			if !member {
				continue // dropping it
			}
		}
		out = append(out, g)
	}
	if member && !has {
		out = append(out, group)
	}
	u.Groups = out
	if err := l.Store.Put(ctx, u); err != nil {
		return err
	}
	l.forget(u.Username)
	return nil
}

// RevokeUser drops every live session belonging to a username.
//
// Revocation that leaves an existing session working is not revocation: the
// person keeps their agent, their shell and their transcript until the cookie
// happens to expire. Disabling the account stops the next SIGN-IN; this stops
// the current one, and both are needed.
//
// Returns how many sessions were ended, which the audit entry records — "we
// revoked them and they had three sessions open" is a materially different
// fact from "they were not signed in".
func (l *LocalAuth) RevokeUser(username string) int {
	if username == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for sid, s := range l.sessions {
		if s.Identity != nil && s.Identity.Subject == username {
			delete(l.sessions, sid)
			n++
		}
	}
	return n
}

// LocalAuth satisfies Provider. Its form lives on the front door rather than
// on a page of its own, so SignIn names no entry point: the middleware sends a
// browser to "/" and the server renders the form there.

func (l *LocalAuth) Name() string { return "local" }

func (l *LocalAuth) Identify(r *http.Request) (*Identity, bool) { return l.FromCookie(r) }

func (l *LocalAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/signin", l.SignInHandler)
	mux.HandleFunc("POST /v1/password", l.ChangePasswordHandler)
}

// PublicPaths names sign-in and sign-up. Sign-up is the server's handler, not
// this type's, but it exists only because these accounts do, and it has to be
// reachable without being signed in — that is what registering means. The
// handler decides admission; gating the path instead made invite-only
// registration unreachable, because a valid code got a 401 from the
// middleware before the handler could read it.
func (l *LocalAuth) PublicPaths() []string { return []string{"/v1/signin", "/v1/signup"} }

func (l *LocalAuth) SignIn() (url, label string) { return "", "" }
