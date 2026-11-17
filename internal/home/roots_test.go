package home_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/customcmd"
	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/orgpolicy"
	"github.com/zybuu-ai/abhed/store/local"
)

// Every piece of state that is the person's follows the chosen person; every
// piece that is the machine's stays in ~/.abhed. A path added to either side
// belongs in this table.
func TestStateRootsSplit(t *testing.T) {
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv(home.EnvPerson, "")
	t.Setenv(config.ProfilesEnv, "")
	t.Setenv(secrets.EnvFile, "")
	root := filepath.Join(h, home.DirName, home.AccountsDir, "a1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := home.SetPerson(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.SetPerson("") })

	must := func(p string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cmdTrust, err := customcmd.DefaultTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	person := map[string]string{
		"records":       must(local.DefaultDir()),
		"history":       ui.HistoryPath(t.TempDir()),
		"profiles":      must(config.ProfilesPath()),
		"trust":         must(config.TrustStorePath()),
		"startup files": must(config.StartupFilesPath()),
		"secrets":       must(secrets.DefaultPath()),
		"command trust": cmdTrust.Path,
		"auto memory":   must(tools.AutoMemoryFor(t.TempDir())),
	}
	for name, p := range person {
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			t.Errorf("%s: %s is not under the person's folder %s", name, p, root)
		}
	}
	machine := map[string]string{
		"org policy": must(orgpolicy.Path()),
		"cli key":    must(config.Config{}.CLIKeyFile(t.TempDir())),
	}
	for name, p := range machine {
		if strings.HasPrefix(p, root) || !strings.HasPrefix(p, filepath.Join(h, home.DirName)) {
			t.Errorf("%s: %s should be the machine's", name, p)
		}
	}
}
