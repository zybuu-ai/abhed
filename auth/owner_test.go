package auth

import (
	"context"
	"errors"
	"net/http/httptest"
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
		{"verified email in any case", &Identity{Provider: "oidc", Subject: "u1", Email: "Carol@Example.TEST", EmailVerified: true}, "carol@example.test"},
		{"proxy email in any case", &Identity{Provider: ProviderProxy, Subject: "p", Email: "P@Example.test", EmailVerified: true}, "p@example.test"},
		{"proxy user that is an address", &Identity{Provider: ProviderProxy, Subject: "Pat@Example.test"}, "pat@example.test"},
		{"proxy user that is a name keeps its case", &Identity{Provider: ProviderProxy, Subject: "Pat"}, "Pat"},
		{"unverified subject keeps its case", &Identity{Provider: "oidc", Subject: "U1@x"}, "oidc:U1@x"},
		{"proxy subject in the local namespace", &Identity{Provider: ProviderProxy, Subject: "local:bob"}, "proxy:local:bob"},
		{"proxy subject in any namespace case", &Identity{Provider: ProviderProxy, Subject: "Unclaimed:bob"}, "proxy:Unclaimed:bob"},
		{"unnamed subject in the oidc namespace", &Identity{Subject: "oidc:u1"}, "subject:oidc:u1"},
		{"unnamed subject in the github namespace", &Identity{Subject: "github:42"}, "subject:github:42"},
		{"proxy email only", &Identity{Provider: ProviderProxy, Email: "Pat@Example.test", EmailVerified: true}, "pat@example.test"},
		{"proxy email only, not an address", &Identity{Provider: ProviderProxy, Email: "pat", EmailVerified: true}, "nobody:proxy"},
		{"proxy user named anonymous", &Identity{Provider: ProviderProxy, Subject: "anonymous"}, "proxy:anonymous"},
		{"proxy user named ANONYMOUS", &Identity{Provider: ProviderProxy, Subject: "ANONYMOUS"}, "proxy:ANONYMOUS"},
		{"proxy user named agent", &Identity{Provider: ProviderProxy, Subject: "agent"}, "proxy:agent"},
		{"proxy user named Agent with an email that cannot own", &Identity{Provider: ProviderProxy, Subject: "Agent", Email: "x", EmailVerified: true}, "proxy:Agent"},
		{"proxy user named anonymous with spaces", &Identity{Provider: ProviderProxy, Subject: " anonymous "}, "proxy: anonymous "},
		{"proxy user named agent with a tab", &Identity{Provider: ProviderProxy, Subject: "agent\t"}, "proxy:agent\t"},
		{"proxy with neither", &Identity{Provider: ProviderProxy}, "nobody:proxy"},
		{"unnamed subject Anonymous", &Identity{Subject: "Anonymous"}, "subject:Anonymous"},
		{"unnamed subject agent", &Identity{Subject: "agent"}, "subject:agent"},
		{"local account named anonymous", &Identity{Provider: ProviderLocal, Subject: "anonymous"}, "local:anonymous"},
		{"local account named agent", &Identity{Provider: ProviderLocal, Subject: "Agent"}, "local:agent"},
		{"provider with no subject", &Identity{Provider: "oidc", Email: "a@x.test", EmailVerified: true}, "nobody:oidc"},
		{"local with no subject", &Identity{Provider: ProviderLocal}, "nobody:local"},
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

// Only the identity with no subject from no provider is anonymous; a
// subjectless provider identity owns nothing, not everything.
func TestNobodyOwnsNothing(t *testing.T) {
	for _, id := range []*Identity{{Provider: "oidc"}, {Provider: "github"}, {Provider: ProviderLocal}} {
		if o := id.Owner(); o == Anonymous || !OwnsNothing(o) {
			t.Errorf("%+v owns %q", id, o)
		}
	}
	if OwnsNothing(Anonymous) || OwnsNothing("local:bob") {
		t.Error("a real owner reads as nobody")
	}
}

// The proxy vouches for the email it sends, with or without a user, and a
// request with neither is anonymous.
func TestProxyHeadersOwner(t *testing.T) {
	for _, tc := range []struct {
		user, email, want string
	}{
		{"pat", "Pat@Example.test", "pat@example.test"},
		{"", "pat@example.test", "pat@example.test"},
		{"pat", "", "pat"},
		{"", "", "anonymous"},
		{"local:bob", "", "proxy:local:bob"},
		{"anonymous", "", "proxy:anonymous"},
		{"ANONYMOUS", "", "proxy:ANONYMOUS"},
		{"agent", "", "proxy:agent"},
		{"anonymous", "not-an-address", "proxy:anonymous"},
		{"", "not-an-address", "nobody:proxy"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.user != "" {
			r.Header.Set("X-Abhed-User", tc.user)
		}
		if tc.email != "" {
			r.Header.Set("X-Abhed-Email", tc.email)
		}
		id := headerIdentity(r)
		if got := id.Owner(); got != tc.want {
			t.Errorf("user %q email %q: owner %q, want %q", tc.user, tc.email, got, tc.want)
		}
		if named := tc.user != "" || tc.email != ""; named != (id.Provider == ProviderProxy && id.EmailVerified) {
			t.Errorf("user %q email %q: provider %q verified %v", tc.user, tc.email, id.Provider, id.EmailVerified)
		}
	}
}

// The identity a sign-in issues is already a local one, before any request
// re-reads the account.
func TestSignInIssuesALocalIdentity(t *testing.T) {
	l := NewLocalAuth(NewMemoryUserStore(), time.Hour, false)
	if err := l.CreateUser(context.Background(), User{Username: "bob"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	u, err := l.Authenticate(context.Background(), "bob", "correct-horse-1")
	if err != nil {
		t.Fatal(err)
	}
	l.issue(httptest.NewRecorder(), u)
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.sessions {
		if s.Identity.Provider != ProviderLocal || s.Identity.Owner() != "local:bob" {
			t.Fatalf("issued %+v", s.Identity)
		}
	}
	if len(l.sessions) != 1 {
		t.Fatalf("%d sessions", len(l.sessions))
	}
}
