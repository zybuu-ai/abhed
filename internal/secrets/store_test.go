package secrets

import (
	"encoding/json"
	"fmt"
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

// A store that exists but cannot be loaded refuses a session, naming the file
// and the fix; a missing store is empty. Redactor then withholds everything.
func TestUnloadableStoreFailsClosed(t *testing.T) {
	dir := t.TempDir()
	missing := Open(filepath.Join(dir, "none.json"))
	if r, err := missing.LoadRedactor(); err != nil || r.Span() != 0 {
		t.Fatalf("a missing store must be empty, not an error: %v", err)
	}
	cases := map[string]func(string) error{
		"corrupt": func(p string) error { return os.WriteFile(p, []byte(`{"FAKE_TOKEN": `), 0o600) },
		"wrong mode": func(p string) error {
			if err := os.WriteFile(p, []byte(`{"FAKE_TOKEN":"fake-mode-value"}`), 0o600); err != nil {
				return err
			}
			return os.Chmod(p, 0o644)
		},
	}
	for name, make := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := make(path); err != nil {
			t.Fatal(err)
		}
		s := Open(path)
		_, err := s.LoadRedactor()
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "refusing to start") ||
			!strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("%s: want a refusal naming %s and the fix, got %v", name, path, err)
		}
		if out := s.Redactor().Redact([]byte(`{"a":"fake-mode-value"}`)); json.Valid(out) {
			t.Fatalf("%s: an unloadable store's redactor let a payload through: %s", name, out)
		}
	}
}

// An empty file, a directory and an oversized file are refused, each naming the
// store once.
func TestStoreShapesThatAreRefused(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(dir, "folder.json")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, MaxFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{empty: "empty (0 bytes)", folder: "not a regular file", big: "over the"} {
		_, err := Open(path).LoadRedactor()
		if err == nil || !strings.Contains(err.Error(), want) || strings.Count(err.Error(), path) != 1 {
			t.Fatalf("%s: want %q naming the file once, got %v", path, want, err)
		}
	}
}

// A value shorter than MinLength, stored before the minimum, is redacted in
// values but leaves JSON keys alone; a longer one is redacted in both.
func TestShortValuesLeaveKeysAlone(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := s.Set("SHORT", "type"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("LONG", "fake-long-value-99"); err != nil {
		t.Fatal(err)
	}
	got := string(s.Redactor().Redact([]byte(`{"type": "a type", "fake-long-value-99":"fake-long-value-99"}`)))
	want := `{"type": "a [secret:SHORT]", "[secret:LONG]":"[secret:LONG]"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Short is counted in characters, as `abhed secret set` counts: a value of
// fewer than 8 characters leaves keys alone however many bytes it takes.
func TestShortIsCountedInCharacters(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := s.Set("WIDE", "ключ"); err != nil { // 4 characters, 8 bytes
		t.Fatal(err)
	}
	got := string(s.Redactor().Redact([]byte(`{"ключ":"ключ"}`)))
	if got != `{"ключ":"[secret:WIDE]"}` {
		t.Fatalf("got %s", got)
	}
}

// Value is what a tool that authenticates itself reads: one name, and an
// unknown one is an error rather than an empty credential.
func TestValueReadsOneSecretByName(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := s.Set("OCP_TOKEN", "sha256~with=equals"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Value("OCP_TOKEN"); err != nil || v != "sha256~with=equals" {
		t.Fatalf("Value = %q, %v", v, err)
	}
	if _, err := s.Value("MISSING"); err == nil {
		t.Fatal("an unknown name gave a value")
	}
	if !ValidName("OCP_TOKEN") || ValidName("sha256~abc") {
		t.Fatal("ValidName does not tell a name from a token")
	}
}

// Fresh follows the store: a value added is redacted from then on, and a store
// that stops loading withholds every payload until it loads again.
func TestFreshFollowsTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	s := Open(path)
	first, err := s.LoadRedactor()
	if err != nil {
		t.Fatal(err)
	}
	f := s.Fresh(first)
	if got := string(f.Redact([]byte(`"fresh-value-123"`))); got != `"fresh-value-123"` {
		t.Fatalf("an empty store redacted: %s", got)
	}
	if err := s.Set("LATE", "fresh-value-123"); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Redact([]byte(`"fresh-value-123"`))); got != `"[secret:LATE]"` {
		t.Fatalf("a value added later was not redacted: %s", got)
	}
	if err := os.WriteFile(path, []byte(`{"LATE": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.Redact([]byte(`"fresh-value-123"`)); got != nil {
		t.Fatalf("a store that stopped loading did not withhold: %s", got)
	}
}

// Fresh sees a rewrite that keeps the size and the modification time, keeps
// redacting a value rotated out during the session, and withholds for as long
// as the store cannot be loaded.
func TestFreshSeesEveryRewriteAndForgetsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"TOKEN":"first-value-aaaa"}`)
	s := Open(path)
	first, err := s.LoadRedactor()
	if err != nil {
		t.Fatal(err)
	}
	f := s.Fresh(first)
	fi, _ := os.Stat(path)
	// Same size, mtime put back: only the inode's change time moves.
	write(`{"TOKEN":"other-value-bbbb"}`)
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Redact([]byte(`"other-value-bbbb"`))); got != `"[secret:TOKEN]"` {
		t.Fatalf("a same-size rewrite with the old mtime was not seen: %s", got)
	}
	if got := string(f.Redact([]byte(`"first-value-aaaa"`))); got != `"[secret:TOKEN]"` {
		t.Fatalf("a value rotated out during the session stopped being redacted: %s", got)
	}
	write(`{"TOKEN": `)
	for i := 0; i < 2; i++ {
		if got := f.Redact([]byte(`"x"`)); got != nil {
			t.Fatalf("call %d on a store that cannot be loaded did not withhold: %s", i+1, got)
		}
	}
	write(`{"TOKEN":"third-value-cccc"}`)
	if got := string(f.Redact([]byte(`"first-value-aaaa third-value-cccc"`))); got != `"[secret:TOKEN] [secret:TOKEN]"` {
		t.Fatalf("after the store loaded again: %s", got)
	}
}

// FindSent finds a value however deeply it is percent-encoded, with a
// malformed escape or a literal "100%" elsewhere in the text.
func TestFindSentDecodesLeniently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"TOKEN":"Sent-Value 7c2d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path).LoadRedactor()
	if err != nil {
		t.Fatal(err)
	}
	enc := func(s string, n int) string {
		for i := 0; i < n; i++ {
			var b strings.Builder
			for _, c := range []byte(s) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
			s = b.String()
		}
		return s
	}
	for _, text := range []string{
		"100% " + enc("Sent-Value 7c2d", 1),
		"%zz" + enc("Sent-Value 7c2d", 1),
		"q=" + enc("Sent-Value 7c2d", 6),
		"Sent-Value+7c2d",
		"sent-value%207C2D",
	} {
		if label, found := r.FindSent(text); !found || label != "[secret:TOKEN]" {
			t.Errorf("%q: not found", text)
		}
	}
	if _, found := r.FindSent("100% of the tests pass %zz"); found {
		t.Error("ordinary text was taken for the value")
	}
}

// The per-account stores sit beside the operator's unless placed elsewhere.
func TestAccountsDir(t *testing.T) {
	t.Setenv(EnvFile, "/srv/abhed/secrets.json")
	t.Setenv(EnvAccountsDir, "")
	if got, _ := AccountsDir(); got != "/srv/abhed/secrets.d" {
		t.Errorf("beside the store: %q", got)
	}
	t.Setenv(EnvAccountsDir, "/srv/accounts")
	if got, _ := AccountsDir(); got != "/srv/accounts" {
		t.Errorf("overridden: %q", got)
	}
}

// A server opens a store per request: writes through separate Stores on one
// path must neither lose one another nor leave a torn file.
func TestConcurrentWritersOnOnePathKeepEveryValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	const n = 24
	errs := make(chan error, n)
	for i := range n {
		go func() {
			errs <- Open(path).Set(fmt.Sprintf("KEY_%02d", i), fmt.Sprintf("value-%02d-abcdef", i))
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	names, err := Open(path).Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != n {
		t.Errorf("%d of %d values survived: %v", len(names), n, names)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*tmp*"))
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

// A joined redactor redacts every part's values, the longest first, and
// withholds everything while any part cannot be loaded.
func TestJoinedRedactsEveryPartAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	own, op := Open(filepath.Join(dir, "own.json")), Open(filepath.Join(dir, "op.json"))
	if err := own.Set("OWN", "own-value-123"); err != nil {
		t.Fatal(err)
	}
	if err := op.Set("OP", "own-value-123-and-more"); err != nil {
		t.Fatal(err)
	}
	j := Joined{own.Session(), op.Session()}
	got := string(j.Redact([]byte(`"own-value-123 own-value-123-and-more"`)))
	if got != `"[secret:OWN] [secret:OP]"` {
		t.Errorf("joined redaction: %s", got)
	}
	if names := strings.Join(j.Names(), ","); !strings.Contains(names, "OWN") || !strings.Contains(names, "OP") {
		t.Errorf("names: %s", names)
	}
	if err := os.WriteFile(op.Path(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := string(j.Redact([]byte(`"own-value-123"`))); strings.Contains(got, "own-value") {
		t.Errorf("a part that cannot be loaded did not withhold: %s", got)
	}
}
