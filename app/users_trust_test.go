package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
)

// plantedWorkspace is a workspace with no configuration of its own and an
// account planted in .abhed/users.json, opened by a person whose own
// configuration turns local accounts on.
func plantedWorkspace(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_USERS_FILE", "")
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv(config.TrustEnv, "")
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"auth":{"mode":"local"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	fs, err := auth.NewFileUserStore(filepath.Join(ws, ".abhed", "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	la := auth.NewLocalAuth(fs, 0, true)
	if err := la.CreateUser(context.Background(), auth.User{Username: "planted", Groups: []string{"admin"}}, "planted-pass-12345"); err != nil {
		t.Fatal(err)
	}
	return ws
}

// localAuthFor builds the server's identity layer as serve does and returns
// its local accounts.
func localAuthFor(t *testing.T, ws string, trust config.TrustChoice) *auth.LocalAuth {
	t.Helper()
	cfg, err := config.LoadWith(ws, config.LoadOptions{Trust: trust})
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.trust = trust
	mw, err := a.buildAuth(context.Background(), cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range mw.Providers {
		if la, ok := p.(*auth.LocalAuth); ok {
			return la
		}
	}
	t.Fatal("no local accounts provider")
	return nil
}

// An untrusted workspace's .abhed/users.json is not read: its accounts are
// not listed and cannot sign in to the server.
func TestUntrustedWorkspaceAccountsAreIgnored(t *testing.T) {
	ws := plantedWorkspace(t)
	la := localAuthFor(t, ws, config.TrustAsStored)
	users, err := la.ListUsers(context.Background())
	if err != nil || len(users) != 0 {
		t.Fatalf("an untrusted workspace's accounts were read: %v %v", users, err)
	}
	if u, err := la.Authenticate(context.Background(), "planted", "planted-pass-12345"); err == nil {
		t.Fatalf("a planted account signed in: %+v", u)
	}
	// -trust-workspace refused is as untrusted.
	if users, _ := localAuthFor(t, ws, config.TrustRefused).ListUsers(context.Background()); len(users) != 0 {
		t.Fatalf("refused trust read %v", users)
	}
}

// A trusted workspace's accounts work as before: for one run, or once
// `abhed trust grant` has recorded it.
func TestTrustedWorkspaceAccountsSignIn(t *testing.T) {
	ws := plantedWorkspace(t)
	if _, err := localAuthFor(t, ws, config.TrustGranted).Authenticate(context.Background(), "planted", "planted-pass-12345"); err != nil {
		t.Fatalf("-trust-workspace: %v", err)
	}
	var out strings.Builder
	if code := trustCmd(ws, []string{"grant"}, &out); code != 0 {
		t.Fatalf("trust grant = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "users.json") {
		t.Fatalf("trust grant did not name the accounts: %s", out.String())
	}
	// Accounts trust is not tied to the file's content, so it must not say a change re-asks.
	if s := out.String(); strings.Contains(s, "untrusted again") || !strings.Contains(s, "until `abhed trust revoke`") {
		t.Fatalf("an accounts-only grant misdescribes how long it lasts: %s", s)
	}
	if _, err := localAuthFor(t, ws, config.TrustAsStored).Authenticate(context.Background(), "planted", "planted-pass-12345"); err != nil {
		t.Fatalf("after trust grant: %v", err)
	}
	// Revoking forgets it again.
	out.Reset()
	if code := trustCmd(ws, []string{"revoke"}, &out); code != 0 {
		t.Fatalf("trust revoke = %d", code)
	}
	if users, _ := localAuthFor(t, ws, config.TrustAsStored).ListUsers(context.Background()); len(users) != 0 {
		t.Fatalf("revoked trust still read %v", users)
	}
}

// `abhed user list` in an untrusted workspace says which file it ignores,
// lists nothing from it, and a change is refused rather than written there.
func TestUserListSaysItIgnoresAnUntrustedFile(t *testing.T) {
	ws := plantedWorkspace(t)
	code, out := runUser(t, ws, "list")
	file := filepath.Join(ws, ".abhed", "users.json")
	if code != 0 || !strings.Contains(out, "ignoring the accounts in") || !strings.Contains(out, filepath.Base(filepath.Dir(file))) ||
		!strings.Contains(out, "abhed trust grant") {
		t.Fatalf("user list = %d %q, want a warning naming the file", code, out)
	}
	if strings.Contains(out, "planted") {
		t.Fatalf("user list showed a planted account: %q", out)
	}
	before, _ := os.ReadFile(file)
	if code, out := runUser(t, ws, "add", "eve", "-password", "correct-horse-1"); code != 1 || !strings.Contains(out, "not trusted") {
		t.Fatalf("user add = %d %q, want refused", code, out)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("user add wrote the ignored file")
	}
}

// A workspace's .abhed that links to a directory elsewhere still needs the
// workspace's trust: `abhed user list` neither lists nor signs in its accounts.
func TestUserListIgnoresALinkedAbhedDir(t *testing.T) {
	planted := plantedWorkspace(t)
	ws := t.TempDir()
	if err := os.Symlink(filepath.Join(planted, ".abhed"), filepath.Join(ws, ".abhed")); err != nil {
		t.Fatal(err)
	}
	code, out := runUser(t, ws, "list")
	if code != 0 || !strings.Contains(out, "ignoring the accounts in") || strings.Contains(out, "planted") {
		t.Fatalf("user list through a linked .abhed = %d %q, want the file ignored", code, out)
	}
	if _, err := localAuthFor(t, ws, config.TrustAsStored).Authenticate(context.Background(), "planted", "planted-pass-12345"); err == nil {
		t.Fatal("an account behind a linked .abhed signed in")
	}
}

// An ignored file is recorded as config.refused with who ran the process,
// and only when there is one and local accounts are in use.
func TestIgnoredAccountsFileIsRecorded(t *testing.T) {
	ws := plantedWorkspace(t)
	cfg, err := config.LoadWith(ws, config.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range cfg.Attempts() {
		if a.Key == "auth.users_file" {
			found = a.Decision == "ignored_untrusted" && a.Layer == config.LayerWorkspace && strings.HasSuffix(a.Source, "users.json")
		}
	}
	if !found {
		t.Fatalf("no ignored_untrusted attempt for the accounts file: %+v", cfg.Attempts())
	}
	cfg, err = config.LoadWith(ws, config.LoadOptions{Trust: config.TrustGranted})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cfg.Attempts() {
		if a.Key == "auth.users_file" {
			t.Fatalf("a trusted workspace's file was recorded as ignored: %+v", a)
		}
	}
}

// The managed auth.users_file, or one set outside the workspace, is read
// whatever the workspace's trust.
func TestAccountsOutsideTheWorkspaceAreRead(t *testing.T) {
	ws := plantedWorkspace(t)
	outside := filepath.Join(t.TempDir(), "users.json")
	fs, err := auth.NewFileUserStore(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.NewLocalAuth(fs, 0, true).CreateUser(context.Background(), auth.User{Username: "ops"}, "ops-password-12345"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_USERS_FILE", outside)
	la := localAuthFor(t, ws, config.TrustAsStored)
	if _, err := la.Authenticate(context.Background(), "ops", "ops-password-12345"); err != nil {
		t.Fatalf("an account outside the workspace: %v", err)
	}
	if _, err := la.Authenticate(context.Background(), "planted", "planted-pass-12345"); err == nil {
		t.Fatal("the planted account signed in")
	}
}
