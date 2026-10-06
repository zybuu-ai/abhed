//go:build unix

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/secretstore"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/server/servertest"
	"github.com/zybuu-ai/abhed/store"
)

// Values a command prints from its first character on: not the stored value,
// so the record keeps them and the test can see whose store a command read.
const (
	operatorToken = "operator-value-1d9c4e"
	aliceToken    = "alice-value-7f2b80"
)

func writeStore(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// secretsServer is a proxy-authenticated server whose agent, asked "use NAME",
// runs a command given secret NAME that prints its value less the first
// character. The operator's store holds TEAM_TOKEN; adjust may scope stores.
func secretsServer(t *testing.T, adjust func(*server.Options)) (http.Handler, *servertest.Events) {
	t.Helper()
	dir := t.TempDir()
	operator := filepath.Join(dir, "operator.json")
	writeStore(t, operator, `{"TEAM_TOKEN":"`+operatorToken+`"}`)
	model := &servertest.Scripted{Reply: func(first, _ string, fromTool bool) servertest.Turn {
		name, ok := strings.CutPrefix(first, "use ")
		if !ok || fromTool {
			return servertest.Turn{Text: "ok"}
		}
		return servertest.Turn{Tool: "bash", Args: map[string]any{
			"command": `printf '%s' "${` + name + `:1}"`, "description": "print it", "secrets": []string{name}}}
	}}
	events := &servertest.Events{}
	s := servertest.Proxy(t, servertest.WithShellAndWrite, servertest.WithSecrets(operator), events.Tap,
		func(o *server.Options) {
			o.Adapter = model
			o.Config.Permissions.Mode = "bypass"
			o.Config.Permissions.Allow = []string{"secret(TEAM_TOKEN)"}
			if adjust != nil {
				adjust(o)
			}
		})
	return s.Handler(), events
}

// run has who ask the agent to use TEAM_TOKEN and returns what the record holds.
func run(t *testing.T, h http.Handler, events *servertest.Events, who string) string {
	t.Helper()
	return ended(t, events, who, create(t, h, who, `{"prompt":"use TEAM_TOKEN"}`))
}

// create starts a session as who and returns its id.
func create(t *testing.T, h http.Handler, who, body string) string {
	t.Helper()
	rec := post(h, who, "/v1/sessions", body)
	var out struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.SessionID == "" {
		t.Fatalf("%s: create: %d %s", who, rec.Code, rec.Body)
	}
	return out.SessionID
}

func post(h http.Handler, who, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("X-Abhed-User", who)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ended waits for the session's run to end here and returns what its record holds.
func ended(t *testing.T, events *servertest.Events, who, id string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for events.Count(id, "session.ended") == 0 {
		if time.Now().After(deadline) {
			var types []string
			for _, e := range events.Of(id) {
				types = append(types, e.Type+" "+string(e.Payload))
			}
			t.Fatalf("%s: the session did not end:\n%s", who, strings.Join(types, "\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	var all strings.Builder
	for _, e := range events.Of(id) {
		all.Write(e.Payload)
	}
	return all.String()
}

// With no SecretsFor, every caller's sessions read the operator's store: the
// single-user default, and what a server shared by several accounts must not keep.
func TestSessionsShareTheOperatorStoreByDefault(t *testing.T) {
	h, events := secretsServer(t, nil)
	for _, who := range []string{"alice", "bob"} {
		if got := run(t, h, events, who); !strings.Contains(got, operatorToken[1:]) {
			t.Errorf("%s's command did not get the operator's value:\n%s", who, got)
		}
	}
}

// With SecretsFor, a session reads only its owner's store: alice's command
// gets her value, and bob's is told there is no such secret, though alice has
// one and the operator has one under the same name.
func TestSecretsForScopesEachSessionToItsOwner(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, filepath.Join(dir, "alice.json"), `{"TEAM_TOKEN":"`+aliceToken+`"}`)
	var asked []string
	h, events := secretsServer(t, func(o *server.Options) {
		o.SecretsFor = func(tenant, user string) (*secretstore.Store, error) {
			asked = append(asked, tenant+"/"+user)
			return secretstore.Open(filepath.Join(dir, user+".json")), nil
		}
	})
	alice := run(t, h, events, "alice")
	if !strings.Contains(alice, aliceToken[1:]) {
		t.Errorf("alice's command did not get her own value:\n%s", alice)
	}
	bob := run(t, h, events, "bob")
	if strings.Contains(alice, operatorToken[1:]) {
		t.Errorf("alice's command got the operator's value:\n%s", alice)
	}
	for _, v := range []string{aliceToken[1:], operatorToken[1:]} {
		if strings.Contains(bob, v) {
			t.Errorf("bob's command got a value that is not his (%s):\n%s", v, bob)
		}
	}
	if !strings.Contains(bob, "no secret named TEAM_TOKEN") {
		t.Errorf("bob's command was not refused for want of his own secret:\n%s", bob)
	}
	if strings.Join(asked, ",") != "default/alice,default/bob" {
		t.Errorf("SecretsFor was asked for %v", asked)
	}
}

// An account whose store cannot be given, or cannot be loaded, starts no
// session: its values could not be redacted.
func TestSecretsForRefusesWhatItCannotLoad(t *testing.T) {
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "carol.json")
	writeStore(t, unreadable, "not json")
	h, _ := secretsServer(t, func(o *server.Options) {
		o.SecretsFor = func(_, user string) (*secretstore.Store, error) {
			switch user {
			case "carol":
				return secretstore.Open(unreadable), nil
			case "dave":
				return nil, nil
			}
			return nil, errors.New("down")
		}
	})
	for _, who := range []string{"carol", "dave", "erin"} {
		req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"use TEAM_TOKEN"}`))
		req.Header.Set("X-Abhed-User", who)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("%s: a session started without a loadable store: %d %s", who, rec.Code, rec.Body)
		}
	}
}

// ownStores is SecretsFor over one file per user in dir.
func ownStores(dir string) func(string, string) (*secretstore.Store, error) {
	return func(_, user string) (*secretstore.Store, error) {
		return secretstore.Open(filepath.Join(dir, user+".json")), nil
	}
}

// A workbench session, opened with no prompt and given one later, reads its
// owner's store as a prompted session does.
func TestSecretsForScopesAWorkbenchSession(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, filepath.Join(dir, "alice.json"), `{"TEAM_TOKEN":"`+aliceToken+`"}`)
	h, events := secretsServer(t, func(o *server.Options) { o.SecretsFor = ownStores(dir) })
	id := create(t, h, "alice", `{"workbench":true}`)
	if r := post(h, "alice", "/v1/sessions/"+id+"/messages", `{"prompt":"use TEAM_TOKEN"}`); r.Code != http.StatusAccepted {
		t.Fatalf("prompt the workbench: %d %s", r.Code, r.Body)
	}
	got := ended(t, events, "alice", id)
	if !strings.Contains(got, aliceToken[1:]) || strings.Contains(got, operatorToken[1:]) {
		t.Errorf("the workbench session did not read alice's own store:\n%s", got)
	}
}

// rowsMem is the memory store with session rows, so a second server can find
// a session the first one ran and continue it from the record.
type rowsMem struct {
	*agent.MemStore
	mu   sync.Mutex
	rows []store.SessionRecord
}

func (m *rowsMem) CreateSession(_ context.Context, r store.SessionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, r)
	return nil
}

func (m *rowsMem) ListSessions(context.Context, int) ([]store.SessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.SessionRecord(nil), m.rows...), nil
}

// A session continued from its record, here by a server that did not run it,
// reads its owner's store, not the operator's.
func TestSecretsForScopesAResumedSession(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, filepath.Join(dir, "alice.json"), `{"TEAM_TOKEN":"`+aliceToken+`"}`)
	shared := &rowsMem{MemStore: agent.NewMemStore()}
	scoped := func(o *server.Options) { o.SecretsFor = ownStores(dir); o.Store = shared }
	first, firstEvents := secretsServer(t, scoped)
	id := create(t, first, "alice", `{"prompt":"use TEAM_TOKEN"}`)
	ended(t, firstEvents, "alice", id)

	second, events := secretsServer(t, scoped)
	if r := post(second, "alice", "/v1/sessions/"+id+"/messages", `{"prompt":"again"}`); r.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", r.Code, r.Body)
	}
	got := ended(t, events, "alice", id)
	if !strings.Contains(got, aliceToken[1:]) || strings.Contains(got, operatorToken[1:]) {
		t.Errorf("the resumed session did not read alice's own store:\n%s", got)
	}
}

// With SecretsFor, a session's record is redacted with the operator's store as
// well as its owner's: an operator value that reaches it stays redacted.
func TestSecretsForKeepsOperatorValuesRedacted(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, filepath.Join(dir, "alice.json"), `{"OWN_TOKEN":"`+aliceToken+`"}`)
	h, events := secretsServer(t, func(o *server.Options) { o.SecretsFor = ownStores(dir) })
	id := create(t, h, "alice", `{"prompt":"note `+operatorToken+` and `+aliceToken+`"}`)
	got := ended(t, events, "alice", id)
	for _, v := range []string{operatorToken, aliceToken} {
		if strings.Contains(got, v) {
			t.Errorf("the record holds %s unredacted:\n%s", v, got)
		}
	}
	if !strings.Contains(got, "[secret:TEAM_TOKEN]") || !strings.Contains(got, "[secret:OWN_TOKEN]") {
		t.Errorf("the record does not name both redacted values:\n%s", got)
	}
}

// A server several accounts sign in to that gives them all the operator's
// store warns at startup of each allow rule that lets a session name a secret.
func TestSharedSecretRulesWarnAtStartup(t *testing.T) {
	for _, c := range []struct {
		name, mode string
		allow      []string
		scoped     bool
		want       bool
	}{
		{"local auth and a secret rule", "local", []string{"bash(ls*)", "secret(GITHUB_TOKEN)"}, false, true},
		{"proxy auth and every tool", "proxy", []string{"*"}, false, true},
		{"no auth", "none", []string{"secret(GITHUB_TOKEN)"}, false, false},
		{"no secret rule", "local", []string{"bash(ls*)"}, false, false},
		{"a store per account", "oidc", []string{"secret(GITHUB_TOKEN)"}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf strings.Builder
			var mu sync.Mutex
			servertest.New(t, func(o *server.Options) {
				o.Logger = slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
				o.Config.Auth.Mode = c.mode
				o.Config.Permissions.Allow = c.allow
				if c.scoped {
					o.SecretsFor = ownStores(t.TempDir())
				}
			})
			mu.Lock()
			got := strings.Contains(buf.String(), "shares one secrets store")
			mu.Unlock()
			if got != c.want {
				t.Errorf("warned %v, want %v:\n%s", got, c.want, buf.String())
			}
		})
	}
}

type lockedWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
