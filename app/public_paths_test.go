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
