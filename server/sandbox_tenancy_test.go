package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/server/expose"
)

// tenancyRig is a server with the sandbox API, exposures and the SSH relay
// on, as `abhed serve` runs them for two accounts of one tenant.
func tenancyRig(t *testing.T) (*wsRig, *expose.Server) {
	t.Helper()
	var svc *controlplane.Service
	ex := expose.New(expose.Options{Enabled: true, Current: func(ctx context.Context, id string) (int64, bool, error) {
		return svc.Current(ctx, id)
	}})
	r := buildCPRig(t, nil, ex, []driver.Capability{driver.CapAttach, driver.CapForward}, func(o *Options) {
		o.SandboxSSHKey = testHostKey(t)
	})
	svc = r.svc
	srv := httptest.NewServer(r.h)
	t.Cleanup(srv.Close)
	return &wsRig{cpRig: r, url: srv.URL}, ex
}

// One account cannot list, read, change, exec in, ssh into, forward to,
// expose, copy from, snapshot, restore, read the events of or delete
// another's sandbox. Each refusal is the 404 a sandbox that does not exist
// gets, word for word, so it confirms nothing; nothing is recorded or
// changed for it; and the other account's sandbox goes on working.
func TestSandboxTenancyAcrossTwoAccounts(t *testing.T) {
	r, ex := tenancyRig(t)
	alice := r.ready(t, "alice")
	path := "/v1/sandboxes/" + alice.ID
	// A well-formed id nobody holds, for what "not there" looks like.
	ghost, err := controlplane.NewID()
	if err != nil {
		t.Fatal(err)
	}
	gpath := "/v1/sandboxes/" + ghost

	// alice exposes a port; bob must not reach it, nor make his own on her sandbox.
	rec := r.do(t, cpCall{method: "POST", path: path + ":expose", token: "alice", body: `{"port":8000,"ttl_seconds":60}`})
	var e exposeResult
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &e) != nil {
		t.Fatalf("alice's expose: %d %s", rec.Code, rec.Body.String())
	}
	before := len(actionsOf(t, r.cpRig, alice.ID))
	anyMatch := map[string]string{"If-Match": "*"}
	calls := []cpCall{
		{method: "GET", path: ""},
		{method: "PATCH", path: "", body: `{"labels":{"x":"y"}}`, header: anyMatch},
		{method: "POST", path: ":start"},
		{method: "POST", path: ":stop"},
		{method: "POST", path: ":exec", body: `{"command":"id"}`},
		{method: "POST", path: ":ssh"},
		{method: "POST", path: ":expose", body: `{"port":8000}`},
		{method: "POST", path: ":cp?direction=out&path=src"},
		{method: "POST", path: ":cp?direction=in&path=planted", body: "x"},
		{method: "POST", path: ":snapshot"},
		{method: "POST", path: ":restore", body: `{"snapshot":{"id":"snap_x","generation":1,"digest":"sha256:00"}}`},
		{method: "GET", path: ":events"},
		{method: "DELETE", path: ""},
		{method: "DELETE", path: "", header: anyMatch},
	}
	for _, c := range calls {
		suffix := c.path
		c.token = "bob"
		c.path = path + suffix
		got := r.do(t, c)
		c.path = gpath + suffix
		want := r.do(t, c)
		if got.Code != http.StatusNotFound || errCode(t, got) != "not_found" {
			t.Errorf("bob %s %s: %d %s", c.method, suffix, got.Code, got.Body.String())
			continue
		}
		if g, w := strings.ReplaceAll(got.Body.String(), alice.ID, "ID"), strings.ReplaceAll(want.Body.String(), ghost, "ID"); g != w || got.Code != want.Code {
			t.Errorf("bob %s %s tells alice's sandbox from none: %d %q, none: %d %q", c.method, suffix, got.Code, g, want.Code, w)
		}
	}
	// The streaming verbs refuse before the connection is taken over.
	for _, v := range []struct{ path, proto string }{
		{":attach", protoAttach}, {":exec", protoExec}, {":forward?port=8000", protoForward}, {":ssh", protoSSH},
	} {
		if _, err := r.dial(t, "bob", path+v.path, v.proto); handshakeStatus(t, err) != http.StatusNotFound {
			t.Errorf("bob dials %s: %v", v.path, err)
		}
	}
	// alice's exposure answers bob as an unknown one does; a guessed one too.
	got := r.do(t, cpCall{method: "GET", path: e.Path, token: "bob"})
	want := r.do(t, cpCall{method: "GET", path: expose.DefaultPrefix + "exp_0000000000000000/", token: "bob"})
	if got.Code != http.StatusNotFound || got.Body.String() != want.Body.String() {
		t.Errorf("bob on alice's exposure: %d %q, unknown: %d %q", got.Code, got.Body.String(), want.Code, want.Body.String())
	}
	if n := len(ex.List(alice.ID)); n != 1 {
		t.Errorf("bob's calls changed alice's exposures: %d", n)
	}

	// Nothing bob did is in alice's sandbox's record, or changed it.
	if acts := actionsOf(t, r.cpRig, alice.ID); len(acts) != before {
		t.Errorf("bob's refused calls were recorded on alice's sandbox: %v", acts[before:])
	}
	now, _ := r.store.Get(context.Background(), alice.ID)
	if now.ResourceVersion != alice.ResourceVersion || now.State != controlplane.StateReady {
		t.Errorf("alice's sandbox changed: %+v", now)
	}

	// Listing: bob sees only his own, page by page, and no cursor names hers.
	bobs := r.ready(t, "bob")
	for i := 0; i < 3; i++ {
		r.ready(t, "alice")
	}
	seen := map[string]bool{}
	after := ""
	for page := 0; page < 10; page++ {
		rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes?limit=1&after=" + after, token: "bob"})
		var l sandboxList
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &l) != nil {
			t.Fatalf("bob's list: %d %s", rec.Code, rec.Body.String())
		}
		for _, sb := range l.Items {
			seen[sb.ID] = true
		}
		if strings.Contains(rec.Body.String(), alice.ID) || (l.Next != "" && l.Next != bobs.ID && !seen[l.Next]) {
			t.Fatalf("bob's list names another's sandbox: %s", rec.Body.String())
		}
		if l.Next == "" {
			break
		}
		after = l.Next
	}
	if len(seen) != 1 || !seen[bobs.ID] {
		t.Fatalf("bob listed %v", seen)
	}
	// A cursor of hers, or a selector, shows him nothing of hers either.
	for _, q := range []string{"?after=" + alice.ID, "?selector=team%3Da", "?state=Ready"} {
		rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes" + q, token: "bob"})
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), alice.ID) {
			t.Errorf("bob lists %s: %d %s", q, rec.Code, rec.Body.String())
		}
	}

	// An Idempotency-Key alice used does not replay her answer to bob.
	key := map[string]string{"Idempotency-Key": "k-shared-1"}
	a := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`, header: key})
	b := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "bob", body: `{"template":"default"}`, header: key})
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated || decodeSandbox(t, a).ID == decodeSandbox(t, b).ID {
		t.Fatalf("a shared key: alice %d %s, bob %d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}

	// alice still uses hers: her exposure and her ssh details answer her.
	if rec := r.do(t, cpCall{method: "POST", path: path + ":ssh", token: "alice"}); rec.Code != http.StatusOK {
		t.Fatalf("alice's ssh details: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(t, cpCall{method: "GET", path: e.Path, token: "alice"}); rec.Code != http.StatusBadGateway {
		t.Fatalf("alice's exposure: %d %s", rec.Code, rec.Body.String())
	}
}

// Sandbox ids are 64 random bits: none repeats, and none follows from the last.
func TestSandboxIDsAreNotGuessable(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 2000; i++ {
		id, err := controlplane.NewID()
		if err != nil || !controlplane.ValidID(id) || seen[id] {
			t.Fatalf("id %q: %v (repeat %v)", id, err, seen[id])
		}
		if prev != "" && id[:12] == prev[:12] {
			t.Fatalf("consecutive ids share a prefix: %s %s", prev, id)
		}
		seen[id], prev = true, id
	}
}
