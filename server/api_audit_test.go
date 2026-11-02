package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/server/expose"
)

// routeConsts are the constants route registrations join to a method.
var routeConsts = map[string]string{
	"CLIDevicePath": auth.CLIDevicePath, "CLITokenPath": auth.CLITokenPath, "CLIApprovePath": auth.CLIApprovePath,
	"CLIAuthorizePath": auth.CLIAuthorizePath, "CLIRevokePath": auth.CLIRevokePath,
	"CLIDiscoveryPath": CLIDiscoveryPath, "CLIPagePath": auth.CLIPagePath,
}

// exposeLine is how the exposure routes are registered, one per method.
const exposeLine = `mux.Handle(m+" "+s.opts.Expose.Prefix(), s.opts.Expose.Guard(s.mayUseExposure))`

// sourceRoutes reads every route the server's mux is given, from the
// packages that register on it, so a route added anywhere is in the table.
func sourceRoutes(t *testing.T, prefix string) []string {
	t.Helper()
	lit := regexp.MustCompile(`Handle(?:Func)?\("([A-Z]+ [^"]+)"`)
	joined := regexp.MustCompile(`Handle(?:Func)?\("([A-Z]+) "\s*\+\s*(?:auth\.)?(\w+)`)
	var out []string
	for _, dir := range []string{".", "../auth"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(b), "\n") {
				if !strings.Contains(line, "mux.Handle(") && !strings.Contains(line, "mux.HandleFunc(") {
					continue
				}
				switch m, j := lit.FindStringSubmatch(line), joined.FindStringSubmatch(line); {
				case m != nil:
					out = append(out, m[1])
				case j != nil && routeConsts[j[2]] != "":
					out = append(out, j[1]+" "+routeConsts[j[2]])
				case strings.TrimSpace(line) == exposeLine:
					for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
						out = append(out, method+" "+prefix)
					}
				default:
					t.Errorf("%s: a route registration the table cannot read: %s", f, strings.TrimSpace(line))
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// fill makes a path a pattern matches.
func fill(pattern string) string {
	path := pattern[strings.IndexByte(pattern, ' ')+1:]
	segs := strings.Split(path, "/")
	for i, s := range segs {
		switch {
		case s == "{$}":
			segs[i] = ""
		case strings.HasSuffix(s, "...}"):
			segs[i] = "a/b"
		case strings.HasPrefix(s, "{"):
			segs[i] = "x1"
		}
	}
	path = strings.Join(segs, "/")
	if strings.HasSuffix(path, "/") {
		path += "probe"
	}
	return path
}

// auditRig is a server with every route on: local accounts, CLI sign-in,
// the sandbox API, exposures, account secrets and an edition's mount.
func auditRig(t *testing.T, audit controlplane.AuditLog, m *Metrics, mounted *bool) (*cpRig, *expose.Server) {
	t.Helper()
	var svc *controlplane.Service
	ex := expose.New(expose.Options{Enabled: true, Current: func(ctx context.Context, id string) (int64, bool, error) {
		return svc.Current(ctx, id)
	}})
	r := buildCPRig(t, nil, ex, []driver.Capability{driver.CapAttach}, func(o *Options) {
		local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
		if err := local.CreateUser(context.Background(), auth.User{Username: "alice"}, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
		cli, err := auth.OpenCLITokens(local, filepath.Join(t.TempDir(), "cli.key"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		o.Config.Auth.Mode = "local"
		o.Auth = &auth.Middleware{Providers: []auth.Provider{local}, Verifier: o.Auth.Verifier,
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}
		o.CLILogin = auth.NewCLILogin(cli)
		o.APIAudit, o.Metrics = audit, m
		o.Mounts = append(o.Mounts, func(_ *Server, mux *http.ServeMux) {
			mux.HandleFunc("POST /v1/edition/made", func(w http.ResponseWriter, _ *http.Request) {
				WriteJSON(w, http.StatusCreated, map[string]string{"id": "thing_42", "name": "a name with spaces", "text": "secret words"})
			})
			mux.HandleFunc("POST /v1/edition/thing", func(w http.ResponseWriter, _ *http.Request) {
				if mounted != nil {
					*mounted = true
				}
				w.WriteHeader(http.StatusNoContent)
			})
		})
	})
	svc = r.svc
	return r, ex
}

// Plan measure B13: every mutating route writes a chained audit event with
// its principal before it runs and another with its outcome, whatever the
// handler answers; a route left out says why.
func TestEveryMutatingRouteIsAudited(t *testing.T) {
	audit := controlplane.NewMemory()
	r, ex := auditRig(t, audit, nil, nil)
	routes := append(sourceRoutes(t, ex.Prefix()), "POST /v1/edition/thing", "POST /v1/edition/made")
	public := r.srv.opts.Auth.PublicPaths
	mutating := 0
	for _, p := range routes {
		method := p[:strings.IndexByte(p, ' ')]
		if !Mutating(method) {
			continue
		}
		mutating++
		if r.srv.exposureRoute(p) {
			before, _ := audit.AuditFor(context.Background(), "")
			r.do(t, cpCall{method: method, path: fill(p), token: "admin", body: "{}"})
			if after, _ := audit.AuditFor(context.Background(), ""); len(after) != len(before) {
				t.Errorf("an exposed app's request %s was audited: %+v", p, after[len(before):])
			}
			continue
		}
		if why, ok := unaudited[p]; ok {
			if why == "" {
				t.Errorf("%s is left out of the audit with no reason", p)
			}
			continue
		}
		t.Run(p, func(t *testing.T) {
			before, _ := audit.AuditFor(context.Background(), "")
			rec := r.do(t, cpCall{method: method, path: fill(p), token: "admin", body: "{}"})
			after, _ := audit.AuditFor(context.Background(), "")
			evs := after[len(before):]
			if batched[p] != "" {
				if len(evs) != 1 || evs[0].Outcome != controlplane.OutcomeRequested || evs[0].Principal != "root" ||
					evs[0].Action != APIAuditPrefix+p || evs[0].Detail["batch"] == "" {
					t.Fatalf("a batched route wrote %+v, want its one requested event", evs)
				}
				return
			}
			if len(evs) != 2 {
				t.Fatalf("%d audit events for %s (status %d), want 2: %+v", len(evs), p, rec.Code, evs)
			}
			req, out := evs[0], evs[1]
			if req.Action != APIAuditPrefix+p || out.Action != req.Action {
				t.Fatalf("actions %q, %q; want %q", req.Action, out.Action, APIAuditPrefix+p)
			}
			// A sign-in route answers before anyone is signed in.
			who := "root"
			if slices.Contains(public, fill(p)) {
				who = "anonymous"
			}
			if req.Principal != who || out.Principal != who {
				t.Fatalf("principal %q, %q; want %s", req.Principal, out.Principal, who)
			}
			if req.Outcome != controlplane.OutcomeRequested {
				t.Fatalf("first outcome %q", req.Outcome)
			}
			want := controlplane.OutcomeOK
			if rec.Code >= 400 {
				want = controlplane.OutcomeFailed
			}
			if out.Outcome != want || (want == controlplane.OutcomeFailed && out.Code == "") {
				t.Fatalf("outcome %q code %q for status %d", out.Outcome, out.Code, rec.Code)
			}
			if req.Detail["route"] != p || req.Detail["tenant"] != "default" {
				t.Fatalf("detail %v", req.Detail)
			}
		})
	}
	if mutating < 50 {
		t.Fatalf("the table has %d mutating routes; the scan is missing some", mutating)
	}
	for _, list := range []map[string]string{unaudited, batched} {
		for p, why := range list {
			if !slices.Contains(routes, p) || why == "" {
				t.Errorf("%s is listed with reason %q but no code registers it", p, why)
			}
		}
	}
}

type failingAudit struct{ calls int }

func (f *failingAudit) Append(context.Context, controlplane.AuditEvent) error {
	f.calls++
	return errors.New("disk full")
}

// A change whose record cannot be written is not made.
func TestUnrecordedChangeIsRefused(t *testing.T) {
	ran := false
	fa := &failingAudit{}
	m := NewMetrics()
	r, _ := auditRig(t, fa, m, &ran)
	rec := r.do(t, cpCall{method: "POST", path: "/v1/edition/thing", token: "admin"})
	if rec.Code != http.StatusServiceUnavailable || ran {
		t.Fatalf("status %d, handler ran %v", rec.Code, ran)
	}
	if fa.calls != 1 {
		t.Fatalf("%d appends, want 1", fa.calls)
	}
	// Reads go on.
	if rec := r.do(t, cpCall{method: "GET", path: "/v1/health", token: "admin"}); rec.Code != http.StatusOK {
		t.Fatalf("health %d", rec.Code)
	}
	scrape := httptest.NewRecorder()
	m.Handler().ServeHTTP(scrape, httptest.NewRequest("GET", "/metrics", nil))
	b := scrape.Body
	for _, want := range []string{
		`abhed_record_write_errors_total{record="audit"} 1`,
		`abhed_http_requests_total{route="POST /v1/edition/thing",method="POST",code="503"} 1`,
		`abhed_http_requests_total{route="GET /v1/health",method="GET",code="200"} 1`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %s:\n%s", want, b.String())
		}
	}
}

// The audit keeps what the route's wildcards took, and nothing of the
// body or the query.
func TestAuditEventDetail(t *testing.T) {
	audit := controlplane.NewMemory()
	r, _ := auditRig(t, audit, nil, nil)
	r.do(t, cpCall{method: "PUT", path: "/v1/secrets/DEPLOY_KEY?value=hunter2", token: "admin", body: `{"value":"hunter2"}`})
	evs, _ := audit.AuditFor(context.Background(), "")
	if len(evs) != 2 {
		t.Fatalf("%d events", len(evs))
	}
	d := evs[0].Detail
	if d["path.name"] != "DEPLOY_KEY" || d["method"] != "PUT" || d["route"] != "PUT /v1/secrets/{name}" || d["remote"] == "" {
		t.Fatalf("detail %v", d)
	}
	for _, ev := range evs {
		for k, v := range ev.Detail {
			if strings.Contains(k+v, "hunter2") {
				t.Fatalf("the audit holds the secret: %v", ev.Detail)
			}
		}
	}
}

func TestWildcards(t *testing.T) {
	got := wildcards("POST /v1/sessions/{id}/pty/{pty}/input", "/v1/sessions/s1/pty/p2/input")
	if got["id"] != "s1" || got["pty"] != "p2" || len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if got := wildcards("GET /files/{rest...}", "/files/a/b/c"); got["rest"] != "a/b/c" {
		t.Fatalf("%v", got)
	}
	if got := wildcards("POST /x/{id}", "/x/"+strings.Repeat("a", 500)); len(got["id"]) != maxWildcard {
		t.Fatalf("not clipped: %d", len(got["id"]))
	}
}

// Without an audit or metrics the mux is served as it was.
func TestRoutedIsOffByDefault(t *testing.T) {
	s := &Server{opts: Options{}}
	mux := http.NewServeMux()
	if h := s.routed(mux); h != http.Handler(mux) {
		t.Fatal("routed wrapped the mux with nothing to do")
	}
}

// The outcome of a create names what it made, from the answer's id fields
// alone; text in the answer, and an id field holding text, are left out.
func TestAuditNamesWhatWasCreated(t *testing.T) {
	audit := controlplane.NewMemory()
	r, _ := auditRig(t, audit, nil, nil)
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/edition/made", token: "admin"}); rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	evs, _ := audit.AuditFor(context.Background(), "")
	if len(evs) != 2 {
		t.Fatalf("%d events", len(evs))
	}
	d := evs[1].Detail
	if d["created.id"] != "thing_42" || d["created.name"] != "" || evs[0].Detail["created.id"] != "" {
		t.Fatalf("detail %v / %v", evs[0].Detail, d)
	}
	for _, v := range d {
		if strings.Contains(v, "secret") || strings.Contains(v, "spaces") {
			t.Fatalf("the audit holds answer text: %v", d)
		}
	}
	// A sandbox's id, from the real create.
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes", token: "admin", body: `{"template":"default"}`}); rec.Code != http.StatusCreated {
		t.Fatalf("create sandbox: %d %s", rec.Code, rec.Body)
	} else {
		sb := decodeSandbox(t, rec)
		evs, _ = audit.AuditFor(context.Background(), "")
		if got := evs[len(evs)-1].Detail["created.id"]; got != sb.ID {
			t.Fatalf("created.id %q, want %s", got, sb.ID)
		}
	}
}

// A zero or nil Metrics counts nothing and does not panic, wherever it is used.
func TestZeroMetricsIsSafe(t *testing.T) {
	for _, m := range []*Metrics{nil, {}} {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("%d %q", rec.Code, rec.Body)
		}
		mem := controlplane.NewMemory()
		if got := m.AuditLog(mem); got != controlplane.AuditLog(mem) {
			t.Fatalf("a zero Metrics wrapped the log: %T", got)
		}
		audit := controlplane.NewMemory()
		r, _ := auditRig(t, audit, m, nil)
		if rec := r.do(t, cpCall{method: "POST", path: "/v1/edition/thing", token: "admin"}); rec.Code != http.StatusNoContent {
			t.Fatalf("status %d", rec.Code)
		}
		if evs, _ := audit.AuditFor(context.Background(), ""); len(evs) != 2 {
			t.Fatalf("%d events with a zero Metrics", len(evs))
		}
	}
}
