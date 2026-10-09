package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
)

// Templates: an administrator makes them, everyone reads them, and a
// sandbox made from one gets its driver, labels and settings, which a
// create may tighten but never loosen.
func TestSandboxTemplates(t *testing.T) {
	r := newCPRig(t, nil)
	body := `{"name":"web","description":"web apps","driver":"process","labels":{"team":"a"},"settings":{"exec.timeout_seconds":"60"}}`
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandbox-templates", token: "alice", body: body}); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-admin made a template: %d %s", rec.Code, rec.Body.String())
	}
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandbox-templates", token: "admin", body: body,
		header: map[string]string{"Idempotency-Key": "k1"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var tmpl controlplane.Template
	_ = json.Unmarshal(rec.Body.Bytes(), &tmpl)
	if tmpl.Owner != "root" || tmpl.ResourceVersion < 1 {
		t.Fatalf("template %+v", tmpl)
	}
	// The same key replays; a second create under another key conflicts.
	again := r.do(t, cpCall{method: "POST", path: "/v1/sandbox-templates", token: "admin", body: body,
		header: map[string]string{"Idempotency-Key": "k1"}})
	if again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v", again.Code, again.Header())
	}
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandbox-templates", token: "admin", body: body}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	for _, bad := range []string{`{"name":"../x"}`, `{"name":"x","driver":"nope"}`, `{"name":"x","labels":{"abhed.dev/owner":"me"}}`,
		`{"name":"x","settings":{"exec.timeout_seconds":"soon"}}`, `{"name":"x","owner":"me"}`} {
		if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandbox-templates", token: "admin", body: bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body.String())
		}
	}

	// Reads, by anyone signed in, with selectors.
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandbox-templates/web", token: "alice"}); rec.Code != http.StatusOK ||
		rec.Header().Get("ETag") != controlplane.ETag(tmpl.ResourceVersion) {
		t.Fatalf("get: %d %v", rec.Code, rec.Header())
	}
	for sel, want := range map[string]int{"team%3Da": 1, "team%3Db": 0, "!team": 0} {
		rec := r.do(t, cpCall{method: "GET", path: "/v1/sandbox-templates?selector=" + sel, token: "alice"})
		var l templateList
		_ = json.Unmarshal(rec.Body.Bytes(), &l)
		if rec.Code != http.StatusOK || len(l.Items) != want {
			t.Errorf("selector %s: %d %s", sel, rec.Code, rec.Body.String())
		}
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandbox-templates?selector=a%20in", token: "alice"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad selector: %d", rec.Code)
	}

	// A sandbox from it.
	sb := r.create(t, "alice", `{"template":"web","labels":{"app":"shop"},"settings":{"exec.timeout_seconds":"30"}}`)
	if sb.Driver != "process" || sb.Labels["team"] != "a" || sb.Labels["app"] != "shop" || sb.Settings["exec.timeout_seconds"] != "30" {
		t.Fatalf("sandbox from the template %+v", sb)
	}
	sb = r.create(t, "alice", `{"template":"web","labels":{"team":"b"}}`)
	if sb.Labels["team"] != "b" || sb.Settings["exec.timeout_seconds"] != "60" {
		t.Fatalf("a create's own labels win, the template's settings stay: %+v", sb)
	}
	for _, bad := range []string{`{"template":"web","settings":{"exec.timeout_seconds":"600"}}`, `{"template":"web","driver":"other"}`} {
		if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	// A name with no template object stays a plain name.
	if sb := r.create(t, "alice", `{"template":"default"}`); sb.Template != "default" || len(sb.Settings) != 0 {
		t.Fatalf("a plain name %+v", sb)
	}

	// Removal: admin only, If-Match honoured, recorded.
	if rec := r.do(t, cpCall{method: "DELETE", path: "/v1/sandbox-templates/web", token: "alice"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-admin removed a template: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "DELETE", path: "/v1/sandbox-templates/web", token: "admin",
		header: map[string]string{"If-Match": `"999"`}}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "DELETE", path: "/v1/sandbox-templates/web", token: "admin",
		header: map[string]string{"If-Match": controlplane.ETag(tmpl.ResourceVersion)}}); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandbox-templates/web", token: "alice"}); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete: %d", rec.Code)
	}
	var made, removed bool
	evs, _ := r.store.AuditFor(context.Background(), "")
	for _, ev := range evs {
		made = made || (ev.Action == "template.create" && ev.Outcome == controlplane.OutcomeOK && ev.Principal == "root")
		removed = removed || (ev.Action == "template.delete" && ev.Outcome == controlplane.OutcomeOK)
	}
	if !made || !removed {
		t.Fatalf("template changes not recorded: %v %v", made, removed)
	}
}

// A sandbox's record is read back by whoever may see the sandbox, in pages.
func TestSandboxEvents(t *testing.T) {
	r := newCPRig(t, nil)
	sb := r.ready(t, "alice")
	rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID + ":events?limit=1", token: "alice"})
	var page eventsResult
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].Action != "create" || page.Next == 0 {
		t.Fatalf("first page: %d %s", rec.Code, rec.Body.String())
	}
	rec = r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID + ":events?after=" + jsonInt(page.Next), token: "alice"})
	var rest eventsResult
	_ = json.Unmarshal(rec.Body.Bytes(), &rest)
	if rec.Code != http.StatusOK || len(rest.Items) == 0 || rest.Items[0].Seq <= page.Next {
		t.Fatalf("next page: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID + ":events", token: "bob"}); rec.Code != http.StatusNotFound {
		t.Fatalf("another caller read the record: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID + ":events?after=x", token: "alice"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad cursor: %d", rec.Code)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

// On a server with no sign-in, whoever reaches it changes templates, as
// they change everything else there.
func TestSandboxTemplatesWithoutSignIn(t *testing.T) {
	r := newCPRig(t, nil, func(o *Options) { o.Config.Auth.Mode = "none" })
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandbox-templates", token: "alice", body: `{"name":"web"}`}); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(t, cpCall{method: "DELETE", path: "/v1/sandbox-templates/web", token: "alice"}); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
}
