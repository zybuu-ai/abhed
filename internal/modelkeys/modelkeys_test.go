package modelkeys

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// fakeChain is a Keychain in memory; no test reaches the real one.
type fakeChain struct{ m map[string]string }

// slot keys a legacy item by its name, as the tests read it, and another
// service's by service|name.
func slot(s, n string) string {
	if s == Service {
		return n
	}
	return s + "|" + n
}

func (f *fakeChain) Get(s, n string) (string, bool, error) {
	if s == "" {
		// As find-generic-password with no service: any item of that account.
		for k, v := range f.m {
			if k == n || strings.HasSuffix(k, "|"+n) {
				return v, true, nil
			}
		}
		return "", false, nil
	}
	v, ok := f.m[slot(s, n)]
	return v, ok, nil
}
func (f *fakeChain) Set(s, n, v string) error { f.m[slot(s, n)] = v; return nil }
func (f *fakeChain) Delete(s, n string) (bool, error) {
	_, ok := f.m[slot(s, n)]
	delete(f.m, slot(s, n))
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
	if _, ok, err := s.Get(Service, "ANTHROPIC_API_KEY"); ok || err != nil {
		t.Fatalf("Get before Set = %v %v", ok, err)
	}
	if err := s.Set(Service, "ANTHROPIC_API_KEY", key); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.Get(Service, "ANTHROPIC_API_KEY"); !ok || v != key || err != nil {
		t.Fatalf("Get = %q %v %v", v, ok, err)
	}
	b, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "ARGV") && strings.Contains(line, key) {
			t.Fatalf("the key was in a command line: %s", line)
		}
	}
	if ok, err := s.Delete(Service, "ANTHROPIC_API_KEY"); !ok || err != nil {
		t.Fatalf("Delete = %v %v", ok, err)
	}
	if err := s.Set(Service, "ANTHROPIC_API_KEY", `bad"quote-0123456`); err == nil {
		t.Fatal("a value that would break the command was accepted")
	}
}

func TestKeychainOffReasons(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, c := range []struct {
		getenv func(string) string
		temp   bool
		want   string
	}{
		{env(), false, ""},
		{env("ABHED_SANDBOX", "1"), false, "inside an agent's command"},
		{env("ABHED_SANDBOX", ""), false, ""},
		{env("ABHED_TESTENV", "1"), false, "under a test"},
		{env("ABHED_KEYCHAIN", "OFF"), false, "ABHED_KEYCHAIN is off"},
		{env("ABHED_KEYCHAIN", "on"), false, ""},
		{env(), true, "temporary"},
	} {
		got := keychainOff(c.getenv, c.temp)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("keychainOff = %q, want %q", got, c.want)
		}
	}
}

func TestGuardRefusesStoreAndRemove(t *testing.T) {
	chain := &fakeChain{m: map[string]string{"X_KEY": key}}
	defer Use(chain, filepath.Join(t.TempDir(), "s.json"))()
	SetGuard(func() error { return errors.New("a guest keeps nothing") })
	t.Cleanup(func() { guard.Store(nil) })
	if _, err := Store("X_KEY", key+"2"); err == nil {
		t.Fatal("stored under the guard")
	}
	if _, err := Remove("X_KEY"); err == nil || chain.m["X_KEY"] != key {
		t.Fatal("removed under the guard")
	}
	if v, _, _ := Lookup("X_KEY"); v != key {
		t.Fatal("a lookup is not a change and is not refused")
	}
}

// personHome makes a home with an account folder for each id and a guest
// folder, and returns a func that makes one the person working.
func personHome(t *testing.T, ids ...string) func(kind, id string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv(secrets.EnvFile, "")
	m := filepath.Join(h, ".abhed")
	for _, id := range ids {
		if err := os.MkdirAll(filepath.Join(m, home.AccountsDir, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(m, home.GuestDir, "g1"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.SetPerson("") })
	return func(kind, id string) {
		t.Helper()
		root := ""
		switch kind {
		case "account":
			root = filepath.Join(m, home.AccountsDir, id)
		case "guest":
			root = filepath.Join(m, home.GuestDir, "g1")
		}
		if err := home.SetPerson(root); err != nil {
			t.Fatal(err)
		}
	}
}

// Stored keys are the Abhed account's, not the machine user's: B never sees
// A's, a guest sees none, and before sign-in nothing changes.
func TestKeysBelongToTheAccount(t *testing.T) {
	for _, withChain := range []bool{true, false} {
		t.Run(map[bool]string{true: "keychain", false: "store"}[withChain], func(t *testing.T) {
			be := personHome(t, "alice", "bob")
			chain := &fakeChain{m: map[string]string{}}
			var k Keychain
			if withChain {
				k = chain
			}
			defer Use(k, "")()
			be("account", "alice")
			if _, err := Store("ANTHROPIC_API_KEY", "sk-alice-0123456789"); err != nil {
				t.Fatal(err)
			}
			if v, _, _ := Lookup("ANTHROPIC_API_KEY"); v != "sk-alice-0123456789" {
				t.Fatalf("alice lost her key: %q", v)
			}
			be("account", "bob")
			if v, _, _ := Lookup("ANTHROPIC_API_KEY"); v != "" {
				t.Fatalf("bob sees alice's key")
			}
			if from, _ := Remove("ANTHROPIC_API_KEY"); len(from) != 0 {
				t.Fatalf("bob removed alice's key from %v", from)
			}
			be("guest", "")
			if v, _, _ := Lookup("ANTHROPIC_API_KEY"); v != "" {
				t.Fatal("a guest sees a stored key")
			}
			if _, err := Store("ANTHROPIC_API_KEY", key); !errors.Is(err, ErrGuest) {
				t.Fatalf("a guest stored a key: %v", err)
			}
			be("legacy", "")
			if v, _, _ := Lookup("ANTHROPIC_API_KEY"); v != "" {
				t.Fatal("legacy sees an account's key")
			}
			if _, err := Store("ANTHROPIC_API_KEY", "sk-legacy-0123456789"); err != nil {
				t.Fatal(err)
			}
			if withChain && chain.m["ANTHROPIC_API_KEY"] != "sk-legacy-0123456789" {
				t.Fatalf("legacy is not under the old service and name: %v", chain.m)
			}
			be("account", "alice")
			if v, _, _ := Lookup("ANTHROPIC_API_KEY"); v != "sk-alice-0123456789" {
				t.Fatalf("alice sees %q", v)
			}
		})
	}
}

// The first account takes over the Keychain keys from before sign-in.
func TestAdoptLegacyKeys(t *testing.T) {
	be := personHome(t, "alice")
	chain := &fakeChain{m: map[string]string{"ANTHROPIC_API_KEY": "sk-old-0123456789", "CORP_KEY": "corp-old-0123456789"}}
	defer Use(chain, "")()
	moved, err := AdoptLegacy("alice", append(HostedKeyNames(), "CORP_KEY"))
	if err != nil || len(moved) != 2 {
		t.Fatalf("moved %v %v", moved, err)
	}
	if _, left := chain.m["ANTHROPIC_API_KEY"]; left {
		t.Fatal("the legacy item was left")
	}
	be("account", "alice")
	if v, _, _ := Lookup("CORP_KEY"); v != "corp-old-0123456789" {
		t.Fatalf("alice did not get the earlier key: %q", v)
	}
	if _, err := AdoptLegacy("../x", nil); err == nil {
		t.Fatal("a bad account id was taken")
	}
}

// Unmigrate moves the account's Keychain keys back to the service from before sign-in.
func TestReturnLegacyKeys(t *testing.T) {
	be := personHome(t, "alice")
	chain := &fakeChain{m: map[string]string{}}
	defer Use(chain, "")()
	be("account", "alice")
	if _, err := Store("ANTHROPIC_API_KEY", "sk-alice-0123456789"); err != nil {
		t.Fatal(err)
	}
	moved, err := ReturnLegacy("alice", HostedKeyNames())
	if err != nil || len(moved) != 1 {
		t.Fatalf("moved %v %v", moved, err)
	}
	if v, _, _ := Lookup("ANTHROPIC_API_KEY"); v != "" {
		t.Fatal("the account kept the key")
	}
	be("legacy", "")
	if v, _, _ := Lookup("ANTHROPIC_API_KEY"); v != "sk-alice-0123456789" {
		t.Fatalf("legacy does not see the key again: %q", v)
	}
	if _, err := ReturnLegacy("../x", nil); err == nil {
		t.Fatal("a bad account id was taken")
	}
}
