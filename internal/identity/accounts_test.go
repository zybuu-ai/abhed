package identity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/zybuu-ai/abhed/internal/home"
)

func init() { SetHashCost(bcrypt.MinCost) }

// testStore is a store under a fresh home.
func testStore(t *testing.T) *Store {
	t.Helper()
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv(home.EnvPerson, "")
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const pw = "correct horse"

func TestCreateMakesAPrivateAccount(t *testing.T) {
	s := testStore(t)
	e, code, err := s.Create("Yuvraj", pw, "y@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Username != "yuvraj" || !validID(e.ID) || code == "" {
		t.Fatalf("entry %+v code %q", e, code)
	}
	for _, p := range []string{s.file(), s.Dir(), filepath.Join(s.Dir(), e.ID)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is mode %o", p, fi.Mode().Perm())
		}
	}
	b, _ := os.ReadFile(s.file())
	if string(b) == "" || containsAny(string(b), pw, code) {
		t.Error("the accounts file holds the password or the recovery code in clear")
	}
	if err := home.Check(filepath.Join(s.Dir(), e.ID)); err != nil {
		t.Errorf("the account's folder is not one home accepts: %v", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if x != "" && strings.Contains(s, x) {
			return true
		}
	}
	return false
}

func TestCreateRefuses(t *testing.T) {
	s := testStore(t)
	if _, _, err := s.Create("a", "short", "", ""); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("short password: %v", err)
	}
	if _, _, err := s.Create("a", "password123", "", ""); !errors.Is(err, ErrCommon) {
		t.Errorf("common password: %v", err)
	}
	for _, u := range []string{"", "-a", "a b", "a/b", "../x", "UPPER:colon"} {
		if _, _, err := s.Create(u, pw, "", ""); !errors.Is(err, ErrBadUsername) {
			t.Errorf("username %q: %v", u, err)
		}
	}
}

// Two processes creating the same account: exactly one wins.
func TestCreateRaceOneWins(t *testing.T) {
	s := testStore(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, _ := OpenAt(s.Dir())
			_, _, errs[i] = other.Create("same", pw, "", "")
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrAccountExists):
			t.Errorf("unexpected: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d creations succeeded", won)
	}
	if list, _ := s.List(); len(list) != 1 {
		t.Fatalf("%d accounts", len(list))
	}
}

func TestWrongPasswordsBackOff(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if _, _, err := s.Create("a", pw, "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < freeTries; i++ {
		if _, err := s.Authenticate("a", "wrong one!!"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	var locked *LockedError
	if _, err := s.Authenticate("a", pw); !errors.As(err, &locked) {
		t.Fatalf("right password while locked: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := s.Authenticate("a", pw); err != nil {
		t.Fatalf("after the wait: %v", err)
	}
	e, _ := s.Get("a")
	if e.Failed != 0 {
		t.Errorf("failures not cleared: %d", e.Failed)
	}
	if lockFor(100) != maxLock {
		t.Errorf("the wait is not capped: %s", lockFor(100))
	}
	if _, err := s.Authenticate("nobody", pw); !errors.Is(err, ErrBadPassword) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestRememberedUntilSignOutOrRevocation(t *testing.T) {
	s := testStore(t)
	e, _, err := s.Create("a", pw, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Fatal("remembered before any sign-in")
	}
	if err := s.Remember(e.ID); err != nil {
		t.Fatal(err)
	}
	// No idle expiry: a year later it still holds.
	later := time.Now().Add(365 * 24 * time.Hour)
	s.now = func() time.Time { return later }
	if got, ok, err := s.Remembered(nil); err != nil || !ok || got.ID != e.ID {
		t.Fatalf("remembered = %v %v %v", got.ID, ok, err)
	}
	days := 30
	if _, ok, _ := s.Remembered(&days); ok {
		t.Error("remember_days 30 did not bound it")
	}
	zero := 0
	if _, ok, _ := s.Remembered(&zero); ok {
		t.Error("remember_days 0 did not ask")
	}
	s.now = time.Now
	if err := s.ChangePassword("a", pw, "another horse"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("a password change left the remembered sign-in")
	}
	if err := s.Remember(e.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SignOutEverywhere(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("sign out everywhere left it")
	}
	if err := s.Remember(e.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("sign-out left it")
	}
	// A token copied into the file by hand is not the one issued.
	if err := s.Remember(e.ID); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(s.rememberedFile(), remembered{ID: e.ID, Token: "forged", IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("a forged token was accepted")
	}
}

func TestResets(t *testing.T) {
	s := testStore(t)
	e, code, err := s.Create("a", pw, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remember(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetWithCode("a", "AAAA-BBBB", "new password 1"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("wrong code: %v", err)
	}
	fresh, err := s.ResetWithCode("a", " "+code+" ", "new password 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("a", "new password 1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("a reset left the remembered sign-in")
	}
	if _, err := s.ResetWithCode("a", code, "new password 2"); !errors.Is(err, ErrBadCode) {
		t.Errorf("a used code worked again: %v", err)
	}
	if _, err := s.ResetWithCode("a", fresh, "new password 2"); err != nil {
		t.Errorf("the new code: %v", err)
	}
	if err := s.ResetAsOSUser("a", "new password 3", "yuvraj", "mac"); err != nil {
		t.Fatal(err)
	}
	r, err := s.TakeReset(e.ID)
	if err != nil || r == nil || r.By != "os-user" || r.OSUser != "yuvraj" {
		t.Fatalf("reset note = %+v, %v", r, err)
	}
	if r, _ := s.TakeReset(e.ID); r != nil {
		t.Error("the reset note was shown twice")
	}
}

func TestStoreRefusesLinksAndOpenFiles(t *testing.T) {
	s := testStore(t)
	if _, _, err := s.Create("a", pw, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.file(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Error("an accounts file open to others was read")
	}
	_ = os.Chmod(s.file(), 0o600)
	if err := os.Chmod(s.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAt(s.Dir()); err == nil {
		t.Error("an accounts folder open to others was opened")
	}
	_ = os.Chmod(s.Dir(), 0o700)
	target := s.file()
	link := filepath.Join(t.TempDir(), "accounts")
	if err := os.Mkdir(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(link, "accounts.json")); err != nil {
		t.Fatal(err)
	}
	other, err := OpenAt(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.List(); err == nil {
		t.Error("an accounts file that is a link was read")
	}
}

// A store's cost is its own: one test's fast store never changes the cost
// another store, or the process, hashes at.
func TestWithCostIsPerStore(t *testing.T) {
	s := testStore(t)
	fast := s.WithCost(bcrypt.MinCost + 1)
	e, _, err := fast.Create("fast", pw, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := bcrypt.Cost([]byte(e.Hash)); c != bcrypt.MinCost+1 {
		t.Fatalf("WithCost store hashed at %d", c)
	}
	e, _, err = s.Create("plain", pw, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := bcrypt.Cost([]byte(e.Hash)); c != bcrypt.MinCost {
		t.Fatalf("the original store hashed at %d, not the process cost", c)
	}
	if _, err := s.Authenticate("fast", pw); err != nil {
		t.Fatal(err)
	}
}
