package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A server built with no redactor, or a typed nil one, redacts with the
// operator's secrets store rather than recording a stored value as it is.
func TestServerWithNoRedactorUsesTheSecretsStore(t *testing.T) {
	const raw = "fake-server-secret-71b0"
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FAKE_TOKEN":"`+raw+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secrets.EnvFile, path)
	var typedNil *secrets.Redactor
	for name, set := range map[string]func(*Options){
		"nil":       func(o *Options) { o.Redact = nil },
		"typed nil": func(o *Options) { o.Redact = typedNil },
	} {
		wb := shellBenchOpts(t, nil, set)
		start := wb.startShell()
		wb.typeLines(start.ID, enter("echo "+raw, "exit")...)
		deadline := time.Now().Add(3 * time.Second)
		for {
			var all strings.Builder
			closed := false
			for _, e := range wb.events() {
				all.Write(e.Payload)
				closed = closed || e.Type == agent.EvObservation
			}
			if strings.Contains(all.String(), raw) {
				t.Fatalf("%s: a stored value reached the record:\n%s", name, all.String())
			}
			if closed && strings.Contains(all.String(), "[secret:FAKE_TOKEN]") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the shell's line was not recorded redacted:\n%s", name, all.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// With no redactor and a secrets store it cannot load, a server withholds every
// payload rather than record one unredacted.
func TestServerWithAnUnloadableStoreWithholds(t *testing.T) {
	const raw = "fake-server-mode-value-2e9a"
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FAKE_TOKEN":"`+raw+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secrets.EnvFile, path)
	wb := shellBenchOpts(t, nil, func(o *Options) { o.Redact = nil })
	start := wb.startShell()
	wb.typeLines(start.ID, enter("echo "+raw, "exit")...)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var all strings.Builder
		closed := false
		for _, e := range wb.events() {
			all.Write(e.Payload)
			closed = closed || e.Type == agent.EvObservation
		}
		if strings.Contains(all.String(), raw) {
			t.Fatalf("a stored value reached the record:\n%s", all.String())
		}
		if closed && strings.Contains(all.String(), agent.Withheld) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell's record was not withheld:\n%s", all.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A secret stored while the server runs is redacted from the next session on:
// the store is read again whenever a session starts.
func TestServerReadsTheStoreForEachSession(t *testing.T) {
	const later = "fake-added-later-4c7d"
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FIRST_TOKEN":"fake-first-value-11aa"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secrets.EnvFile, path)
	for name, red := range map[string]agent.Redactor{"nil": nil, "live": secrets.Open(path).Live()} {
		wb := shellBenchOpts(t, nil, func(o *Options) { o.Redact = red })
		if err := secrets.Open(path).Set("LATER_TOKEN", later); err != nil {
			t.Fatal(err)
		}
		wb.session = freshSession(wb)
		start := wb.startShell()
		wb.typeLines(start.ID, enter("echo "+later, "exit")...)
		deadline := time.Now().Add(3 * time.Second)
		for {
			var all strings.Builder
			closed := false
			for _, e := range wb.events() {
				all.Write(e.Payload)
				closed = closed || e.Type == agent.EvObservation
			}
			if strings.Contains(all.String(), later) {
				t.Fatalf("%s: a secret stored while the server ran reached a new session's record:\n%s", name, all.String())
			}
			if closed && strings.Contains(all.String(), "[secret:LATER_TOKEN]") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the new session's line was not recorded redacted:\n%s", name, all.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := secrets.Open(path).Remove("LATER_TOKEN"); err != nil {
			t.Fatal(err)
		}
	}
}

// A store that breaks while the server runs withholds the next session's
// payloads rather than record a value it can no longer redact.
func TestServerWithholdsWhenTheStoreBreaksMidRun(t *testing.T) {
	const later = "fake-broken-later-8e3f"
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FIRST_TOKEN":"fake-first-value-22bb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secrets.EnvFile, path)
	wb := shellBenchOpts(t, nil, func(o *Options) { o.Redact = secrets.Open(path).Live() })
	if err := secrets.Open(path).Set("LATER_TOKEN", later); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	wb.session = freshSession(wb)
	start := wb.startShell()
	wb.typeLines(start.ID, enter("echo "+later, "exit")...)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var all strings.Builder
		closed := false
		for _, e := range wb.events() {
			all.Write(e.Payload)
			closed = closed || e.Type == agent.EvObservation
		}
		if strings.Contains(all.String(), later) {
			t.Fatalf("a value the broken store holds reached the new session's record:\n%s", all.String())
		}
		if closed && strings.Contains(all.String(), agent.Withheld) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new session's record was not withheld:\n%s", all.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// freshSession opens another workbench session, so its recorder starts now.
func freshSession(wb *workbench) string {
	wb.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"workbench":true}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		wb.t.Fatalf("open a workbench session: %d %s", rec.Code, rec.Body)
	}
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	return created.SessionID
}

// A server session redacts a secret stored, or changed, after it started, as
// bash can be handed it at any call; a removed value stays redacted.
func TestServerSessionFollowsTheSecretsStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FIRST_TOKEN":"fake-first-value-11aa"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{opts: Options{Redact: secrets.Open(path).Live()}}
	red := s.sessionRedactor()
	for _, v := range []string{"fake-added-mid-session-5e1f", "fake-changed-mid-session-77c0"} {
		if err := secrets.Open(path).Set("LATE_TOKEN", v); err != nil {
			t.Fatal(err)
		}
		if out := string(red.Redact([]byte(`{"x":"` + v + `"}`))); strings.Contains(out, v) {
			t.Fatalf("a value stored mid-session was recorded: %s", out)
		}
	}
	if err := secrets.Open(path).Remove("LATE_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if out := string(red.Redact([]byte(`{"x":"fake-added-mid-session-5e1f"}`))); strings.Contains(out, "fake-added") {
		t.Fatalf("a removed value was recorded: %s", out)
	}
}

// A session started while the operator's store is broken withholds until the
// store loads again, then redacts, rather than withholding for good; while it
// is broken, /v1/health says so.
func TestBrokenOperatorStoreRecovers(t *testing.T) {
	const value = "fake-recover-value-51ac"
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := secrets.Open(path).Set("RECOVER_TOKEN", value); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secrets.EnvFile, path)
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Redact: secrets.Open(path).Live()})
	health := func() map[string]any {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	red := s.sessionRedactor()
	if out := red.Redact([]byte(`"` + value + `"`)); out != nil {
		t.Fatalf("a broken store did not withhold: %s", out)
	}
	if h := health(); h["status"] != "degraded" || h["secrets_store"] != "unreadable" {
		t.Fatalf("health with a broken store: %v", h)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := string(red.Redact([]byte(`"` + value + `"`))); out != `"[secret:RECOVER_TOKEN]"` {
		t.Fatalf("the session still withholds after the store was fixed: %s", out)
	}
	if h := health(); h["status"] != "ok" || h["secrets_store"] != nil {
		t.Fatalf("health with the store fixed: %v", h)
	}
}
