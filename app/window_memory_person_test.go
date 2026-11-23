package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/model"
)

// windowFiles are the context-windows files anywhere under ~/.abhed.
func windowFiles(t *testing.T, homeDir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(homeDir, ".abhed"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "context-windows") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func claudeAdapter(t *testing.T) model.Adapter {
	t.Helper()
	a, err := model.New(model.Spec{Type: "anthropic", BaseURL: "https://api.example", Model: "claude-sonnet-5-5", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A guest keeps no window memory: building adapters and a provider naming a
// limit leave no file anywhere, during the guest's run or after it.
func TestGuestKeepsNoWindowMemory(t *testing.T) {
	signInTest(t)
	h, _ := os.UserHomeDir()
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	a := claudeAdapter(t)
	if !model.LearnWindow(a, 200_000, true) {
		t.Fatal("not learned for the guest's session")
	}
	if files := windowFiles(t, h); len(files) != 0 {
		t.Fatalf("a guest left %v", files)
	}
	endGuest()
	if files := windowFiles(t, h); len(files) != 0 {
		t.Fatalf("after the guest: %v", files)
	}
}

// An account's limits live in its own folder, are not seen by another
// account or a guest, and are its own again when it signs back in.
func TestWindowMemoryIsPerAccount(t *testing.T) {
	signInTest(t)
	h, _ := os.UserHomeDir()
	s, _ := identity.Open()
	for _, u := range []string{"a1", "a2"} {
		if _, _, err := s.Create(u, "correct horse "+u, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := switchPrincipal("auth.signin", "a1", "correct horse a1", ""); err != nil {
		t.Fatal(err)
	}
	model.LearnWindow(claudeAdapter(t), 200_000, true)
	files := windowFiles(t, h)
	if len(files) == 0 {
		t.Fatal("nothing kept for the account")
	}
	root := filepath.Join(h, ".abhed", "accounts") + string(filepath.Separator)
	for _, f := range files {
		if !strings.HasPrefix(f, root) {
			t.Fatalf("an account's window file outside its folder: %s", f)
		}
	}

	if _, err := switchPrincipal("auth.signin", "a2", "correct horse a2", ""); err != nil {
		t.Fatal(err)
	}
	if w := claudeAdapter(t).Profile().ContextWindow; w != 1_000_000 {
		t.Fatalf("a2 sees a1's limit: %d", w)
	}
	if _, err := switchPrincipal("auth.guest", "", "", ""); err != nil {
		t.Fatal(err)
	}
	before := len(windowFiles(t, h))
	if w := claudeAdapter(t).Profile().ContextWindow; w != 1_000_000 {
		t.Fatalf("a guest sees an account's limit: %d", w)
	}
	if after := len(windowFiles(t, h)); after != before {
		t.Fatal("a guest made a window file")
	}
	if _, err := switchPrincipal("auth.signin", "a1", "correct horse a1", ""); err != nil {
		t.Fatal(err)
	}
	if w := claudeAdapter(t).Profile().ContextWindow; w != 200_000 {
		t.Fatalf("a1's own limit not recalled: %d", w)
	}
}

// Once no one is working (signed out, or a machine command), nothing keeps
// limits: not at ~/.abhed, not in the last person's folder.
func TestNoOneKeepsNoWindowMemory(t *testing.T) {
	signInTest(t)
	h, _ := os.UserHomeDir()
	s, _ := identity.Open()
	if _, _, err := s.Create("a1", "correct horse a1", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := switchPrincipal("auth.signin", "a1", "correct horse a1", ""); err != nil {
		t.Fatal(err)
	}
	endGuest() // the process is no one's
	model.LearnWindow(claudeAdapter(t), 200_000, true)
	if files := windowFiles(t, h); len(files) != 0 {
		t.Fatalf("kept with no one working: %v", files)
	}
	// A machine command never reaches the gate: Main installs no memory.
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	stdout := os.Stdout
	os.Stdout = null
	Main([]string{"providers"})
	os.Stdout = stdout
	model.LearnWindow(claudeAdapter(t), 200_000, true)
	if files := windowFiles(t, h); len(files) != 0 {
		t.Fatalf("kept after a machine command: %v", files)
	}
}

// abhed doctor is a machine command: it shows the model's window but no one
// is working, so it reads and makes no window memory.
func TestDoctorKeepsNoWindowMemory(t *testing.T) {
	srv := doctorEndpoint(t)
	h := accountHome(t)
	ws := t.TempDir()
	cfg := `{"model":{"default":"c","providers":{"c":{"type":"anthropic","base_url":"` + srv.URL + `","model":"claude-sonnet-5-5","api_key":"k"}}}}`
	_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755)
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1")
	t.Setenv("ABHED_DATABASE_URL", "")
	_, _ = stdoutOf(t, func() int { return Main([]string{"-C", ws, "doctor"}) })
	if files := windowFiles(t, h); len(files) != 0 {
		t.Fatalf("doctor made %v", files)
	}
}
