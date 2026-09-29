package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/secrets"
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
		wb.session = wb.openIdle("acme")
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
