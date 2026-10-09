package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/server/expose"
)

// tokens maps bearer tokens to who they sign in.
type tokens map[string]*auth.Identity

func (t tokens) Verify(_ context.Context, token string) (*auth.Identity, error) {
	if id, ok := t[token]; ok {
		return id, nil
	}
	return nil, errors.New("unknown token")
}

type cpRig struct {
	srv   *Server
	h     http.Handler
	svc   *controlplane.Service
	store *controlplane.Memory
	fake  *drivertest.Fake
}

func newCPRig(t *testing.T, values map[string]string) *cpRig {
	t.Helper()
	return newCPRigWith(t, values, nil, driver.CapAttach)
}

// newCPRigWith is newCPRig with the fake declaring caps and, when ex is
// set, exposures served from it.
func newCPRigWith(t *testing.T, values map[string]string, ex *expose.Server, caps ...driver.Capability) *cpRig {
	t.Helper()
	st, err := controlplane.NewSettings(values, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", caps...)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := controlplane.NewMemory()
	ws := t.TempDir()
	svc, err := controlplane.New(controlplane.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"
	mw := &auth.Middleware{PublicPaths: PublicPaths(), Verifier: tokens{
		"alice": {Subject: "alice", Tenant: "default"},
		"bob":   {Subject: "bob", Tenant: "default"},
		"admin": {Subject: "root", Tenant: "default", Groups: []string{DefaultAdminGroup}},
		"eve":   {Subject: "eve", Tenant: "other"},
	}}
	srv := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Auth: mw, ControlPlane: svc, Expose: ex})
	return &cpRig{srv: srv, h: srv.Handler(), svc: svc, store: mem, fake: fake}
}

type cpCall struct {
	method, path, token, body string
	header                    map[string]string
}

func (r *cpRig) do(t *testing.T, c cpCall) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func decodeSandbox(t *testing.T, rec *httptest.ResponseRecorder) controlplane.Sandbox {
	t.Helper()
	var sb controlplane.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &sb); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return sb
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e cpError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body %q: %v", rec.Body.String(), err)
	}
	return e.Error.Code
}

func (r *cpRig) create(t *testing.T, token, body string) controlplane.Sandbox {
	t.Helper()
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: token, body: body})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	sb := decodeSandbox(t, rec)
	if rec.Header().Get("ETag") != controlplane.ETag(sb.ResourceVersion) {
		t.Fatalf("ETag %q for version %d", rec.Header().Get("ETag"), sb.ResourceVersion)
	}
	return sb
}

func (r *cpRig) ready(t *testing.T, token string) controlplane.Sandbox {
	t.Helper()
	sb := r.create(t, token, `{"template":"default","labels":{"team":"a"}}`)
	if err := r.svc.Reconcile(context.Background(), sb.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.store.Get(context.Background(), sb.ID)
	if got.State != controlplane.StateReady {
		t.Fatalf("not ready: %s", got.State)
	}
	return got
}

func TestSandboxRoutesOffByDefault(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"template":"default"}`)))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("the control plane answered while off: %d", rec.Code)
	}
}

func TestSandboxCRUD(t *testing.T) {
	r := newCPRig(t, nil)
	sb := r.create(t, "alice", `{"template":"default","labels":{"team":"a"}}`)
	if sb.State != controlplane.StateProvisioning || sb.Owner != "subject:alice" && sb.Owner != "alice" {
		t.Fatalf("created %+v", sb)
	}
	r.create(t, "alice", `{"template":"default","labels":{"team":"b"}}`)

	rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID, token: "alice"})
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != controlplane.ETag(sb.ResourceVersion) {
		t.Fatalf("get: %d %q", rec.Code, rec.Header().Get("ETag"))
	}

	rec = r.do(t, cpCall{method: "GET", path: "/v1/sandboxes?selector=team%3Da", token: "alice"})
	var list sandboxList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != 200 || len(list.Items) != 1 || list.Items[0].ID != sb.ID {
		t.Fatalf("selector list: %d %s", rec.Code, rec.Body.String())
	}
	rec = r.do(t, cpCall{method: "GET", path: "/v1/sandboxes?limit=1", token: "alice"})
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Next == "" {
		t.Fatalf("paged list: %s", rec.Body.String())
	}
	if rec = r.do(t, cpCall{method: "GET", path: "/v1/sandboxes?selector=team%20in%20(a", token: "alice"}); rec.Code != 400 {
		t.Fatalf("a bad selector: %d", rec.Code)
	}
	if rec = r.do(t, cpCall{method: "GET", path: "/v1/sandboxes?state=Sleeping", token: "alice"}); rec.Code != 400 {
		t.Fatalf("a bad state: %d", rec.Code)
	}

	// PATCH needs If-Match, and a stale one is 412.
	patch := `{"labels":{"team":null,"tier":"gold"}}`
	if rec = r.do(t, cpCall{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, token: "alice", body: patch}); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PATCH without If-Match: %d", rec.Code)
	}
	rec = r.do(t, cpCall{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, token: "alice", body: patch,
		header: map[string]string{"If-Match": controlplane.ETag(sb.ResourceVersion + 100)}})
	if rec.Code != http.StatusPreconditionFailed || errCode(t, rec) != "precondition_failed" {
		t.Fatalf("PATCH with a stale If-Match: %d %s", rec.Code, rec.Body.String())
	}
	if rec = r.do(t, cpCall{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, token: "alice", body: patch,
		header: map[string]string{"If-Match": `W/"1"`}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a weak If-Match: %d", rec.Code)
	}
	rec = r.do(t, cpCall{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, token: "alice", body: patch,
		header: map[string]string{"If-Match": controlplane.ETag(sb.ResourceVersion)}})
	got := decodeSandbox(t, rec)
	if rec.Code != 200 || got.Labels["tier"] != "gold" || got.Labels["team"] != "" {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
	}
	rec = r.do(t, cpCall{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, token: "alice", body: `{"settings":{"exec.timeout_seconds":"30"}}`,
		header: map[string]string{"If-Match": "*"}})
	if got = decodeSandbox(t, rec); rec.Code != 200 || got.Settings["exec.timeout_seconds"] != "30" {
		t.Fatalf("PATCH settings: %d %s", rec.Code, rec.Body.String())
	}
	rec = r.do(t, cpCall{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, token: "alice", body: `{"settings":null}`,
		header: map[string]string{"If-Match": "*"}})
	if got = decodeSandbox(t, rec); rec.Code != 200 || len(got.Settings) != 0 {
		t.Fatalf("PATCH settings null: %d %s", rec.Code, rec.Body.String())
	}
	if rec = r.do(t, cpCall{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, token: "alice", body: `{"owner":"bob"}`,
		header: map[string]string{"If-Match": "*"}}); rec.Code != 400 {
		t.Fatalf("PATCH of the owner: %d", rec.Code)
	}

	rec = r.do(t, cpCall{method: "DELETE", path: "/v1/sandboxes/" + sb.ID, token: "alice",
		header: map[string]string{"If-Match": controlplane.ETag(got.ResourceVersion - 1)}})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("DELETE with a stale If-Match: %d", rec.Code)
	}
	rec = r.do(t, cpCall{method: "DELETE", path: "/v1/sandboxes/" + sb.ID, token: "alice"})
	if rec.Code != http.StatusAccepted || decodeSandbox(t, rec).State != controlplane.StateDeleting {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	if rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default","owner":"bob"}`}); rec.Code != 400 {
		t.Fatalf("create with an owner: %d", rec.Code)
	}
	if rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default","driver":"nope"}`}); rec.Code != 400 {
		t.Fatalf("create with an unknown driver: %d", rec.Code)
	}
}

func TestSandboxAuthAndTenancy(t *testing.T) {
	r := newCPRig(t, nil)
	sb := r.create(t, "alice", `{"template":"default"}`)
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", body: `{"template":"default"}`}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("create with no token: %d", rec.Code)
	}
	// Another user of the tenant cannot see it, by id or by list, nor act on it.
	for _, c := range []cpCall{
		{method: "GET", path: "/v1/sandboxes/" + sb.ID},
		{method: "DELETE", path: "/v1/sandboxes/" + sb.ID},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":stop"},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", body: `{"command":"id"}`},
		{method: "GET", path: "/v1/sandboxes/" + sb.ID + ":attach"},
		{method: "PATCH", path: "/v1/sandboxes/" + sb.ID, body: `{"labels":{"x":"y"}}`, header: map[string]string{"If-Match": "*"}},
	} {
		c.token = "bob"
		if rec := r.do(t, c); rec.Code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes", token: "bob"})
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("bob listed: %s", rec.Body.String())
	}
	// An admin sees every sandbox in the tenant.
	if rec = r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID, token: "admin"}); rec.Code != 200 {
		t.Fatalf("admin get: %d", rec.Code)
	}
	// Another tenant is refused outright, on every route.
	for _, c := range []cpCall{
		{method: "GET", path: "/v1/sandboxes"},
		{method: "POST", path: "/v1/sandboxes", body: `{"template":"default"}`},
		{method: "GET", path: "/v1/sandboxes/" + sb.ID},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":start"},
	} {
		c.token = "eve"
		if rec := r.do(t, c); rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
			t.Errorf("eve %s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if items, _, _ := r.store.List(context.Background(), controlplane.ListOptions{}); len(items) != 1 {
		t.Fatalf("%d sandboxes stored", len(items))
	}
}

// A browser on another origin cannot drive the API with the user's cookie.
func TestSandboxCrossOriginRefused(t *testing.T) {
	r := newCPRig(t, nil)
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`,
		header: map[string]string{"Origin": "https://evil.example"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin create: %d", rec.Code)
	}
	if items, _, _ := r.store.List(context.Background(), controlplane.ListOptions{}); len(items) != 0 {
		t.Fatal("a cross-origin create was stored")
	}
}

func TestSandboxIdempotency(t *testing.T) {
	r := newCPRig(t, nil)
	key := map[string]string{"Idempotency-Key": "create-1"}
	body := `{"template":"default"}`
	first := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: body, header: key})
	again := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: body, header: key})
	if first.Code != 201 || again.Code != 201 || again.Header().Get("Idempotent-Replayed") != "true" ||
		first.Body.String() != again.Body.String() || again.Header().Get("ETag") == "" {
		t.Fatalf("replay: %d %d %q\n%s\n%s", first.Code, again.Code, again.Header().Get("Idempotent-Replayed"), first.Body, again.Body)
	}
	if items, _, _ := r.store.List(context.Background(), controlplane.ListOptions{}); len(items) != 1 {
		t.Fatalf("a replayed create made %d sandboxes", len(items))
	}
	// The same key with another body is refused; another principal's key is its own.
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"other"}`, header: key}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a reused key with another body: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "bob", body: body, header: key}); rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("bob's request under alice's key: %d replayed=%q", rec.Code, rec.Header().Get("Idempotent-Replayed"))
	}
	// A request that failed validation changed nothing and frees its key.
	bad := map[string]string{"Idempotency-Key": "bad-1"}
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":""}`, header: bad}); rec.Code != 400 {
		t.Fatalf("invalid create: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: body, header: bad}); rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("the key of a refused request was not freed: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: body,
		header: map[string]string{"Idempotency-Key": "bad key with spaces"}}); rec.Code != 400 {
		t.Fatalf("a malformed key: %d", rec.Code)
	}
	// While the first request runs, a repeat is a conflict.
	sb := decodeSandbox(t, first)
	scope := controlplane.Scope(sb.Owner, "POST", "/v1/sandboxes/"+sb.ID+":stop")
	if _, ok, err := r.store.Claim(context.Background(), controlplane.IdemRecord{Scope: scope, Key: "stop-1",
		RequestHash: emptyHash, ExpiresAt: sb.CreatedAt.Add(1e15)}, sb.CreatedAt); err != nil || !ok {
		t.Fatal("could not hold the key")
	}
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":stop", token: "alice",
		header: map[string]string{"Idempotency-Key": "stop-1"}})
	if rec.Code != http.StatusConflict || errCode(t, rec) != "conflict" {
		t.Fatalf("a repeat of a request in flight: %d %s", rec.Code, rec.Body.String())
	}
}

// emptyHash is the request hash of an empty body.
var emptyHash = func() string { h := sha256.Sum256(nil); return hex.EncodeToString(h[:]) }()

func readFrames(t *testing.T, body []byte) []execFrame {
	t.Helper()
	var out []execFrame
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		var f execFrame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		out = append(out, f)
	}
	return out
}

func TestSandboxVerbsAndExecStream(t *testing.T) {
	r := newCPRig(t, nil)
	sb := r.create(t, "alice", `{"template":"default"}`)
	// Only Ready takes exec.
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: `{"command":"echo hi"}`}); rec.Code != http.StatusConflict {
		t.Fatalf("exec while provisioning: %d", rec.Code)
	}
	_ = r.svc.Reconcile(context.Background(), sb.ID)

	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: `{"argv":["echo","hi"]}`})
	frames := readFrames(t, rec.Body.Bytes())
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-ndjson" || len(frames) != 2 ||
		string(frames[0].Stdout) != "echo hi\n" || frames[1].Exit == nil || *frames[1].Exit != 0 || rec.Header().Get("X-Abhed-Generation") != "1" {
		t.Fatalf("exec: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), base64.StdEncoding.EncodeToString([]byte("echo hi\n"))) {
		t.Fatalf("stdout is not base64 in its frame: %s", rec.Body.String())
	}
	for _, body := range []string{`{}`, `{"argv":["a"],"command":"b"}`, `{"command":"x","bogus":1}`} {
		if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: body}); rec.Code != 400 {
			t.Errorf("exec %s: %d", body, rec.Code)
		}
	}
	// Seams owned by later work, and capabilities the driver lacks, answer
	// 501 with the contract's code.
	for _, c := range []cpCall{
		{method: "GET", path: "/v1/sandboxes/" + sb.ID + ":attach"},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":ssh"},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":forward", body: `{"port":80}`},
		// The fake declares neither copy nor snapshot, nor is exposing on.
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":cp?direction=out&path=f"},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":snapshot"},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":expose", body: `{"port":80}`},
		{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", body: `{"command":"cat","stdin":true}`},
	} {
		c.token = "alice"
		if rec := r.do(t, c); rec.Code != http.StatusNotImplemented || errCode(t, rec) != "unsupported" {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":explode", token: "alice"}); rec.Code != 404 {
		t.Fatalf("unknown verb: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/" + sb.ID + ":stop", token: "alice"}); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET :stop: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes/not-an-id", token: "alice"}); rec.Code != 404 {
		t.Fatalf("a malformed id: %d", rec.Code)
	}
	// The verb splits on the last ':'.
	if id, verb := splitTarget("sbx_0123456789abcdef:a:b"); id != "sbx_0123456789abcdef:a" || verb != "b" {
		t.Fatalf("split %q %q", id, verb)
	}

	rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":stop", token: "alice", body: `{"grace_seconds":1}`})
	if rec.Code != http.StatusAccepted || decodeSandbox(t, rec).State != controlplane.StateStopping {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body.String())
	}
	_ = r.svc.Reconcile(context.Background(), sb.ID)
	rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":start", token: "alice"})
	if got := decodeSandbox(t, rec); rec.Code != http.StatusAccepted || got.State != controlplane.StateStarting || got.Generation != 2 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if rec = r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":start", token: "alice"}); rec.Code != http.StatusConflict {
		t.Fatalf("start while starting: %d", rec.Code)
	}

	// Every mutation is in the record with who asked.
	evs, _ := r.store.AuditFor(context.Background(), sb.ID)
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.Outcome == controlplane.OutcomeOK {
			seen[ev.Action+" by "+ev.Principal] = true
		}
	}
	for _, want := range []string{"create by " + sb.Owner, "exec by " + sb.Owner, "stop by " + sb.Owner, "start by " + sb.Owner} {
		if !seen[want] {
			t.Errorf("record lacks %q: %v", want, seen)
		}
	}
}

func TestSandboxExecTotalOutputCap(t *testing.T) {
	r := newCPRig(t, map[string]string{controlplane.SetExecOutput: "4096"})
	sb := r.ready(t, "alice")
	r.fake.ExecFunc = func(ctx context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		// Small frames, each under any per-frame cap, without end.
		for {
			w := req.Stdout
			if ctx.Err() != nil {
				return driver.ExecResult{}, ctx.Err()
			}
			if _, err := w.Write(bytes.Repeat([]byte("y"), 100)); err != nil {
				return driver.ExecResult{}, err
			}
			if _, err := req.Stderr.Write([]byte("e")); err != nil {
				return driver.ExecResult{}, err
			}
		}
	}
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: `{"command":"yes"}`})
	frames := readFrames(t, rec.Body.Bytes())
	total := 0
	for _, f := range frames[:len(frames)-1] {
		total += len(f.Stdout) + len(f.Stderr)
	}
	last := frames[len(frames)-1]
	if total != 4096 || last.Exit != nil || last.Error == nil || last.Error.Code != "invalid" {
		t.Fatalf("%d bytes streamed under a 4096 cap; last frame %+v", total, last)
	}
}

func TestSandboxRateLimit(t *testing.T) {
	r := newCPRig(t, map[string]string{controlplane.SetMutationsPerMin: "2"})
	for i := range 2 {
		if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`}); rec.Code != 201 {
			t.Fatalf("create %d: %d", i, rec.Code)
		}
	}
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "alice", body: `{"template":"default"}`})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third create in a minute: %d", rec.Code)
	}
	// Per caller: bob still has his own budget, and reads are not limited.
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "bob", body: `{"template":"default"}`}); rec.Code != 201 {
		t.Fatalf("bob: %d", rec.Code)
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/sandboxes", token: "alice"}); rec.Code != 200 {
		t.Fatalf("alice list: %d", rec.Code)
	}
}

type brokenStore struct{ *controlplane.Memory }

func (brokenStore) Create(context.Context, controlplane.Sandbox) (controlplane.Sandbox, error) {
	return controlplane.Sandbox{}, errors.New("connection reset")
}

func (brokenStore) List(context.Context, controlplane.ListOptions) ([]controlplane.Sandbox, string, error) {
	return nil, "", errors.New("connection reset")
}

func TestSandboxStoreFaultFailsClosed(t *testing.T) {
	st, _ := controlplane.NewSettings(nil, nil)
	reg := driver.NewRegistry()
	_ = reg.Register(drivertest.New("process"))
	mem := controlplane.NewMemory()
	svc, err := controlplane.New(controlplane.Config{Store: brokenStore{mem}, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return "/w", nil }})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), ControlPlane: svc})
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/sandboxes", `{"template":"default"}`},
		{"GET", "/v1/sandboxes", ""},
	} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s with the store down: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// Readiness says so; health keeps its shape.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/ready", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"control_plane":"failed"`) {
		t.Fatalf("ready with the store down: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection reset") {
		t.Fatalf("readiness leaks the error: %s", rec.Body.String())
	}
}

func TestReady(t *testing.T) {
	r := newCPRig(t, nil)
	// Public, like health.
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/ready"}); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), `"control_plane":"failed"`) {
		t.Fatalf("ready before the reconciler runs: %d %s", rec.Code, rec.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.svc.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for i := 0; !r.svc.Running(); i++ {
		if i > 1000 {
			t.Fatal("the reconciler did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/ready"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ready":true`) {
		t.Fatalf("ready: %d %s", rec.Code, rec.Body.String())
	}
	r.srv.draining.Store(true)
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/ready"}); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"serving":"failed"`) {
		t.Fatalf("ready while draining: %d %s", rec.Code, rec.Body.String())
	}
	// Health keeps its shape.
	rec := r.do(t, cpCall{method: "GET", path: "/v1/health"})
	var h map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	for _, k := range []string{"status", "sessions", "model", "draining"} {
		if _, ok := h[k]; !ok {
			t.Fatalf("health lost %s: %v", k, h)
		}
	}
	if len(h) != 4 {
		t.Fatalf("health changed shape: %v", h)
	}
}
