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

// Over https, the credential helpers the repository's configuration names,
// for every URL or for this one, never run on the host: the person's own
// helper answers, and only to the remote's own server.
func TestBrokerHTTPSIgnoresTheRepositorysHelpers(t *testing.T) {
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
	_, _ = f.WriteString("[http]\n\tsslCAInfo = " + ca + "\n[credential]\n\thelper = \"!f() { echo username=person; echo password=PERSON-TOKEN; }; f\"\n")
	_ = f.Close()
	origin := ts.URL + "/team/app.git"
	run(t, r.ws, "remote", "set-url", "origin", origin)
	planted := filepath.Join(r.home, "helper-ran")
	run(t, r.ws, "config", "credential."+ts.URL+".helper", "!touch "+planted+"-url; echo password=PLANTED")
	run(t, r.ws, "config", "credential.helper", "!touch "+planted+"; echo password=PLANTED")
	b := r.broker(Network{Open: true})
	out, ferr := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", ferr, out)
	for _, m := range []string{planted, planted + "-url"} {
		if _, err := os.Stat(m); err == nil {
			t.Fatalf("the repository's credential helper ran on the host (%s)\n%s", m, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("person:PERSON-TOKEN"))
	got := strings.Join(seen, " | ")
	if !strings.Contains(got, want) || strings.Contains(got, base64.StdEncoding.EncodeToString([]byte("person:PLANTED"))) {
		t.Fatalf("the server saw %q", got)
	}
	if strings.Contains(out, "PERSON-TOKEN") {
		t.Fatalf("the token reached the output:\n%s", out)
	}
	seen = nil
	mu.Unlock()
	// The person's helper gives no password: git asks the next helper in the
	// list, which must never be one the repository names.
	gc, _ := os.ReadFile(filepath.Join(r.home, ".gitconfig"))
	if err := os.WriteFile(filepath.Join(r.home, ".gitconfig"), []byte(strings.Replace(string(gc), "echo password=PERSON-TOKEN; ", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	b = r.broker(Network{Open: true})
	out, ferr = runCmd(t, b.Command(r.ctx("c2"), r.ws, "git fetch"))
	t.Logf("fetch without a password: %v\n%s", ferr, out)
	for _, m := range []string{planted, planted + "-url"} {
		if _, err := os.Stat(m); err == nil {
			t.Fatalf("the repository's credential helper ran on the host once the person's gave no password (%s)", m)
		}
	}
	mu.Lock()
	if got := strings.Join(seen, " | "); strings.Contains(got, base64.StdEncoding.EncodeToString([]byte("person:PLANTED"))) {
		t.Fatalf("the server saw %q", got)
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
