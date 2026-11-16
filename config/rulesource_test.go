package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A refusal by a configured rule names the layer the rule came from, in the
// reason the model and the person read and in the result's Source: the org
// policy apart from the machine's managed file, the workspace, the user's
// file, a flag.
func TestDenialNamesTheRulesLayer(t *testing.T) {
	f := newOrg(t)
	f.write(t, `{"permissions":{"deny":["bash(curl*)"]}}`, time.Now())
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"deny":["bash(wget*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	home := os.Getenv("HOME")
	writeConfig(t, home, `{"permissions":{"deny":["bash(nc *)"]}}`)
	ws := t.TempDir()
	// A workspace's deny list would replace the user's, so it asks instead.
	writeConfig(t, ws, `{"permissions":{"ask":["bash(make deploy*)"]}}`)
	t.Setenv(TrustEnv, "1")

	cfg, err := Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = cfg.Apply(Overrides{Deny: []string{"bash(ftp *)"}}); err != nil {
		t.Fatal(err)
	}
	pol := policy.New(policy.ModeBypass)
	pol.RuleSource = cfg.RuleSource
	if err := pol.AddDeny(cfg.Permissions.Deny...); err != nil {
		t.Fatal(err)
	}
	if err := pol.AddAsk(cfg.Permissions.Ask...); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]struct{ source, where string }{
		"curl -sI https://example.com": {LayerOrg, "from the org policy (https://org.example, version 1)"},
		"wget x":                       {LayerManaged, "from the managed configuration " + path},
		"nc host 80":                   {LayerUser, "from your user configuration ~/.abhed/config.json"},
		"make deploy":                  {LayerWorkspace, "from the workspace configuration " + filepath.Join(ws, ".abhed", "config.json")},
		"ftp host":                     {LayerFlag, "from the -deny flag"},
	} {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		res := pol.Evaluate("bash", true, args)
		if res.Decision == policy.Allow || res.Source != want.source || !strings.Contains(res.Reason, want.where) {
			t.Errorf("%s: %s source %q reason %q; want %q, %q", cmd, res.Decision, res.Source, res.Reason, want.source, want.where)
		}
	}
}
