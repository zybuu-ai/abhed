package identity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/home"
)

func open() Rules { return Rules{GuestAllowed: true} }

// The gate's answer for every combination of what a start knows. A run that
// cannot ask never gets Ask.
func TestResolveTable(t *testing.T) {
	type setup struct {
		account    bool // an account "a" exists
		remembered bool // and is remembered
		legacy     bool // ~/.abhed holds data from before sign-in
	}
	cases := []struct {
		name  string
		setup setup
		req   Request
		want  Kind // "" with ask or err
		ask   bool
		err   bool
		note  bool
	}{
		{"guest flag", setup{}, Request{Guest: true, Rules: open()}, Guest, false, false, false},
		{"guest flag, guest off", setup{}, Request{Guest: true, Rules: Rules{GuestAllowed: false}}, "", false, true, false},
		{"remembered", setup{account: true, remembered: true}, Request{Rules: open()}, Account, false, false, false},
		{"remembered, interactive", setup{account: true, remembered: true}, Request{Interactive: true, Rules: open()}, Account, false, false, false},
		{"nothing, interactive", setup{}, Request{Interactive: true, Rules: open()}, "", true, false, false},
		{"nothing, headless", setup{}, Request{Rules: open()}, Guest, false, false, true},
		{"legacy, headless", setup{legacy: true}, Request{Rules: open()}, Legacy, false, false, true},
		{"legacy, headless, sign-in required", setup{legacy: true}, Request{Rules: Rules{SignIn: config.SignInRequired, GuestAllowed: true}}, "", false, true, false},
		{"nothing, headless, sign-in required", setup{}, Request{Rules: Rules{SignIn: config.SignInRequired, GuestAllowed: true}}, "", false, true, false},
		{"nothing, headless, guest off", setup{}, Request{Rules: Rules{GuestAllowed: false}}, "", false, true, false},
		{"remembered, sign-in required", setup{account: true, remembered: true}, Request{Rules: Rules{SignIn: config.SignInRequired}}, Account, false, false, false},
		{"remembered account, server required", setup{account: true, remembered: true}, Request{Rules: Rules{SignIn: config.SignInServer, Server: "https://abhed.corp"}}, "", false, true, false},
		{"-account, remembered", setup{account: true, remembered: true}, Request{Account: "a", Rules: open()}, Account, false, false, false},
		{"-account, not remembered, headless", setup{account: true}, Request{Account: "a", Rules: open()}, "", false, true, false},
		{"-account, not remembered, interactive", setup{account: true}, Request{Account: "a", Interactive: true, Rules: open()}, "", true, false, false},
		{"-account unknown", setup{}, Request{Account: "zz", Rules: open()}, "", false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testStore(t)
			if c.setup.account {
				e, _, err := s.Create("a", pw, "", "")
				if err != nil {
					t.Fatal(err)
				}
				if c.setup.remembered {
					if err := s.Remember(e.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if c.setup.legacy {
				m, _ := home.Machine()
				if err := os.MkdirAll(filepath.Join(m, "records"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(m, "records", "s1.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := Resolve(s, c.req)
			if c.err {
				if !errors.Is(err, ErrSignInRequired) {
					t.Fatalf("err = %v, want sign-in required", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.Ask != c.ask {
				t.Fatalf("ask = %v", out.Ask)
			}
			if !c.req.Interactive && out.Ask {
				t.Fatal("a run that cannot ask was told to ask")
			}
			if out.Principal.Kind != c.want {
				t.Fatalf("kind = %q, want %q", out.Principal.Kind, c.want)
			}
			if (out.Note != "") != c.note {
				t.Fatalf("note = %q", out.Note)
			}
		})
	}
}

// Once an account has taken over the data from before sign-in, a headless
// run no longer runs as before.
func TestLegacyEndsOnceTaken(t *testing.T) {
	s := testStore(t)
	m, _ := home.Machine()
	if err := os.MkdirAll(filepath.Join(m, "records"), 0o700); err != nil {
		t.Fatal(err)
	}
	e, _, err := s.Create("a", pw, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLegacyUser(e.ID, "yuvraj"); err != nil {
		t.Fatal(err)
	}
	out, err := Resolve(s, Request{Rules: open()})
	if err != nil || out.Principal.Kind != Guest {
		t.Fatalf("got %+v, %v", out, err)
	}
	b, _, _ := s.Create("b", pw, "", "")
	if err := s.SetLegacyUser(b.ID, "yuvraj"); err == nil {
		t.Error("a second account took over the same data")
	}
}

func TestOwnersAreNamespaced(t *testing.T) {
	if got := OwnerOf(Entry{Kind: KindAccount, Username: "Yuvraj"}); got != "device:yuvraj" {
		t.Errorf("account owner %q", got)
	}
	if got := OwnerOf(Entry{Kind: KindServer, Username: "y", URL: "https://abhed.corp:8443/x"}); got != "device:y@abhed.corp:8443" {
		t.Errorf("server owner %q", got)
	}
	if !strings.HasPrefix(GuestPrincipal().Owner, GuestOwner) {
		t.Error("guest owner")
	}
}

func TestRequiredErrorNamesTheWayOut(t *testing.T) {
	s := testStore(t)
	_, err := Resolve(s, Request{Rules: Rules{SignIn: config.SignInServer, Server: "https://abhed.corp"}})
	var re *RequiredError
	if !errors.As(err, &re) || !strings.Contains(re.Error(), "abhed login https://abhed.corp") {
		t.Fatalf("err = %v", err)
	}
}

// A managed server URL is compared as a URL, not byte for byte.
func TestAdmitsNormalisesServerURLs(t *testing.T) {
	r := Rules{SignIn: config.SignInServer, Server: "https://Abhed.corp:443/"}
	for _, u := range []string{"https://abhed.corp", "https://ABHED.corp/", "https://abhed.corp:443"} {
		if err := r.Admits(Entry{Kind: KindServer, URL: u}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://abhed.corp", "https://abhed.corp:8443", "https://evil.corp"} {
		if err := r.Admits(Entry{Kind: KindServer, URL: u}); err == nil {
			t.Errorf("%s admitted", u)
		}
	}
}

// Only work an earlier version recorded counts as data from before sign-in:
// trust.json, a plugin or an empty records folder alone, which a 1.3 command
// run before anyone signed in could leave, do not.
func TestLegacyDataIsRecordedWork(t *testing.T) {
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv(home.EnvPerson, "")
	m, _ := home.Machine()
	for _, p := range []string{"records", "history"} {
		if err := os.MkdirAll(filepath.Join(m, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"trust.json", "plugins.json"} {
		if err := os.WriteFile(filepath.Join(m, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if LegacyData() {
		t.Fatal("trust.json, plugins.json and empty folders counted as data from before sign-in")
	}
	if !LegacyItemsPresent() {
		t.Fatal("abhed account adopt would find nothing to move")
	}
	if err := os.WriteFile(filepath.Join(m, "history", "prompts"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !LegacyData() {
		t.Fatal("an earlier version's history did not count")
	}
}
