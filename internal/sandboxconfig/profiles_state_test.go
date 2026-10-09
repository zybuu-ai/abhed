package sandboxconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// The profiles file, kept outside ~/.abhed, and the client certificate, key
// and CA a profile names are state: a command can neither read the tokens
// or the key nor rewrite the CA the command line trusts.
func TestProfilesAndClientKeysAreState(t *testing.T) {
	base := outsideWritableAreas(t)
	controlPlaneHome(t, base)
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "login")
	secret := map[string]string{
		filepath.Join(dir, "profiles.json"): "",
		filepath.Join(dir, "client.key"):    "client-key",
		filepath.Join(dir, "client.pem"):    "client-cert",
		filepath.Join(dir, "ca.pem"):        "server-ca",
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for p, body := range secret {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	profilesFile := filepath.Join(dir, "profiles.json")
	body := `{"version":1,"profiles":{"lab":{"url":"https://lab.example","token":"tok-secret",
	  "ca_file":"` + filepath.Join(dir, "ca.pem") + `","cert_file":"` + filepath.Join(dir, "client.pem") + `",
	  "key_file":"` + filepath.Join(dir, "client.key") + `"}}}`
	if err := os.WriteFile(profilesFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	secret[profilesFile] = "tok-secret"
	t.Setenv(config.ProfilesEnv, profilesFile)

	cfg := config.Default()
	cfg.Sandbox.MinTier = "process"
	p, err := Policy(cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	tp := ToolStatePaths(cfg, ws)
	for f := range secret {
		if !slices.Contains(p.StatePaths, f) || !slices.Contains(tp, f) {
			t.Errorf("%s is not state: sandbox %v, tools %v", f, p.StatePaths, tp)
		}
	}
	if testing.Short() {
		return
	}
	sb, err := Build(cfg, ws)
	if err != nil {
		if _, statErr := os.Stat("/usr/bin/sandbox-exec"); statErr == nil {
			t.Fatalf("Seatbelt is present but no sandbox was built: %v", err)
		}
		t.Skipf("no process sandbox here: %v", err)
	}
	defer func() { _ = sandbox.Close(sb) }()
	for f, body := range secret {
		out, _ := sb.Command(context.Background(), ws, "cat '"+f+"'; echo x > '"+f+"'").CombinedOutput()
		if strings.Contains(string(out), body) {
			t.Errorf("a command read %s: %s", f, out)
		}
		if got, _ := os.ReadFile(f); string(got) == "x\n" {
			t.Errorf("a command rewrote %s", f)
		}
	}
}
