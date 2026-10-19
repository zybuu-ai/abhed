package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// sent is one request a fake cluster received.
type sent struct {
	method, uri, body string
	dryRun            bool
}

// fakeCluster answers GET with live (404 when empty) and a write with
// answer, or with a 422 carrying refuse for a dry run when refuse is set.
type fakeCluster struct {
	mu     sync.Mutex
	got    []sent
	live   string
	answer string
	refuse string
}

func (f *fakeCluster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	dry := r.URL.Query().Get("dryRun") == "All"
	f.mu.Lock()
	f.got = append(f.got, sent{r.Method, r.URL.RequestURI(), string(b), dry})
	live, answer, refuse := f.live, f.answer, f.refuse
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && live == "":
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","message":"not there"}`)
	case r.Method == http.MethodGet:
		fmt.Fprint(w, live)
	case dry && refuse != "":
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintf(w, `{"kind":"Status","message":%q}`, refuse)
	default:
		fmt.Fprint(w, answer)
	}
}

func (f *fakeCluster) writes() (dry, real []sent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.got {
		switch {
		case s.method == http.MethodGet:
		case s.dryRun:
			dry = append(dry, s)
		default:
			real = append(real, s)
		}
	}
	return dry, real
}

// reasons keeps the reason each approval showed, and answers yes.
type reasons struct {
	mu   sync.Mutex
	seen []string
}

func (a *reasons) Approve(_ context.Context, _ string, _ json.RawMessage, res policy.Result) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, res.Reason)
	return true, nil
}

// runApply runs one k8s_apply call through the loop against f, in default
// mode with pol's rules, and returns what the approver saw and the events.
func runApply(t *testing.T, f *fakeCluster, pol *policy.Engine, args string) (*reasons, []agent.Event) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	if pol == nil {
		pol = policy.New(policy.ModeDefault)
	}
	ap := &reasons{}
	store := agent.NewMemStore()
	l := agent.NewLoop(&oneCall{name: "k8s_apply", args: args}, reg, pol, ap, newSession(t),
		agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s")
	return ap, evs
}

// toolResult is what the call's outcome told the model: its observation, or
// the refusal a call refused before approval is answered with.
func toolResult(evs []agent.Event) string {
	for _, ev := range evs {
		if ev.Type == agent.EvObservation || ev.Type == agent.EvActionDenied {
			return string(ev.Payload)
		}
	}
	return ""
}

const liveDeploy = `{"kind":"Deployment","apiVersion":"apps/v1","metadata":{"name":"web","namespace":"prod",
 "resourceVersion":"7","uid":"u1","managedFields":[{"manager":"x"}]},
 "spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"nginx:1.25"}]}}},
 "status":{"readyReplicas":2}}`

const dryDeploy = `{"kind":"Deployment","apiVersion":"apps/v1","metadata":{"name":"web","namespace":"prod",
 "resourceVersion":"8","uid":"u1","labels":{"tier":"front"}},
 "spec":{"replicas":3,"template":{"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}},
 "status":{"readyReplicas":2}}`

// The approval shows what the cluster's dry run says would change, field by
// field, and what was dry-run is what is sent once approved.
func TestApplyApprovalShowsTheDryRunDiff(t *testing.T) {
	f := &fakeCluster{live: liveDeploy, answer: dryDeploy}
	m := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","labels":{"tier":"front"}},` +
		`"spec":{"replicas":3,"template":{"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}}}`
	args, _ := json.Marshal(map[string]string{"action": "apply", "manifest": m})
	ap, _ := runApply(t, f, nil, string(args))
	if len(ap.seen) != 1 {
		t.Fatalf("asked %d times", len(ap.seen))
	}
	why := ap.seen[0]
	for _, want := range []string{"dry run: 3 field(s) change", "spec.replicas: 2 → 3",
		`spec.template.spec.containers[0].image: "nginx:1.25" → "nginx:1.27"`, `+ metadata.labels = {"tier":"front"}`} {
		if !strings.Contains(why, want) {
			t.Errorf("approval %q lacks %q", why, want)
		}
	}
	for _, noise := range []string{"resourceVersion", "managedFields", "readyReplicas"} {
		if strings.Contains(why, noise) {
			t.Errorf("approval %q names %s", why, noise)
		}
	}
	dry, real := f.writes()
	if len(dry) != 1 || len(real) != 1 || dry[0].body != real[0].body || !strings.Contains(real[0].uri, "force=true") {
		t.Fatalf("dry-ran %+v, sent %+v", dry, real)
	}
}

// A new object reads as created.
func TestApplyDryRunOfANewObject(t *testing.T) {
	f := &fakeCluster{answer: dryDeploy}
	m := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"}}`
	args, _ := json.Marshal(map[string]string{"action": "apply", "manifest": m})
	ap, _ := runApply(t, f, nil, string(args))
	if len(ap.seen) != 1 || !strings.Contains(ap.seen[0], "dry run: creates Deployment prod/web") {
		t.Fatalf("approval: %q", ap.seen)
	}
}

// A change the cluster's dry run refuses is refused before anyone is asked,
// with the cluster's reason for the model, and nothing is applied.
func TestFailedDryRunRefusesBeforeAsking(t *testing.T) {
	f := &fakeCluster{live: liveDeploy, answer: dryDeploy, refuse: `spec.replicas: Invalid value: -1`}
	args := `{"action":"scale","resource":"deployments","name":"web","replicas":-1}`
	ap, evs := runApply(t, f, nil, args)
	if len(ap.seen) != 0 {
		t.Fatalf("asked about a change the dry run refused: %q", ap.seen)
	}
	if _, real := f.writes(); len(real) != 0 {
		t.Fatalf("applied after a failed dry run: %+v", real)
	}
	out := toolResult(evs)
	if !strings.Contains(out, "dry run of this change failed") || !strings.Contains(out, "Invalid value: -1") {
		t.Fatalf("the model was told %s", out)
	}
}

// A denied call reaches no cluster, not even for a dry run.
func TestDeniedApplyIsNotDryRun(t *testing.T) {
	f := &fakeCluster{live: liveDeploy, answer: dryDeploy}
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddDeny("k8s_apply(*)"); err != nil {
		t.Fatal(err)
	}
	runApply(t, f, pol, `{"action":"restart","resource":"deployments","name":"web"}`)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) != 0 {
		t.Fatalf("a denied call reached the cluster: %+v", f.got)
	}
}

// A restart's patch carries a time: the one dry-run is the one sent.
func TestRestartSendsTheBodyItDryRan(t *testing.T) {
	f := &fakeCluster{live: liveDeploy, answer: liveDeploy}
	ap, _ := runApply(t, f, nil, `{"action":"restart","resource":"deployments","name":"web"}`)
	dry, real := f.writes()
	if len(dry) != 1 || len(real) != 1 || dry[0].body != real[0].body || dry[0].uri != real[0].uri+"?dryRun=All" {
		t.Fatalf("dry-ran %+v, sent %+v", dry, real)
	}
	if len(ap.seen) != 1 || !strings.Contains(ap.seen[0], "dry run: no change to the live object") {
		t.Fatalf("approval: %q", ap.seen)
	}
}

// A Secret's diff names the keys that change and never a value.
func TestSecretDiffShowsNoValues(t *testing.T) {
	f := &fakeCluster{
		live:   `{"kind":"Secret","apiVersion":"v1","metadata":{"name":"s","namespace":"prod"},"data":{"tls.key":"T0xELUtFWQ=="}}`,
		answer: `{"kind":"Secret","apiVersion":"v1","metadata":{"name":"s","namespace":"prod"},"data":{"tls.key":"TkVXLUtFWQ==","tls.crt":"Q1JU"}}`,
	}
	m := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"tls.key":"TkVXLUtFWQ==","tls.crt":"Q1JU"}}`
	args, _ := json.Marshal(map[string]string{"action": "apply", "manifest": m})
	ap, _ := runApply(t, f, nil, string(args))
	if len(ap.seen) != 1 {
		t.Fatalf("asked %d times", len(ap.seen))
	}
	why := ap.seen[0]
	if !strings.Contains(why, "data.tls.key: \"[withheld: 7 bytes, fingerprint ") || !strings.Contains(why, "+ data.tls.crt") ||
		strings.Contains(why, "sha256") {
		t.Errorf("approval %q", why)
	}
	for _, leak := range []string{"T0xELUtFWQ", "OLD-KEY", "Q1JU"} {
		if strings.Contains(why, leak) {
			t.Errorf("approval %q shows %q", why, leak)
		}
	}
}

// The dry run stops with the turn: a cluster that does not answer holds the
// call only until its context ends.
func TestDryRunStopsWithTheTurn(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := ApplyTool{M: mgr}.PrecheckContext(ctx, newSession(t),
		json.RawMessage(`{"action":"restart","resource":"deployments","name":"web"}`))
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("dry run against a silent cluster: %v after %s", err, time.Since(start))
	}
}
