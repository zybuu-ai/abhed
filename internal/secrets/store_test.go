package secrets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreKeepsValuesPrivateAndByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "secrets.json")
	s := Open(path)
	if err := s.Set("GITHUB_TOKEN", "ghp_abc123"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("lowercase", "x"); err == nil {
		t.Fatal("a lower-case name was accepted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("store is mode %o, want 600", info.Mode().Perm())
	}
	names, _ := s.Names()
	if strings.Join(names, ",") != "GITHUB_TOKEN" {
		t.Fatalf("names: %v", names)
	}
	env, err := s.Env([]string{"GITHUB_TOKEN"})
	if err != nil || env[0] != "GITHUB_TOKEN=ghp_abc123" {
		t.Fatalf("env: %v %v", env, err)
	}
	if _, err := s.Env([]string{"MISSING"}); err == nil || !strings.Contains(err.Error(), "abhed secret set MISSING") {
		t.Fatalf("an unknown name must say how to add it: %v", err)
	}
	if err := s.Remove("GITHUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.Names(); len(names) != 0 {
		t.Fatalf("still stored: %v", names)
	}
}

func TestStoreRefusesAFileOthersCanRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"A":"b"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path).Names(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a world-readable store was accepted: %v", err)
	}
}

// Redaction matches the stored values exactly, in the escaped form they take
// inside a JSON payload, longest first.
func TestRedactorReplacesStoredValuesInPayloads(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	_ = s.Set("TOKEN", `tok"en/with\slash`)
	_ = s.Set("PREFIX", "tok")
	redact := s.Redactor().Redact

	payload, _ := json.Marshal(map[string]string{"content": `Authorization: tok"en/with\slash and tok alone`})
	got := string(redact(payload))
	if strings.Contains(got, "with") || !strings.Contains(got, "[secret:TOKEN]") {
		t.Fatalf("the value survived: %s", got)
	}
	if !strings.Contains(got, "[secret:PREFIX] alone") {
		t.Fatalf("the shorter value was not replaced on its own: %s", got)
	}
	var back map[string]string
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("redaction broke the JSON: %v\n%s", err, got)
	}
	if string(Open(filepath.Join(t.TempDir(), "none.json")).Redactor().Redact([]byte(`{"a":1}`))) != `{"a":1}` {
		t.Fatal("an empty store must leave payloads alone")
	}
}

// A value is matched in the decoded text of each string, never across an
// escape, so the redacted payload is still valid JSON and nothing else moves.
func TestRedactorNeverMatchesAcrossAnEscape(t *testing.T) {
	cases := []struct{ value, text, want string }{
		{"003e9a8b7c6d5e", "a>9a8b7c6d5e key 003e9a8b7c6d5e", "a>9a8b7c6d5e key [secret:K]"},
		{"nf00d1e2b3c4", "log:\nf00d1e2b3c4 and key nf00d1e2b3c4", "log:\nf00d1e2b3c4 and key [secret:K]"},
	}
	for _, tc := range cases {
		s := Open(filepath.Join(t.TempDir(), "secrets.json"))
		_ = s.Set("K", tc.value)
		payload, _ := json.Marshal(map[string]any{"content": tc.text, "n": 12345678901234567})
		got := s.Redactor().Redact(payload)
		var back map[string]json.RawMessage
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("invalid JSON: %s", got)
		}
		var content string
		_ = json.Unmarshal(back["content"], &content)
		if content != tc.want || string(back["n"]) != "12345678901234567" {
			t.Fatalf("got %s", got)
		}
	}
}
