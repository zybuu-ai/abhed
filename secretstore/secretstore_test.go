package secretstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/secretstore"
)

// The variable names are what an edition and an operator set; they do not move.
func TestVariableNames(t *testing.T) {
	if secretstore.EnvFile != "ABHED_SECRETS_FILE" || secretstore.EnvAccountsDir != "ABHED_ACCOUNT_SECRETS_DIR" || secretstore.MinLength != 8 {
		t.Fatalf("%s %s %d", secretstore.EnvFile, secretstore.EnvAccountsDir, secretstore.MinLength)
	}
}

func TestDefaultPathsFollowTheEnvironment(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ops.json")
	t.Setenv(secretstore.EnvFile, file)
	t.Setenv(secretstore.EnvAccountsDir, "")
	if p, err := secretstore.DefaultPath(); err != nil || p != file {
		t.Fatalf("default path %q %v", p, err)
	}
	// Per-account stores sit beside the operator's, unless moved.
	if d, err := secretstore.AccountsDir(); err != nil || d != strings.TrimSuffix(file, ".json")+".d" {
		t.Fatalf("accounts dir %q %v", d, err)
	}
	moved := t.TempDir()
	t.Setenv(secretstore.EnvAccountsDir, moved)
	if d, err := secretstore.AccountsDir(); err != nil || d != moved {
		t.Fatalf("moved accounts dir %q %v", d, err)
	}
}

func TestValidName(t *testing.T) {
	for name, ok := range map[string]bool{"GITHUB_TOKEN": true, "A": true, "github_token": false, "1X": false,
		"": false, "A-B": false, strings.Repeat("A", 65): false} {
		if secretstore.ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v", name, !ok)
		}
	}
}

// A file store keeps values for its owner only and redacts them by name.
func TestFileStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alice.json")
	st := secretstore.Open(path)
	if names, err := st.Names(); err != nil || len(names) != 0 {
		t.Fatalf("a missing file is not an empty store: %v %v", names, err)
	}
	if err := st.Set("bad name", "value-12345"); err == nil {
		t.Fatal("a name that is not a secret name was stored")
	}
	if err := st.Set("API_KEY", "value-12345"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("store mode %v %v", fi.Mode().Perm(), err)
		}
	}
	if v, err := secretstore.Open(path).Value("API_KEY"); err != nil || v != "value-12345" {
		t.Fatalf("value %q %v", v, err)
	}
	if _, err := st.Value("MISSING"); err == nil {
		t.Fatal("an unknown name gave a value")
	}
	if got := string(st.Redactor().Redact([]byte(`{"out":"key is value-12345"}`))); got != `{"out":"key is [secret:API_KEY]"}` {
		t.Fatalf("redacted %s", got)
	}
}

type failing struct{}

func (failing) Load(context.Context) (map[string]string, error) { return nil, errors.New("down") }
func (failing) Set(context.Context, string, string) error       { return errors.New("down") }
func (failing) Remove(context.Context, string) error            { return errors.New("down") }

type held map[string]string

func (h held) Load(context.Context) (map[string]string, error) { return h, nil }
func (h held) Set(_ context.Context, k, v string) error        { h[k] = v; return nil }
func (h held) Remove(_ context.Context, k string) error        { delete(h, k); return nil }

// A store kept by a driver hands out and redacts its values like a file;
// one that cannot be reached hands out nothing and withholds every payload.
func TestSourceStore(t *testing.T) {
	src := held{}
	st := secretstore.OpenSource("vault", src)
	if err := st.Set("DB_PASSWORD", "hunter2-hunter2"); err != nil {
		t.Fatal(err)
	}
	if names, err := st.Names(); err != nil || !slices.Equal(names, []string{"DB_PASSWORD"}) {
		t.Fatalf("names %v %v", names, err)
	}
	if got := string(st.Redactor().Redact([]byte(`"hunter2-hunter2"`))); got != `"[secret:DB_PASSWORD]"` {
		t.Fatalf("redacted %s", got)
	}
	down := secretstore.OpenSource("vault", failing{})
	if _, err := down.Value("DB_PASSWORD"); err == nil {
		t.Fatal("an unreachable source handed out a value")
	}
	if out := down.Redactor().Redact([]byte(`{"x":"y"}`)); out != nil {
		t.Fatalf("an unreachable source let a payload through: %s", out)
	}
}
