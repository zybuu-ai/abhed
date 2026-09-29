package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// authLog is a fake cluster that accepts one token and notes every one it is sent.
func authLog(t *testing.T, accept string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+accept {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","message":"Unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/version") {
			fmt.Fprint(w, `{"major":"1","minor":"29"}`)
			return
		}
		fmt.Fprint(w, `{"kind":"NodeList","items":[{"metadata":{"name":"n1"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// One manager serves every session on a server. A login in one session must
// never be the credential another session's k8s_get uses.
func TestLoginIsScopedToTheSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(server string) Config
	}{
		// B has the operator's kubeconfig: it must keep using that.
		{"kubeconfig", func(server string) Config { return Config{Kubeconfig: writeKubeconfig(t, server)} }},
		// B has nothing configured: it must fail, not borrow A's login.
		{"no kubeconfig", func(string) Config {
			return Config{Kubeconfig: t.TempDir() + "/missing"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, seen := authLog(t, "tok-A-only")
			mgr := NewManager(tc.cfg(srv.URL))
			a, b := newSession(t), newSession(t)

			login := LoginTool{M: mgr, Secret: stored(map[string]string{"OCP_TOKEN": "tok-A-only"})}
			args, _ := json.Marshal(map[string]string{"server": srv.URL, "token_secret": "OCP_TOKEN"})
			if res := login.Run(context.Background(), a, args); res.IsError {
				t.Fatalf("session A could not log in: %s", res.Content)
			}
			get, _ := json.Marshal(map[string]string{"resource": "nodes"})
			if res := (GetTool{M: mgr}).Run(context.Background(), a, get); res.IsError {
				t.Fatalf("session A cannot use its own login: %s", res.Content)
			}

			before := len(seen())
			res := GetTool{M: mgr}.Run(context.Background(), b, get)
			if !res.IsError {
				t.Fatalf("session B read the cluster with session A's login: %s", res.Content)
			}
			for _, auth := range seen()[before:] {
				if strings.Contains(auth, "tok-A-only") {
					t.Fatalf("session B's k8s_get sent session A's token")
				}
			}
			// Nor does a caller with no session at all.
			if res := (GetTool{M: mgr}).Run(context.Background(), nil, get); !res.IsError {
				t.Fatalf("a call with no session used session A's login: %s", res.Content)
			}
		})
	}
}

// Isolation cannot be guaranteed with no session to hold the login, so
// k8s_login refuses rather than keep it where every caller would find it.
func TestLoginRefusesWithoutASession(t *testing.T) {
	srv, _ := authLog(t, "tok")
	mgr := NewManager(Config{})
	args, _ := json.Marshal(map[string]string{"server": srv.URL, "token_secret": "T"})
	res := LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}.Run(context.Background(), nil, args)
	if !res.IsError {
		t.Fatalf("logged in with no session: %s", res.Content)
	}
}

// The token is not an argument: a call that sends one the old way has it
// dropped before policy, approval or the record see the arguments.
func TestLoginTakesNoTokenArgument(t *testing.T) {
	if strings.Contains(string(LoginTool{}.Schema()), `"token"`) {
		t.Fatal("k8s_login's schema still asks the model for the token")
	}
	canon, dropped, err := tools.CanonicalArgs(LoginTool{}, json.RawMessage(
		`{"server":"https://x","token":"sha256~pasted","token_secret":"OCP_TOKEN"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canon), "sha256~pasted") {
		t.Fatalf("a token sent as an argument was kept: %s", canon)
	}
	if len(dropped) != 1 || dropped[0] != "token" {
		t.Fatalf("dropped = %v, want [token]", dropped)
	}
	// A token put where the name belongs is withheld the same way.
	got := tools.WithholdSecretValues(LoginTool{}, json.RawMessage(`{"server":"https://x","token_secret":"sha256~pasted"}`))
	if strings.Contains(string(got), "sha256~pasted") {
		t.Fatalf("a token in token_secret was kept: %s", got)
	}
	if names := tools.SecretNames(LoginTool{}, json.RawMessage(`{"token_secret":"OCP_TOKEN"}`)); len(names) != 1 || names[0] != "OCP_TOKEN" {
		t.Fatalf("token_secret is not put to a secret rule: %v", names)
	}
}
