package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// The icon is requested before anyone signs in; the production list, not a
// test's own, has to allow it.
func TestBuiltAuthLetsTheIconThrough(t *testing.T) {
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Auth.UsersFile = filepath.Join(ws, "users.json")
	mw, err := newApp().buildAuth(context.Background(), cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/favicon.ico", "/favicon.svg", "/v1/signin"} {
		if !slices.Contains(mw.PublicPaths, p) {
			t.Errorf("%s is not public: %v", p, mw.PublicPaths)
		}
	}
	if slices.Contains(mw.PublicPaths, "/account") || slices.Contains(mw.PublicPaths, "/ide") {
		t.Errorf("a signed-in page became public: %v", mw.PublicPaths)
	}
}

// auth.guest adds the guest provider and its start, and only when it is on;
// it needs a deployment that signs people in.
func TestBuiltAuthOffersGuestsOnlyWhenAsked(t *testing.T) {
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Auth.UsersFile = filepath.Join(ws, "users.json")
	mw, err := newApp().buildAuth(context.Background(), cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(mw.PublicPaths, "/v1/guest") {
		t.Fatal("guests offered by default")
	}
	cfg.Auth.Guest = true
	if mw, err = newApp().buildAuth(context.Background(), cfg, ws); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(mw.PublicPaths, "/v1/guest") {
		t.Fatalf("guest start not public: %v", mw.PublicPaths)
	}
	none := config.Default()
	none.Auth.Guest = true
	if err := none.Validate(); err == nil {
		t.Error("auth.guest with no sign-in validated")
	}
}
