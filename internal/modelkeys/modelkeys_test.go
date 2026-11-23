package modelkeys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// fakeChain is a Keychain in memory; no test reaches the real one.
type fakeChain struct{ m map[string]string }

func (f *fakeChain) Get(n string) (string, bool, error) { v, ok := f.m[n]; return v, ok, nil }
func (f *fakeChain) Set(n, v string) error              { f.m[n] = v; return nil }
func (f *fakeChain) Delete(n string) (bool, error) {
	_, ok := f.m[n]
	delete(f.m, n)
	return ok, nil
}

const key = "sk-ant-api03-test-0123456789abcdef"

func TestNoSystemKeychainUnderTest(t *testing.T) {
	if NoKeychain() == "" {
		t.Fatal("a test binary must never use the system Keychain")
	}
}

func TestStoreKeychainThenFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secrets.json")
	chain := &fakeChain{m: map[string]string{}}
	defer Use(chain, file)()
	// A copy in the file store is removed when the Keychain takes the key.
	if err := secrets.Open(file).SetHeld(secrets.ModelKeyPrefix+"ANTHROPIC_API_KEY", "old-key-0000000000"); err != nil {
		t.Fatal(err)
	}
	kind, err := Store("ANTHROPIC_API_KEY", key)
	if err != nil || kind != KindKeychain {
		t.Fatalf("Store = %q, %v", kind, err)
	}
	if _, ok, _ := secrets.Open(file).HeldValue(secrets.ModelKeyPrefix + "ANTHROPIC_API_KEY"); ok {
		t.Fatal("the old file copy was left to be found")
	}
	v, k, err := Lookup("ANTHROPIC_API_KEY")
	if err != nil || v != key || k != KindKeychain {
		t.Fatalf("Lookup = %q %q %v", v, k, err)
	}
	from, err := Remove("ANTHROPIC_API_KEY")
	if err != nil || len(from) != 1 || from[0] != KindKeychain {
		t.Fatalf("Remove = %v %v", from, err)
	}
}

func TestStoreFileWithoutKeychain(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secrets.json")
	defer Use(nil, file)()
	kind, err := Store("OPENAI_API_KEY", key)
	if err != nil || kind != KindFile {
		t.Fatalf("Store = %q, %v", kind, err)
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store mode %v %v", fi.Mode(), err)
	}
	// Held: never offered to a command by name.
	if got := secrets.Open(file).Offered(); len(got) != 0 {
		t.Fatalf("a model key is offered to commands: %v", got)
	}
	if _, err := secrets.Open(file).Env([]string{secrets.ModelKeyPrefix + "OPENAI_API_KEY"}); err == nil {
		t.Fatal("a model key was given to a command")
	}
	v, k, _ := Lookup("OPENAI_API_KEY")
	if v != key || k != KindFile {
		t.Fatalf("Lookup = %q %q", v, k)
	}
}

func TestStoreRefusesBadValues(t *testing.T) {
	defer Use(&fakeChain{m: map[string]string{}}, filepath.Join(t.TempDir(), "s.json"))()
	for _, v := range []string{"short", "has space in it 123", "tab\there-0123456"} {
		if _, err := Store("ANTHROPIC_API_KEY", v); err == nil {
			t.Errorf("Store(%q) was accepted", v)
		}
	}
	if _, err := Store("lower", key); err == nil {
		t.Error("a lower-case name was accepted")
	}
}

func TestLookupRedactsEverywhere(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secrets.json")
	chain := &fakeChain{m: map[string]string{"GROQ_API_KEY": "gsk-redact-me-0123456789"}}
	defer Use(chain, file)()
	red := secrets.Open(filepath.Join(t.TempDir(), "other.json")).Session()
	if _, _, err := Lookup("GROQ_API_KEY"); err != nil {
		t.Fatal(err)
	}
	out := string(red.Redact([]byte(`{"text":"the key is gsk-redact-me-0123456789"}`)))
	if strings.Contains(out, "gsk-redact-me") {
		t.Fatalf("a key looked up was not redacted: %s", out)
	}
}

func TestDefaultNameOnlyAtOfficialEndpoint(t *testing.T) {
	for _, c := range []struct{ typ, url, want string }{
		{"anthropic", "", "ANTHROPIC_API_KEY"},
		{"anthropic", "https://api.anthropic.com", "ANTHROPIC_API_KEY"},
		{"anthropic", "https://proxy.example.com", ""},
		{"anthropic", "http://api.anthropic.com", ""},
		{"openai", "http://127.0.0.1:4000/v1", ""},
		{"ollama", "", ""},
	} {
		if got := DefaultName(c.typ, c.url); got != c.want {
			t.Errorf("DefaultName(%s, %s) = %q, want %q", c.typ, c.url, got, c.want)
		}
	}
}

// TestSecurityKeepsValueOutOfArgv runs a stub security program: the key
// goes on standard input, never in the arguments another process can list.
func TestSecurityKeepsValueOutOfArgv(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	stub := filepath.Join(dir, "security")
	script := `#!/bin/sh
echo "ARGV $*" >> "` + log + `"
if [ "$1" = "-i" ]; then cat >> "` + log + `"; echo "$(cat "` + log + `" | sed -n 's/.*-w "\([^"]*\)".*/\1/p' | tail -1)" > "` + dir + `/item"; exit 0; fi
if [ "$1" = "find-generic-password" ]; then [ -s "` + dir + `/item" ] || exit 44; cat "` + dir + `/item"; exit 0; fi
if [ "$1" = "delete-generic-password" ]; then [ -s "` + dir + `/item" ] || exit 44; : > "` + dir + `/item"; exit 0; fi
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Security{Program: stub}
	if _, ok, err := s.Get("ANTHROPIC_API_KEY"); ok || err != nil {
		t.Fatalf("Get before Set = %v %v", ok, err)
	}
	if err := s.Set("ANTHROPIC_API_KEY", key); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.Get("ANTHROPIC_API_KEY"); !ok || v != key || err != nil {
		t.Fatalf("Get = %q %v %v", v, ok, err)
	}
	b, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "ARGV") && strings.Contains(line, key) {
			t.Fatalf("the key was in a command line: %s", line)
		}
	}
	if ok, err := s.Delete("ANTHROPIC_API_KEY"); !ok || err != nil {
		t.Fatalf("Delete = %v %v", ok, err)
	}
	if err := s.Set("ANTHROPIC_API_KEY", `bad"quote-0123456`); err == nil {
		t.Fatal("a value that would break the command was accepted")
	}
}
