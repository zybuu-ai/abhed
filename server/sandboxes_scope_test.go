package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
)

// workspaces puts alice in workspace a and bob in b, refuses dave, and
// cannot answer for carol.
func workspaces(o *Options) {
	o.SandboxCaller = func(r *http.Request, c controlplane.Caller) (controlplane.Caller, error) {
		switch c.Principal {
		case "alice":
			c.Scope = map[string]string{"abhed.dev/workspace": "a"}
		case "bob":
			c.Scope, c.Admin = map[string]string{"abhed.dev/workspace": "b"}, true
		case "dave":
			return c, controlplane.ErrForbidden
		case "carol":
			return c, errors.New("membership store down")
		}
		return c, nil
	}
}

// Every route refuses a sandbox in another scope as if it were not there,
// an administrator of the other scope included.
func TestSandboxRoutesKeepToTheCallersScope(t *testing.T) {
	r := newCPRig(t, nil, workspaces)
	sb := r.ready(t, "alice")
	if sb.Labels["abhed.dev/workspace"] != "a" {
		t.Fatalf("not stamped: %v", sb.Labels)
	}
	path := "/v1/sandboxes/" + sb.ID
	any := map[string]string{"If-Match": "*"}
	for _, c := range []cpCall{
		{method: "GET", path: path},
		{method: "PATCH", path: path, body: `{"labels":{"x":"y"}}`, header: any},
		{method: "DELETE", path: path},
		{method: "POST", path: path + ":start"},
		{method: "POST", path: path + ":stop"},
		{method: "POST", path: path + ":exec", body: `{"command":"true"}`},
		{method: "GET", path: path + ":attach"},
		{method: "POST", path: path + ":snapshot"},
	} {
		c.token = "bob"
		if rec := r.do(t, c); rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
			t.Fatalf("%s %s from another workspace: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes", token: "bob"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), sb.ID) {
		t.Fatalf("another workspace's list: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := r.store.Get(context.Background(), sb.ID); got.ResourceVersion != sb.ResourceVersion {
		t.Fatal("a refused call changed the sandbox")
	}
	rec = r.do(t, cpCall{method: "GET", path: "/v1/sandboxes", token: "admin"})
	if !strings.Contains(rec.Body.String(), sb.ID) {
		t.Fatalf("the unscoped administrator lost sight of it: %s", rec.Body.String())
	}

	if rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "dave", body: `{"template":"default"}`}); rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
		t.Fatalf("a refused caller: %d %s", rec.Code, rec.Body.String())
	}
	if rec = r.do(t, cpCall{method: "GET", path: path, token: "carol"}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a caller that cannot be resolved: %d", rec.Code)
	}
}

type httpGuard struct {
	mu       sync.Mutex
	approval string
	refuse   error
}

func (g *httpGuard) Admit(_ context.Context, a *controlplane.Admission) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.approval = a.Approval
	return nil, g.refuse
}

// A guard's refusal is 403 with its code; one waiting on approval names the
// approval, which the request is then sent again with.
func TestGuardRefusalsOverHTTP(t *testing.T) {
	r := newCPRig(t, nil)
	g := &httpGuard{refuse: controlplane.ErrQuotaExceeded}
	if err := r.svc.SetGuard(g); err != nil {
		t.Fatal(err)
	}
	key := map[string]string{"Idempotency-Key": "k1"}
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`, header: key})
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "quota_exceeded" {
		t.Fatalf("past a quota: %d %s", rec.Code, rec.Body.String())
	}

	g.mu.Lock()
	g.refuse = &controlplane.ApprovalNeeded{ID: "apr_7", Message: "two approvers must agree"}
	g.mu.Unlock()
	rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`, header: key})
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "approval_required" || rec.Header().Get("Abhed-Approval") != "apr_7" ||
		!strings.Contains(rec.Body.String(), "two approvers must agree") {
		t.Fatalf("waiting on approval: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}

	// The refusals released the idempotency key, so the approved retry runs.
	g.mu.Lock()
	g.refuse = nil
	g.mu.Unlock()
	rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`,
		header: map[string]string{"Idempotency-Key": "k1", "Abhed-Approval": "apr_7"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("approved retry: %d %s", rec.Code, rec.Body.String())
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.approval != "apr_7" {
		t.Fatalf("the guard saw approval %q", g.approval)
	}
}

// A control plane turned on only to give each session a sandbox answers its
// own routes for administrators alone; every other account is refused.
func TestSandboxAPIAdminOnly(t *testing.T) {
	r := newCPRig(t, nil, func(o *Options) { o.SandboxAPIAdminOnly = true })
	for _, c := range []cpCall{
		{method: "GET", path: "/v1/sandboxes"},
		{method: "POST", path: "/v1/sandboxes", body: `{"template":"default"}`},
		{method: "GET", path: "/v1/sandboxes/sb-x"},
		{method: "GET", path: "/v1/sandbox-templates"},
		{method: "POST", path: "/v1/sandbox-templates", body: `{"name":"t"}`},
	} {
		c.token = "alice"
		if rec := r.do(t, c); rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
			t.Errorf("%s %s by an account: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/v1/sandboxes", "/v1/sandbox-templates"} {
		if rec := r.do(t, cpCall{method: "GET", path: path, token: "admin"}); rec.Code != http.StatusOK {
			t.Errorf("GET %s by an administrator: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
