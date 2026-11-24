package hostgit

import (
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Over https, host git asks the person's own credential helpers, in their
// order: emptying each helper key comes before any of their helpers, as one
// empty value clears the list every key adds to. A helper the repository's
// own configuration names stops the broker.
func TestBrokerHTTPSUsesThePersonsHelpers(t *testing.T) {
	r := newRemoteRig(t)
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		seen = append(seen, q.Header.Get("Authorization"))
		mu.Unlock()
		if q.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	ca := filepath.Join(r.home, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(r.home, ".gitconfig"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A generic helper that answers (a program of the person's own), and one
	// for this server that says nothing.
	helper := filepath.Join(r.home, "person-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho username=person\necho password=PERSON-TOKEN\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("[http]\n\tsslCAInfo = " + ca + "\n[credential]\n\thelper = \"!" + helper + "\"\n" +
		"[credential \"" + ts.URL + "\"]\n\thelper = \"!true\"\n")
	_ = f.Close()
	run(t, r.ws, "remote", "set-url", "origin", ts.URL+"/team/app.git")
	b := r.broker(Network{Open: true})
	out, ferr := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", ferr, out)
	mu.Lock()
	got := strings.Join(seen, " | ")
	mu.Unlock()
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("person:PERSON-TOKEN")); !strings.Contains(got, want) {
		t.Fatalf("the person's helper did not answer; the server saw %q", got)
	}
	if strings.Contains(out, "PERSON-TOKEN") {
		t.Fatalf("the token reached the output:\n%s", out)
	}
	planted := filepath.Join(r.home, "helper-ran")
	for _, key := range []string{"credential." + ts.URL + ".helper", "credential.helper"} {
		run(t, r.ws, "config", key, "!touch "+planted)
		b = r.broker(Network{Open: true})
		if _, err := runCmd(t, b.Command(r.ctx("c2"), r.ws, "git fetch")); err == nil || !strings.Contains(err.Error(), "with your credentials") {
			t.Fatalf("%s: %v", key, err)
		}
		run(t, r.ws, "config", "--unset-all", key)
	}
	if _, err := os.Stat(planted); err == nil {
		t.Fatal("the repository's credential helper ran on the host")
	}
}

// Where the sandbox does not hold git's configuration (protect_git off, or
// a fence without a mount namespace), nothing is run on the host: what was
// read before a command could change before host git read it.
func TestBrokerNeedsGitHeld(t *testing.T) {
	r := newRemoteRig(t)
	for _, b := range []*HostBroker{
		NewBroker(r.ws, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, r.command),
		NewBroker(r.ws, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, r.command).HoldsGit(func() bool { return false }, nil),
	} {
		if reason, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch"); reason != "" || err != nil {
			t.Fatalf("asked %q %v", reason, err)
		}
		if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch")); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.inner) != 2 {
		t.Fatalf("the sandbox ran %v", r.inner)
	}
	if log, _ := os.ReadFile(r.sshLog); len(log) != 0 {
		t.Fatalf("host ssh ran:\n%s", log)
	}
}
