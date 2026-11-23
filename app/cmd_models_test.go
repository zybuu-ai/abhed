package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/modelkeys"
)

type memKeychain map[string]string

func keySlot(s, n string) string {
	if s == modelkeys.Service {
		return n
	}
	return s + "|" + n
}

func (m memKeychain) Get(s, n string) (string, bool, error) {
	v, ok := m[keySlot(s, n)]
	return v, ok, nil
}
func (m memKeychain) Set(s, n, v string) error { m[keySlot(s, n)] = v; return nil }
func (m memKeychain) Delete(s, n string) (bool, error) {
	_, ok := m[keySlot(s, n)]
	delete(m, keySlot(s, n))
	return ok, nil
}

const testKey = "sk-ant-api03-secretvalue-0123456789"

// modelsHome is a throwaway home with the person's file holding body.
func modelsHome(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if body != "" {
		dir := filepath.Join(home, ".abhed")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(home, ".abhed", "config.json")
}

func runModels(t *testing.T, in string, terminal bool, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := modelsCmd(args, modelsIO{in: strings.NewReader(in), out: &out, err: &errb, terminal: terminal})
	return code, out.String(), errb.String()
}

func TestModelsAddKeepsTheRestOfTheFile(t *testing.T) {
	orig := `{"future_key": {"x": 1}, "limits": {"max_turns": 12345678901234567}, "model": {"default": "local", "providers": {"local": {"type": "ollama", "model": "qwen3:30b", "context_window": 65536}}}}`
	file := modelsHome(t, orig, 0o640)
	code, out, errs := runModels(t, "", false, "add", "-json", "--", "anthropic", "claude-sonnet-5-5", "claude-opus-5-5")
	if code != 0 {
		t.Fatalf("add: %d %s", code, errs)
	}
	var res modelsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Added) != 2 {
		t.Fatalf("result %s: %v", out, err)
	}
	data, _ := os.ReadFile(file)
	s := string(data)
	for _, want := range []string{`"future_key"`, `12345678901234567`, `"claude-opus-5-5"`, `"qwen3:30b"`} {
		if !strings.Contains(s, want) {
			t.Errorf("the file lost %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "api_key\"") {
		t.Error("a key field was written")
	}
	fi, _ := os.Stat(file)
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640 kept", fi.Mode().Perm())
	}
	bak, err := os.ReadFile(file + ".bak")
	if err != nil || string(bak) != orig {
		t.Errorf("backup = %q, %v", bak, err)
	}
	// No temporary file is left beside it.
	ents, _ := os.ReadDir(filepath.Dir(file))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left %s", e.Name())
		}
	}
	// Adding again is no change; a clash under another type is refused.
	if code, out, _ := runModels(t, "", false, "add", "anthropic", "claude-opus-5-5"); code != 0 || !strings.Contains(out, "already configured") {
		t.Errorf("re-add: %d %s", code, out)
	}
	if code, _, _ := runModels(t, "", false, "add", "openai", "claude-opus-5-5"); code == 0 {
		t.Error("a clashing name was replaced")
	}
}

func TestModelsAddFromConfiguredShareKeyName(t *testing.T) {
	file := modelsHome(t, `{"model":{"providers":{"corp":{"type":"openai","base_url":"https://llm.corp.example/v1","model":"a","api_key_secret":"CORP_KEY","api_key":"literal-should-not-copy"}}}}`, 0o600)
	if code, _, errs := runModels(t, "", false, "add", "corp", "b-model"); code != 0 {
		t.Fatal(errs)
	}
	doc, _, _ := readUserDoc()
	e := userProviders(doc)["b-model"].(map[string]any)
	if e["base_url"] != "https://llm.corp.example/v1" || e["api_key_secret"] != "CORP_KEY" || e["api_key"] != nil {
		t.Fatalf("entry %v", e)
	}
	_ = file
}

func TestModelsDefaultConfirmsHosted(t *testing.T) {
	modelsHome(t, `{"model":{"default":"local","providers":{"local":{"type":"ollama","model":"q"},"c":{"type":"anthropic","model":"claude-sonnet-5-5"},"l2":{"type":"ollama","model":"r"}}}}`, 0o600)
	code, out, _ := runModels(t, "", false, "default", "-check", "-json", "--", "c")
	if code != 0 || !strings.Contains(out, `"widens":true`) {
		t.Fatalf("check: %d %s", code, out)
	}
	// No terminal, no yes: refused, nothing written.
	if code, _, _ := runModels(t, "", false, "default", "c"); code == 0 {
		t.Fatal("a hosted default was set with no confirmation")
	}
	if code, _, _ := runModels(t, "n\n", true, "default", "c"); code == 0 {
		t.Fatal("a no set the default")
	}
	own, _ := userFileConfig()
	if own.Model.Default != "local" {
		t.Fatalf("default changed to %s", own.Model.Default)
	}
	if code, _, errs := runModels(t, "y\n", true, "default", "c"); code != 0 {
		t.Fatal(errs)
	}
	// A local one narrows: no question.
	if code, _, errs := runModels(t, "", false, "default", "l2"); code != 0 {
		t.Fatal(errs)
	}
	own, _ = userFileConfig()
	if own.Model.Default != "l2" {
		t.Fatalf("default = %s", own.Model.Default)
	}
	if code, _, _ := runModels(t, "", false, "remove", "l2"); code == 0 {
		t.Fatal("the default was removed")
	}
	if code, _, errs := runModels(t, "", false, "remove", "c"); code != 0 {
		t.Fatal(errs)
	}
}

func TestModelsRefusesManaged(t *testing.T) {
	modelsHome(t, "", 0)
	managedConfig(t, `{"model":{"default":"corp","providers":{"corp":{"type":"anthropic","model":"claude-sonnet-5-5"}},"allowed":["anthropic/claude-sonnet-5-5"]}}`)
	if code, _, errs := runModels(t, "y\n", true, "default", "corp"); code == 0 || !strings.Contains(errs, "managed") {
		t.Fatalf("managed default: %d %s", code, errs)
	}
	if code, _, _ := runModels(t, "", false, "add", "anthropic", "claude-opus-5-5"); code == 0 {
		t.Fatal("a model the managed list refuses was added")
	}
}

func TestModelsParseFlags(t *testing.T) {
	f, err := parseModelsFlags([]string{"-confirm-fd", "3", "-json", "--", "-name-like-id"})
	if err != nil || f.confirmFD != 3 || !f.json || len(f.words) != 1 || f.words[0] != "-name-like-id" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := parseModelsFlags([]string{"-confirm-fd", "1"}); err == nil {
		t.Fatal("a descriptor under 3 was taken")
	}
}

func TestProviderLoginKeepsKeyOutOfConfigAndOutput(t *testing.T) {
	file := modelsHome(t, `{"model":{"default":"c","providers":{"c":{"type":"anthropic","model":"claude-sonnet-5-5"}}}}`, 0o600)
	chain := memKeychain{}
	defer modelkeys.Use(chain, filepath.Join(filepath.Dir(file), "secrets.json"))()
	var out, errb bytes.Buffer
	pio := providerIO{out: &out, err: &errb, readKey: func(string) (string, error) { return testKey, nil }}
	if code := providerCmd([]string{"login", "--", "anthropic"}, pio); code != 0 {
		t.Fatalf("login: %s", errb.String())
	}
	if chain["ANTHROPIC_API_KEY"] != testKey {
		t.Fatal("the key was not stored in the Keychain")
	}
	cfgData, _ := os.ReadFile(file)
	changes, _ := os.ReadFile(filepath.Join(filepath.Dir(file), "config-changes.jsonl"))
	var status bytes.Buffer
	if err := providerStatus(true, &status); err != nil {
		t.Fatal(err)
	}
	cfg, _ := userFileConfig()
	p, err := cfg.ProviderNamed("c")
	if err != nil || p.APIKey != testKey {
		t.Fatalf("resolve: %v", err)
	}
	_, detail, _ := modelKeyCheck(p)
	for name, text := range map[string]string{"config": string(cfgData), "change log": string(changes), "login output": out.String() + errb.String(),
		"status": status.String(), "doctor": detail} {
		if strings.Contains(text, testKey) {
			t.Errorf("the key is in the %s: %s", name, text)
		}
	}
	if !strings.Contains(detail, "Keychain") {
		t.Errorf("doctor does not say the source: %s", detail)
	}
	// Without a terminal nothing is read.
	if code := providerCmd([]string{"login", "anthropic"}, providerIO{out: &out, err: &errb}); code == 0 {
		t.Fatal("a key was taken with no terminal")
	}
}

func TestKeyMismatchHintOnlyOnRefusal(t *testing.T) {
	defer modelkeys.Use(memKeychain{"ANTHROPIC_API_KEY": "sk-stored-0123456789"}, filepath.Join(t.TempDir(), "s.json"))()
	t.Setenv("ANTHROPIC_API_KEY", "sk-stale-0123456789")
	p := config.ResolveKey(config.ProviderConfig{Type: "anthropic", Model: "m"})
	credit := &model.StatusError{Status: 400, Body: `{"error":{"message":"Your credit balance is too low to access the Anthropic API."}}`}
	if h := keyMismatchHint(credit, p); !strings.Contains(h, "unset ANTHROPIC_API_KEY") {
		t.Fatalf("credit hint = %q", h)
	}
	if h := keyMismatchHint(&model.StatusError{Status: 401}, p); h == "" {
		t.Fatal("no hint on 401")
	}
	for _, err := range []error{&model.StatusError{Status: 500}, &model.StatusError{Status: 400, Body: "bad tool"}, errors.New("dial")} {
		if h := keyMismatchHint(err, p); h != "" {
			t.Errorf("hint on %v: %s", err, h)
		}
	}
	if strings.Contains(keyMismatchHint(credit, p), "sk-") {
		t.Error("the hint shows a key")
	}
	// Same key in both places: no mismatch.
	t.Setenv("ANTHROPIC_API_KEY", "sk-stored-0123456789")
	if h := keyMismatchHint(credit, config.ResolveKey(config.ProviderConfig{Type: "anthropic", Model: "m"})); h != "" {
		t.Errorf("hint with the same key: %s", h)
	}
}

// A file with something after its object is refused, not cut short by a write.
func TestModelsRefusesTrailingContent(t *testing.T) {
	file := modelsHome(t, `{"model":{}} {"extra":1}`, 0o600)
	if code, _, errs := runModels(t, "", false, "add", "anthropic", "claude-opus-5-5"); code == 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if _, _, err := readUserDoc(); err == nil || !strings.Contains(err.Error(), "more than one JSON value") {
		t.Fatalf("readUserDoc took it: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != `{"model":{}} {"extra":1}` {
		t.Fatalf("the file was changed: %s", b)
	}
}

// A custom model's key is stored under its own name, as Studio passes it.
func TestProviderLoginByKeyName(t *testing.T) {
	file := modelsHome(t, `{"model":{"providers":{"corp":{"type":"openai","base_url":"https://llm.corp.example/v1","model":"c","api_key_secret":"CORP_KEY"}}}}`, 0o600)
	chain := memKeychain{}
	defer modelkeys.Use(chain, filepath.Join(filepath.Dir(file), "secrets.json"))()
	var out, errb bytes.Buffer
	pio := providerIO{out: &out, err: &errb, readKey: func(string) (string, error) { return testKey, nil }}
	for _, arg := range []string{"CORP_KEY", "corp"} {
		if code := providerCmd([]string{"login", "--", arg}, pio); code != 0 {
			t.Fatalf("%s: %s", arg, errb.String())
		}
		if chain["CORP_KEY"] != testKey || chain["OPENAI_API_KEY"] != "" {
			t.Fatalf("%s stored under %v", arg, chain)
		}
		delete(chain, "CORP_KEY")
	}
	if code := providerCmd([]string{"login", "NOBODY_KEY"}, pio); code == 0 {
		t.Fatal("a key no model reads was stored")
	}
}

// A guest keeps nothing: no model added or chosen, no key stored or removed,
// by the command line or by any other path to the store.
func TestGuestKeepsNoModelsOrKeys(t *testing.T) {
	file := modelsHome(t, `{"model":{"default":"local","providers":{"local":{"type":"ollama","model":"q"},"c":{"type":"anthropic","model":"claude-sonnet-5-5"}}}}`, 0o600)
	chain := memKeychain{"ANTHROPIC_API_KEY": "sk-kept-0123456789"}
	defer modelkeys.Use(chain, filepath.Join(filepath.Dir(file), "secrets.json"))()
	endGuest()
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endGuest)
	for _, args := range [][]string{{"provider", "login", "anthropic"}, {"provider", "logout", "anthropic"},
		{"models", "add", "anthropic", "x"}, {"models", "default", "c"}, {"models", "remove", "c"}, {"models", "rm", "c"},
		{"models", "forget-window", "c"}} {
		if err := guestSubcommand(args); err == nil {
			t.Errorf("a guest may %v", args)
		}
	}
	for _, args := range [][]string{{"provider", "status"}, {"models", "list"}, {"models", "known"}} {
		if err := guestSubcommand(args); err != nil {
			t.Errorf("a guest is refused %v: %v", args, err)
		}
	}
	before, _ := os.ReadFile(file)
	if code, _, _ := runModels(t, "", false, "add", "anthropic", "claude-opus-5-5"); code == 0 {
		t.Error("a guest added a model")
	}
	if code, _, _ := runModels(t, "", false, "remove", "c"); code == 0 {
		t.Error("a guest removed a model")
	}
	if code, _, _ := runModels(t, "y\n", true, "default", "c"); code == 0 {
		t.Error("a guest set the default")
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Error("a guest changed the configuration")
	}
	// The store itself refuses, as Studio's key terminal reaches it.
	if _, err := modelkeys.Store("ANTHROPIC_API_KEY", testKey); err == nil {
		t.Error("a guest stored a key")
	}
	if _, err := modelkeys.Remove("ANTHROPIC_API_KEY"); err == nil {
		t.Error("a guest removed a key")
	}
	var out, errb bytes.Buffer
	pio := providerIO{out: &out, err: &errb, readKey: func(string) (string, error) { return testKey, nil }}
	if code := providerCmd([]string{"login", "anthropic"}, pio); code == 0 || chain["ANTHROPIC_API_KEY"] != "sk-kept-0123456789" {
		t.Errorf("a guest's login stored a key: %d %s", code, errb.String())
	}
}

// From the command line, a guest is refused before anything runs.
func TestGuestRefusedModelCommandsFromMain(t *testing.T) {
	modelsHome(t, `{"model":{"default":"local","providers":{"local":{"type":"ollama","model":"q"}}}}`, 0o600)
	t.Setenv(EnvGuest, "1")
	t.Cleanup(endGuest)
	for _, args := range [][]string{{"provider", "logout", "anthropic"}, {"models", "add", "anthropic", "x"}} {
		endGuest()
		t.Setenv(EnvGuest, "1") // handDown unsets it once read
		out, code := stderrOf(t, args)
		if code == 0 || !strings.Contains(out, "a guest keeps nothing") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
}

// A guest is given no stored key, the machine's from before sign-in
// included; a key in the environment still works.
func TestGuestGetsNoStoredKey(t *testing.T) {
	modelsHome(t, "", 0)
	chain := memKeychain{"ANTHROPIC_API_KEY": "sk-legacy-0123456789"}
	defer modelkeys.Use(chain, "")()
	if p := config.ResolveKey(config.ProviderConfig{Type: "anthropic", Model: "m"}); p.APIKey != "sk-legacy-0123456789" {
		t.Fatalf("legacy lost its key: %+v", p.KeySource)
	}
	endGuest()
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endGuest)
	if p := config.ResolveKey(config.ProviderConfig{Type: "anthropic", Model: "m"}); p.APIKey != "" {
		t.Fatal("a guest was given a stored key")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-0123456789")
	if p := config.ResolveKey(config.ProviderConfig{Type: "anthropic", Model: "m"}); p.APIKey != "sk-env-0123456789" {
		t.Fatalf("a guest lost the environment's key: %+v", p.KeySource)
	}
}
