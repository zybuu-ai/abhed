package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
)

// relayAll answers every call it is asked about, as a relay to the owner would.
type relayAll struct {
	mu    sync.Mutex
	on    bool
	asked []string
}

func (r *relayAll) Route(w http.ResponseWriter, req *http.Request, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, req.Method+" "+req.URL.Path)
	if !r.on {
		return false
	}
	w.Header().Set("X-Relayed-To", "node-b")
	w.WriteHeader(http.StatusTeapot)
	return true
}

func TestSandboxCallsGoToTheOwningReplica(t *testing.T) {
	r := newCPRig(t, nil)
	sb := r.ready(t, "alice")
	rt := &relayAll{on: true}
	r.srv.opts.SandboxRouter = rt
	path := "/v1/sandboxes/" + sb.ID
	for _, c := range []cpCall{
		{method: http.MethodPatch, path: path, token: "alice", body: `{"labels":{"a":"b"}}`, header: map[string]string{"If-Match": "*"}},
		{method: http.MethodDelete, path: path, token: "alice"},
		{method: http.MethodPost, path: path + ":stop", token: "alice"},
		{method: http.MethodPost, path: path + ":exec", token: "alice", body: `{"command":"true"}`},
		{method: http.MethodGet, path: path + ":attach", token: "alice"},
	} {
		if rec := r.do(t, c); rec.Code != http.StatusTeapot || rec.Header().Get("X-Relayed-To") != "node-b" {
			t.Fatalf("%s %s was not relayed: %d", c.method, c.path, rec.Code)
		}
	}
	if r.fake.Calls("exec") != 0 || r.fake.Calls("stop") != 0 {
		t.Fatal("a relayed call also ran here")
	}
	if got, _ := r.store.Get(context.Background(), sb.ID); got.State != controlplane.StateReady || got.Labels["a"] != "" {
		t.Fatalf("a relayed call changed the sandbox here: %+v", got)
	}
	asked := len(rt.asked)
	// Reads come from the shared store; a caller who is not signed in, or is
	// in another tenant, is answered here and never relayed.
	if rec := r.do(t, cpCall{method: http.MethodGet, path: path, token: "alice"}); rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: http.MethodPost, path: path + ":stop"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous stop: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: http.MethodPost, path: path + ":stop", token: "eve"}); rec.Code != http.StatusForbidden {
		t.Fatalf("another tenant's stop: %d", rec.Code)
	}
	if len(rt.asked) != asked {
		t.Fatalf("relayed %v", rt.asked[asked:])
	}
	rt.on = false
	if rec := r.do(t, cpCall{method: http.MethodPost, path: path + ":stop", token: "alice"}); rec.Code != http.StatusAccepted {
		t.Fatalf("stop handled here: %d %s", rec.Code, rec.Body)
	}
}

type denyAll struct{}

func (denyAll) Admit(context.Context, controlplane.AdmissionRequest) (controlplane.AdmissionResult, error) {
	return controlplane.AdmissionResult{}, controlplane.ErrDenied
}

func (denyAll) Committed(context.Context, controlplane.AdmissionRequest, controlplane.Sandbox, error) []controlplane.AdmissionDecision {
	return nil
}

func TestDeniedSandboxCallIsForbidden(t *testing.T) {
	st, _ := controlplane.NewSettings(nil, nil)
	r := newCPRig(t, nil)
	reg := driver.NewRegistry()
	if err := reg.Register(r.fake); err != nil {
		t.Fatal(err)
	}
	svc, err := controlplane.New(controlplane.Config{Store: r.store, Audit: r.store, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return t.TempDir(), nil }, Interceptor: denyAll{}})
	if err != nil {
		t.Fatal(err)
	}
	r.srv.opts.ControlPlane = svc
	rec := r.do(t, cpCall{method: http.MethodPost, path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`})
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
		t.Fatalf("denied create: %d %s", rec.Code, rec.Body)
	}
}
