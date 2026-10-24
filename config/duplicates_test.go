package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDuplicateKeysAreFoundAtAnyDepth(t *testing.T) {
	data := []byte(`{"model":{"default":"gemini","providers":{"gemini":{"model":"a"},"gemini":{"model":"b"},"Gemini":{"model":"c"}}},
		"permissions":{"deny":["x"],"deny":["y"]},"mcp":{"servers":[{"name":"a","name":"b"}]}}`)
	got := duplicateKeys("/h/.abhed/config.json", data)
	var lines []string
	for _, d := range got {
		lines = append(lines, d.String())
	}
	want := []string{
		`/h/.abhed/config.json: the key "gemini" appears more than once in model.providers; only the last one is used`,
		`/h/.abhed/config.json: the key "deny" appears more than once in permissions; only the last one is used`,
		`/h/.abhed/config.json: the key "name" appears more than once in mcp.servers; only the last one is used`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// A setting written twice in different case is one setting; a provider name is not.
	if d := duplicateKeys("f", []byte(`{"permissions":{"deny":["a"]},"PERMISSIONS":{"ask":["b"]}}`)); len(d) != 1 || d[0].Other != "permissions" {
		t.Fatalf("case variants of a setting: %+v", d)
	}
	if d := duplicateKeys("f", []byte(`{"a":{"b":1},"c":[{"b":1},{"b":2}]}`)); d != nil {
		t.Fatalf("no duplicates, got %v", d)
	}
	if d := duplicateKeys("f", []byte(`{"a":`)); d != nil {
		t.Fatalf("a file that does not parse reports its parse error, not duplicates: %v", d)
	}
}

// Loading warns of a repeated provider in the person's own file, and the
// configuration keeps it for the doctor and editors.
func TestLoadWarnsOfARepeatedProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(home, ".abhed", "config.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	body := `{"model":{"default":"g","providers":{"g":{"type":"openai-compatible","base_url":"http://127.0.0.9:1/v1","model":"a"},"g":{"type":"openai-compatible","base_url":"http://127.0.0.9:1/v1","model":"b"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned []string
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(s string) { warned = append(warned, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Duplicates) != 1 || cfg.Duplicates[0].Key != "g" {
		t.Fatalf("duplicates %+v", cfg.Duplicates)
	}
	if !strings.Contains(strings.Join(warned, "\n"), `the key "g" appears more than once in model.providers`) {
		t.Fatalf("no warning: %q", warned)
	}
	if w := cfg.Warnings(); len(w) == 0 || !strings.Contains(w[0], "appears more than once") {
		t.Fatalf("Warnings() = %q", w)
	}
}
