package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// signIn returns a session cookie for username.
func signIn(t *testing.T, l *LocalAuth, username, password string) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	l.SignInHandler(w, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`)))
	for _, c := range w.Result().Cookies() {
		if c.Name == l.CookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("sign-in as %s issued no cookie (status %d)", username, w.Code)
	return nil
}

func withCookie(c *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	return r
}

func changeVia(l *LocalAuth, c *http.Cookie, current, next string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/password",
		strings.NewReader(`{"current_password":"`+current+`","new_password":"`+next+`"}`))
	r.AddCookie(c)
	w := httptest.NewRecorder()
	l.ChangePasswordHandler(w, r)
	return w
}

// Re-entering an administrator's temporary password must not count as
// changing it, or must-change is cleared with the temporary password still set.
func TestChangePasswordRefusesSamePassword(t *testing.T) {
	const temp = "reset-by-the-admin"
	tests := []struct {
		name     string
		next     string
		wantErr  error
		wantMust bool
	}{
		{"same as current", temp, ErrSamePassword, true},
		{"too short", "short", ErrWeakPassword, true},
		{"new password", "chosen-by-the-user", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			l := newTestAuth(t)
			mustCreate(t, l, "ada", "correct-horse-battery")
			u, _ := l.Store.Get(ctx, "ada")
			if err := l.CreateUserOrReset(ctx, u, temp); err != nil {
				t.Fatalf("reset: %v", err)
			}
			c := signIn(t, l, "ada", temp)

			w := changeVia(l, c, temp, tc.next)
			if tc.wantErr == nil && w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
			}
			if tc.wantErr != nil && w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
			if tc.wantErr != nil {
				if !strings.Contains(w.Body.String(), tc.wantErr.Error()) {
					t.Errorf("body %s does not carry %q", w.Body, tc.wantErr)
				}
				if err := l.ChangePassword(ctx, "ada", temp, tc.next); !errors.Is(err, tc.wantErr) {
					t.Errorf("ChangePassword err = %v, want %v", err, tc.wantErr)
				}
			}
			if got := l.MustChangePassword(withCookie(c)); got != tc.wantMust {
				t.Errorf("session must-change = %v, want %v", got, tc.wantMust)
			}
			stored, _ := l.Store.Get(ctx, "ada")
			if stored.MustChange != tc.wantMust {
				t.Errorf("stored must-change = %v, want %v", stored.MustChange, tc.wantMust)
			}
		})
	}
}

// A self-service change ends the user's other sessions, keeps the one that
// made it, and leaves other users alone.
func TestChangePasswordEndsOtherSessions(t *testing.T) {
	tests := []struct {
		name      string
		next      string
		wantOK    bool
		otherLive bool
	}{
		{"changed", "brand-new-password", true, false},
		{"refused as unchanged", "correct-horse-battery", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestAuth(t)
			mustCreate(t, l, "ada", "correct-horse-battery")
			mustCreate(t, l, "bob", "bobs-own-password")
			mine := signIn(t, l, "ada", "correct-horse-battery")
			other := signIn(t, l, "ada", "correct-horse-battery")
			bob := signIn(t, l, "bob", "bobs-own-password")

			w := changeVia(l, mine, "correct-horse-battery", tc.next)
			if (w.Code == http.StatusOK) != tc.wantOK {
				t.Fatalf("status %d, want ok=%v: %s", w.Code, tc.wantOK, w.Body)
			}
			if _, ok := l.FromCookie(withCookie(mine)); !ok {
				t.Error("the session that made the change was ended")
			}
			if _, ok := l.FromCookie(withCookie(other)); ok != tc.otherLive {
				t.Errorf("other session live = %v, want %v", ok, tc.otherLive)
			}
			if _, ok := l.FromCookie(withCookie(bob)); !ok {
				t.Error("another user's session was ended")
			}
		})
	}
}

// A second server sharing the account store ends its sessions too, once it
// reads the account; the changing session on the first server survives.
func TestChangePasswordEndsSessionsOnOtherNodes(t *testing.T) {
	tests := []struct {
		name       string
		next       string
		remoteLive bool
	}{
		{"changed", "brand-new-password", false},
		{"refused as unchanged", "correct-horse-battery", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryUserStore()
			here := NewLocalAuth(store, time.Hour, false)
			there := NewLocalAuth(store, time.Hour, false)
			mustCreate(t, here, "ada", "correct-horse-battery")
			mine := signIn(t, here, "ada", "correct-horse-battery")
			remote := signIn(t, there, "ada", "correct-horse-battery")
			// Read once first, so the change has to displace a cached reading.
			if _, ok := there.FromCookie(withCookie(remote)); !ok {
				t.Fatal("remote session not signed in")
			}

			changeVia(here, mine, "correct-horse-battery", tc.next)
			if _, ok := here.FromCookie(withCookie(mine)); !ok {
				t.Error("the session that made the change was ended")
			}
			if _, ok := there.FromCookie(withCookie(remote)); ok != tc.remoteLive {
				t.Errorf("remote session live = %v, want %v", ok, tc.remoteLive)
			}
			// A fresh sign-in on the other node with the new password works.
			if !tc.remoteLive {
				c := signIn(t, there, "ada", tc.next)
				if _, ok := there.FromCookie(withCookie(c)); !ok {
					t.Error("a sign-in with the new password was ended")
				}
			}
		})
	}
}

// pausingStore holds one armed Get after it has read the account, so a read
// can start before a password change and finish after it.
type pausingStore struct {
	*MemoryUserStore
	mu      sync.Mutex
	armed   bool
	paused  chan struct{}
	release chan struct{}
}

func (p *pausingStore) Get(ctx context.Context, name string) (*User, error) {
	u, err := p.MemoryUserStore.Get(ctx, name)
	p.mu.Lock()
	hold := p.armed
	p.armed = false
	p.mu.Unlock()
	if hold {
		close(p.paused)
		<-p.release
	}
	return u, err
}

// A read of the changing session's own account that straddles the change
// must not end that session, nor stamp it back to the old password.
func TestStaleReadKeepsChangingSession(t *testing.T) {
	tests := []struct {
		name  string
		reset bool
	}{
		{"own password", false},
		{"after an administrator's reset", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := &pausingStore{MemoryUserStore: NewMemoryUserStore(),
				paused: make(chan struct{}), release: make(chan struct{})}
			l := NewLocalAuth(st, time.Hour, false)
			pw := "correct-horse-battery"
			mustCreate(t, l, "ada", pw)
			if tc.reset {
				pw = "reset-by-the-admin"
				u, _ := st.MemoryUserStore.Get(ctx, "ada")
				if err := l.CreateUserOrReset(ctx, u, pw); err != nil {
					t.Fatal(err)
				}
			}
			mine := signIn(t, l, "ada", pw)

			st.mu.Lock()
			st.armed = true
			st.mu.Unlock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				l.FromCookie(withCookie(mine))
			}()
			<-st.paused
			if w := changeVia(l, mine, pw, "brand-new-password"); w.Code != http.StatusOK {
				t.Fatalf("change: %d %s", w.Code, w.Body)
			}
			close(st.release)
			<-done

			for i := range 2 {
				l.forget("ada")
				if _, ok := l.FromCookie(withCookie(mine)); !ok {
					t.Fatalf("read %d: the session that made the change was ended", i)
				}
			}
			if l.MustChangePassword(withCookie(mine)) {
				t.Error("the changing session is still confined")
			}
		})
	}
}

// putHookStore runs onPut before storing, and fails the Put when fail is set.
type putHookStore struct {
	*MemoryUserStore
	onPut func()
	fail  bool
}

func (p *putHookStore) Put(ctx context.Context, u *User) error {
	if p.onPut != nil {
		p.onPut()
	}
	if p.fail {
		return errors.New("database is down")
	}
	return p.MemoryUserStore.Put(ctx, u)
}

// While its change is being stored the session accepts the old hash, and a
// failed store rolls its stamp back so it keeps working.
func TestChangePasswordStampWindowAndRollback(t *testing.T) {
	tests := []struct {
		name   string
		fail   bool
		wantPW string
		wantOK bool
	}{
		{"stored", false, "brand-new-password", true},
		{"store fails", true, "correct-horse-battery", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := &putHookStore{MemoryUserStore: NewMemoryUserStore()}
			l := NewLocalAuth(st, time.Hour, false)
			mustCreate(t, l, "ada", "correct-horse-battery")
			mine := signIn(t, l, "ada", "correct-horse-battery")
			st.fail = tc.fail
			st.onPut = func() {
				l.forget("ada")
				if _, ok := l.FromCookie(withCookie(mine)); !ok {
					t.Error("the changing session was ended while its change was stored")
				}
			}

			w := changeVia(l, mine, "correct-horse-battery", "brand-new-password")
			if (w.Code == http.StatusOK) != tc.wantOK {
				t.Fatalf("status %d, want ok=%v", w.Code, tc.wantOK)
			}
			st.onPut = nil
			l.forget("ada")
			if _, ok := l.FromCookie(withCookie(mine)); !ok {
				t.Error("the changing session was ended after the change")
			}
			if _, err := l.Authenticate(ctx, "ada", tc.wantPW); err != nil {
				t.Errorf("password %q does not work: %v", tc.wantPW, err)
			}
		})
	}
}

// A change submitted twice at once keeps the session that made it: each
// request stamped the session with its own hash, and whichever was stored
// last ended it.
func TestDoubleSubmittedChangeKeepsTheSession(t *testing.T) {
	for range 5 {
		l := newTestAuth(t)
		mustCreate(t, l, "ada", "correct-horse-battery")
		c := signIn(t, l, "ada", "correct-horse-battery")
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = changeVia(l, c, "correct-horse-battery", "a-new-long-password").Code
			}()
		}
		wg.Wait()
		if (codes[0] == http.StatusOK) == (codes[1] == http.StatusOK) {
			t.Fatalf("codes %v: want one change to succeed", codes)
		}
		if _, ok := l.FromCookie(withCookie(c)); !ok {
			t.Fatal("the session that changed the password was ended")
		}
	}
}
