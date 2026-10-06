package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Which accounts files an untrusted workspace's trust decides: the default
// file and any other inside it, a link planted there included; not one
// outside it, nor the managed configuration's.
func TestUsersFileInsideAnUntrustedWorkspaceIsIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(TrustEnv, "")
	ws := t.TempDir()
	cfg, err := LoadWith(ws, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if src := cfg.UsersFile(ws); !src.Ignored || src.Path != WorkspaceUsersFile(ws) {
		t.Fatalf("default: %+v", src)
	}
	cfg.Auth.UsersFile = "state/users.json"
	if src := cfg.UsersFile(ws); !src.Ignored {
		t.Fatalf("relative inside: %+v", src)
	}
	outside := filepath.Join(t.TempDir(), "users.json")
	cfg.Auth.UsersFile = outside
	if src := cfg.UsersFile(ws); src.Ignored || src.Path != outside {
		t.Fatalf("outside: %+v", src)
	}
	// A link inside the workspace is where it sits, not where it points.
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := WorkspaceUsersFile(ws)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.UsersFile = link
	if src := cfg.UsersFile(ws); !src.Ignored {
		t.Fatalf("link inside: %+v", src)
	}
	// The managed configuration's choice stands wherever it points.
	cfg.ManagedKeys = []string{"auth.users_file"}
	if src := cfg.UsersFile(ws); src.Ignored {
		t.Fatalf("managed: %+v", src)
	}
	cfg.ManagedKeys = nil
	if w := cfg.UsersIgnoredWarning(ws); w == "" {
		t.Fatal("no warning for an ignored file")
	}

	trusted, err := LoadWith(ws, LoadOptions{Trust: TrustGranted})
	if err != nil {
		t.Fatal(err)
	}
	if src := trusted.UsersFile(ws); src.Ignored {
		t.Fatalf("trusted: %+v", src)
	}
}

// A workspace's .abhed that is a link to a directory elsewhere is still the
// workspace's: its accounts need the workspace's trust, through the
// workspace as given and through its canonical path.
func TestUsersFileInALinkedAbhedDirIsIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(TrustEnv, "")
	real := t.TempDir()
	ws := filepath.Join(real, "ws")
	planted := filepath.Join(real, "planted")
	for _, d := range []string{ws, planted} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(planted, "users.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(planted, filepath.Join(ws, ".abhed")); err != nil {
		t.Fatal(err)
	}
	// The workspace reached through a link of its own, as well as directly.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(ws, alias); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{ws, alias} {
		cfg, err := LoadWith(dir, LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if src := cfg.UsersFile(dir); !src.Ignored {
			t.Fatalf("%s: linked .abhed read untrusted: %+v", dir, src)
		}
		cfg.Auth.UsersFile = filepath.Join(dir, ".abhed", "users.json")
		if src := cfg.UsersFile(dir); !src.Ignored {
			t.Fatalf("%s: linked .abhed named explicitly read untrusted: %+v", dir, src)
		}
	}
	// A path outside that resolves into the workspace is the workspace's too.
	back := filepath.Join(t.TempDir(), "users.json")
	if err := os.Symlink(filepath.Join(ws, "users.json"), back); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "users.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(ws, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.UsersFile = back
	if src := cfg.UsersFile(ws); !src.Ignored {
		t.Fatalf("outside link into the workspace read untrusted: %+v", src)
	}
}

// RecordDecision, as abhed trust grant writes it, keeps the accounts grant.
func TestRecordDecisionKeepsUsers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(TrustEnv, "")
	ws := t.TempDir()
	if err := GrantUsers(ws); err != nil {
		t.Fatal(err)
	}
	if err := RecordDecision(ws, Reviewed{SHA256: "abc"}, false, false); err != nil {
		t.Fatal(err)
	}
	if ok, why := decideUsers(WorkspaceTrust{Workspace: canonical(ws)}, LoadOptions{}); !ok || why != "stored" {
		t.Fatalf("after RecordDecision: %v %s", ok, why)
	}
}

// The workspace that is the home directory keeps the user's own accounts.
func TestHomeAccountsAreTheUsersOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(TrustEnv, "")
	cfg, err := LoadWith(home, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if src := cfg.UsersFile(home); src.Ignored {
		t.Fatalf("home: %+v", src)
	}
}

// A decision about the configuration file keeps the one about accounts.
func TestRecordDecisionKeepsTheAccountsGrant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(TrustEnv, "")
	ws := t.TempDir()
	if err := GrantUsers(ws); err != nil {
		t.Fatal(err)
	}
	if err := DeclineTrust(ws, "abc"); err != nil {
		t.Fatal(err)
	}
	if ok, why := decideUsers(WorkspaceTrust{Workspace: canonical(ws)}, LoadOptions{}); !ok || why != "stored" {
		t.Fatalf("after a decision on the file: %v %s", ok, why)
	}
	if ok, _ := decideUsers(WorkspaceTrust{Workspace: canonical(ws)}, LoadOptions{Trust: TrustRefused}); ok {
		t.Fatal("refused trust read the accounts")
	}
}
