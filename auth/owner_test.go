package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   *Identity
		want string
	}{
		{"no identity", nil, "anonymous"},
		{"anonymous", &Identity{Subject: "anonymous"}, "anonymous"},
		{"local ignores its email", &Identity{Provider: ProviderLocal, Subject: "Mallet", Email: "bob"}, "local:mallet"},
		{"local ignores a real email", &Identity{Provider: ProviderLocal, Subject: "frank", Email: "carol@example.test"}, "local:frank"},
		{"local even when marked verified", &Identity{Provider: ProviderLocal, Subject: "frank", Email: "carol@example.test", EmailVerified: true}, "local:frank"},
		{"oidc verified email", &Identity{Provider: "oidc", Subject: "u1", Email: "carol@example.test", EmailVerified: true}, "carol@example.test"},
		{"oidc unverified email", &Identity{Provider: "oidc", Subject: "u1", Email: "carol@example.test"}, "oidc:u1"},
		{"oidc verified non-address", &Identity{Provider: "oidc", Subject: "u1", Email: "local:bob", EmailVerified: true}, "oidc:u1"},
		{"oidc no email", &Identity{Provider: "oidc", Subject: "u1"}, "oidc:u1"},
		{"github", &Identity{Provider: "github", Subject: "42", Email: "g@example.test", EmailVerified: true}, "g@example.test"},
		{"proxy email", &Identity{Provider: ProviderProxy, Subject: "p", Email: "p@example.test", EmailVerified: true}, "p@example.test"},
		{"proxy no email", &Identity{Provider: ProviderProxy, Subject: "p"}, "p"},
		{"unnamed provider never uses email", &Identity{Subject: "s", Email: "bob", EmailVerified: true}, "s"},
	} {
		if got := tc.id.Owner(); got != tc.want {
			t.Errorf("%s: Owner() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// No other provider's principal can equal a local account's.
func TestLocalOwnerCannotCollide(t *testing.T) {
	local := (&Identity{Provider: ProviderLocal, Subject: "bob"}).Owner()
	for _, id := range []*Identity{
		{Provider: "oidc", Subject: "bob"},
		{Provider: "oidc", Subject: "x", Email: "local:bob", EmailVerified: true},
	} {
		if id.Owner() == local {
			t.Errorf("%+v owns %q", id, local)
		}
	}
}

func TestCreateUserChecksTheEmail(t *testing.T) {
	ctx := context.Background()
	l := NewLocalAuth(NewMemoryUserStore(), time.Hour, false)
	if err := l.CreateUser(ctx, User{Username: "bob"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.CreateUser(ctx, User{Username: "carol", Email: "carol@example.test"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		email string
		want  error
	}{
		{"bob", ErrBadEmail},
		{"BOB", ErrBadEmail},
		{"not an address", ErrBadEmail},
		{"Carol <c2@example.test>", ErrBadEmail},
		{"a@b@example.test", ErrBadEmail},
		{"carol@example.test", ErrEmailTaken},
		{"CAROL@Example.Test", ErrEmailTaken},
		{" carol@example.test ", ErrEmailTaken},
	} {
		err := l.CreateUser(ctx, User{Username: "eve", Email: tc.email}, "correct-horse-1")
		if !errors.Is(err, tc.want) {
			t.Errorf("email %q: %v, want %v", tc.email, err, tc.want)
		}
	}
	if err := l.CreateUser(ctx, User{Username: "eve", Email: " eve@example.test "}, "correct-horse-1"); err != nil {
		t.Fatalf("a fresh address: %v", err)
	}
	if u, _ := l.Store.Get(ctx, "eve"); u == nil || u.Email != "eve@example.test" {
		t.Fatalf("stored %+v, want the trimmed address", u)
	}
	// An email that is another account's username is refused too.
	if err := l.CheckEmail(ctx, "dan", "Bob"); err == nil {
		t.Fatal("an email equal to another account's username was accepted")
	}
}
