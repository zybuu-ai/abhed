package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTokens(t *testing.T) (*LocalAuth, *CLITokens, string) {
	t.Helper()
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")
	key := filepath.Join(t.TempDir(), "state", "cli-token.key")
	tok, err := OpenCLITokens(l, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return l, tok, key
}

func TestCLITokenRoundTripAndKeyFile(t *testing.T) {
	_, tok, key := newTokens(t)
	ctx := context.Background()
	s, exp, err := tok.Issue(ctx, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, CLITokenPrefix) || time.Until(exp) > time.Hour+time.Minute {
		t.Fatalf("token %q.. expires %s", s[:10], exp)
	}
	id, err := tok.Verify(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "ada" || id.Provider != ProviderLocal || id.Owner() != "local:ada" {
		t.Errorf("identity %+v owner %s", id, id.Owner())
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(key)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %o, want 600", fi.Mode().Perm())
		}
	}
	// The same key file signs for a second process.
	again, err := OpenCLITokens(tok.Local, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.Verify(ctx, s); err != nil {
		t.Errorf("a token from the same key was refused after reopening: %v", err)
	}
	// Another key does not accept it.
	other, _ := OpenCLITokens(tok.Local, filepath.Join(t.TempDir(), "k"), time.Hour)
	if _, err := other.Verify(ctx, s); !errors.Is(err, ErrCLITokenBad) {
		t.Errorf("another key accepted the token: %v", err)
	}
}

func TestCLITokenTamperingAndExpiry(t *testing.T) {
	_, tok, _ := newTokens(t)
	ctx := context.Background()
	s, _, _ := tok.Issue(ctx, "ada")
	payload, sig, _ := strings.Cut(strings.TrimPrefix(s, CLITokenPrefix), ".")
	body, _ := base64.RawURLEncoding.DecodeString(payload)
	forged := strings.Replace(string(body), `"sub":"ada"`, `"sub":"bob"`, 1)
	for name, bad := range map[string]string{
		"other subject": CLITokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + sig,
		"no signature":  CLITokenPrefix + payload,
		"bad signature": CLITokenPrefix + payload + "." + strings.Repeat("A", len(sig)),
		"empty":         CLITokenPrefix,
	} {
		if _, err := tok.Verify(ctx, bad); !errors.Is(err, ErrCLITokenBad) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := tok.Verify(ctx, "eyJhbGciOi.not.ours"); !errors.Is(err, ErrNotCLIToken) {
		t.Errorf("a foreign token: %v", err)
	}
	tok.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := tok.Verify(ctx, s); !errors.Is(err, ErrCLITokenExpiry) {
		t.Errorf("an expired token: %v", err)
	}
}

func TestCLITokenEndsWithTheAccount(t *testing.T) {
	ctx := context.Background()
	t.Run("sign out everywhere", func(t *testing.T) {
		l, tok, _ := newTokens(t)
		s, _, _ := tok.Issue(ctx, "ada")
		if _, err := l.RevokeUserContext(ctx, "ada"); err != nil {
			t.Fatal(err)
		}
		if _, err := tok.Verify(ctx, s); !errors.Is(err, ErrCLITokenEnded) {
			t.Errorf("after sign-out everywhere: %v", err)
		}
		fresh, _, err := tok.Issue(ctx, "ada")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tok.Verify(ctx, fresh); err != nil {
			t.Errorf("a token issued after the sign-out was refused: %v", err)
		}
	})
	t.Run("password change", func(t *testing.T) {
		l, tok, _ := newTokens(t)
		s, _, _ := tok.Issue(ctx, "ada")
		if err := l.ChangePassword(ctx, "ada", "correct-horse-battery", "another-long-password"); err != nil {
			t.Fatal(err)
		}
		if _, err := tok.Verify(ctx, s); !errors.Is(err, ErrCLITokenEnded) {
			t.Errorf("after a password change: %v", err)
		}
	})
	t.Run("removed", func(t *testing.T) {
		l, tok, _ := newTokens(t)
		s, _, _ := tok.Issue(ctx, "ada")
		if _, err := RemoveUser(ctx, l.Store, "ada"); err != nil {
			t.Fatal(err)
		}
		if _, err := tok.Verify(ctx, s); !errors.Is(err, ErrCLITokenEnded) {
			t.Errorf("after removal: %v", err)
		}
	})
	t.Run("admin reset and admission", func(t *testing.T) {
		l, tok, _ := newTokens(t)
		s, _, _ := tok.Issue(ctx, "ada")
		u, _ := l.Store.Get(ctx, "ada")
		u.MustChange = true
		_ = l.Store.Put(ctx, u)
		if _, err := tok.Verify(ctx, s); err == nil {
			t.Error("a token outlived an administrator's password reset")
		}
		if _, _, err := tok.Issue(ctx, "ada"); err == nil {
			t.Error("a token was issued for a password that must be changed")
		}
		u.MustChange = false
		_ = l.Store.Put(ctx, u)
		l.Admit = func(context.Context, *User) error { return errors.New("no seat") }
		if _, err := tok.Verify(ctx, s); err == nil || err.Error() != "no seat" {
			t.Errorf("Admit was not asked on use: %v", err)
		}
		if _, _, err := tok.Issue(ctx, "ada"); err == nil {
			t.Error("Admit was not asked at issue")
		}
	})
}

func TestCLITokenRevokeIsRememberedAcrossRestarts(t *testing.T) {
	_, tok, key := newTokens(t)
	ctx := context.Background()
	a, _, _ := tok.Issue(ctx, "ada")
	b, _, _ := tok.Issue(ctx, "ada")
	if err := tok.Revoke(a); err != nil {
		t.Fatal(err)
	}
	if err := tok.Revoke("garbage"); err != nil {
		t.Errorf("revoking a token that does not verify: %v", err)
	}
	if _, err := tok.Verify(ctx, a); !errors.Is(err, ErrCLITokenEnded) {
		t.Errorf("a revoked token: %v", err)
	}
	if _, err := tok.Verify(ctx, b); err != nil {
		t.Errorf("revoking one token ended another: %v", err)
	}
	restarted, err := OpenCLITokens(tok.Local, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Verify(ctx, a); !errors.Is(err, ErrCLITokenEnded) {
		t.Errorf("a revoked token came back after a restart: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(key + ".revoked")
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("revoked list mode %o", fi.Mode().Perm())
		}
	}
}

func TestCLITokenKeyFileRefusals(t *testing.T) {
	l := newTestAuth(t)
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	_ = os.WriteFile(short, []byte("abcd\n"), 0o600)
	if _, err := OpenCLITokens(l, short, 0); err == nil {
		t.Error("a short key was accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	open := filepath.Join(dir, "open")
	_ = os.WriteFile(open, []byte(strings.Repeat("ab", 32)+"\n"), 0o644)
	if _, err := OpenCLITokens(l, open, 0); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Errorf("a 0644 key: %v", err)
	}
	link := filepath.Join(dir, "link")
	_ = os.Symlink(short, link)
	if _, err := OpenCLITokens(l, link, 0); err == nil {
		t.Error("a key behind a link was accepted")
	}
	if _, err := OpenCLITokens(nil, filepath.Join(dir, "k"), 0); err == nil {
		t.Error("tokens without local accounts")
	}
}

type fixedVerifier struct {
	id  *Identity
	err error
}

func (f fixedVerifier) Verify(context.Context, string) (*Identity, error) { return f.id, f.err }

func TestVerifierChainTriesCLITokensFirst(t *testing.T) {
	_, tok, _ := newTokens(t)
	ctx := context.Background()
	oidc := fixedVerifier{id: &Identity{Subject: "idp-user", Provider: "oidc"}}
	chain := ChainVerifier(oidc, tok)
	s, _, _ := tok.Issue(ctx, "ada")
	if id, err := chain.Verify(ctx, s); err != nil || id.Subject != "ada" {
		t.Errorf("CLI token through the chain: %v %v", id, err)
	}
	if id, err := chain.Verify(ctx, "eyJ.idp.token"); err != nil || id.Subject != "idp-user" {
		t.Errorf("another token through the chain: %v %v", id, err)
	}
	// A CLI token that fails is final, not passed on.
	if _, err := chain.Verify(ctx, s+"x"); !errors.Is(err, ErrCLITokenBad) {
		t.Errorf("a bad CLI token: %v", err)
	}
	if v := ChainVerifier(nil, tok); v != TokenVerifier(tok) {
		t.Error("a chain of one is not the verifier itself")
	}
	if _, err := ChainVerifier(tok, nil).Verify(ctx, "x"); !errors.Is(err, ErrNotCLIToken) {
		t.Errorf("only CLI tokens: %v", err)
	}
}
