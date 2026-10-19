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
	"sync/atomic"
	"testing"
	"time"
)

// rolloutCluster holds one Deployment at revision 3 and its ReplicaSets.
type rolloutCluster struct {
	mu      sync.Mutex
	paused  bool
	patches []sent
	rsList  string
}

const rsItems = `{"kind":"ReplicaSetList","items":[
 {"metadata":{"name":"web-a","annotations":{"deployment.kubernetes.io/revision":"1"},"ownerReferences":[{"uid":"d-uid"}]},
  "spec":{"template":{"metadata":{"labels":{"app":"web","pod-template-hash":"aaa"}},"spec":{"containers":[{"name":"app","image":"nginx:1.23"}]}}}},
 {"metadata":{"name":"web-b","annotations":{"deployment.kubernetes.io/revision":"2"},"ownerReferences":[{"uid":"d-uid"}]},
  "spec":{"template":{"metadata":{"labels":{"app":"web","pod-template-hash":"bbb"}},"spec":{"containers":[{"name":"app","image":"nginx:1.25"}]}}}},
 {"metadata":{"name":"web-c","annotations":{"deployment.kubernetes.io/revision":"3"},"ownerReferences":[{"uid":"d-uid"}]},
  "spec":{"template":{"metadata":{"labels":{"app":"web","pod-template-hash":"ccc"}},"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}}},
 {"metadata":{"name":"other","annotations":{"deployment.kubernetes.io/revision":"2"},"ownerReferences":[{"uid":"not-mine"}]},
  "spec":{"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"app","image":"evil:1"}]}}}}]}`

func (rc *rolloutCluster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/deployments/web"):
		fmt.Fprintf(w, `{"kind":"Deployment","apiVersion":"apps/v1","metadata":{"name":"web","namespace":"prod","uid":"d-uid",
		 "resourceVersion":"41","annotations":{"deployment.kubernetes.io/revision":"3"}},
		 "spec":{"paused":%t,"selector":{"matchLabels":{"app":"web"}},
		  "template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}}}`, rc.paused)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/replicasets"):
		if r.URL.Query().Get("labelSelector") != "app=web" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		list := rc.rsList
		if list == "" {
			list = rsItems
		}
		fmt.Fprint(w, list)
	case r.Method == http.MethodPatch:
		b, _ := io.ReadAll(r.Body)
		rc.patches = append(rc.patches, sent{r.Method, r.URL.RequestURI(), string(b), r.URL.Query().Get("dryRun") == "All"})
		if ct := r.Header.Get("Content-Type"); ct != "application/json-patch+json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		fmt.Fprint(w, `{"kind":"Deployment","apiVersion":"apps/v1","metadata":{"name":"web","namespace":"prod"},
		 "spec":{"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"app","image":"nginx:1.25"}]}}}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func rolloutMgr(t *testing.T, h http.Handler) *Manager {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
}

// A rollback puts back the previous revision's template, less the hash the
// controller adds, only if the Deployment is as it was when planned; a
// ReplicaSet another object owns is never chosen.
func TestRollbackToThePreviousRevision(t *testing.T) {
	rc := &rolloutCluster{}
	mgr := rolloutMgr(t, rc)
	res := ApplyTool{M: mgr}.Run(context.Background(), nil,
		json.RawMessage(`{"action":"rollback","resource":"deployment","name":"web"}`))
	if res.IsError || !strings.Contains(res.Content, "from revision 3 to revision 2") {
		t.Fatalf("rollback: %s", res.Content)
	}
	if len(rc.patches) != 1 {
		t.Fatalf("patches: %+v", rc.patches)
	}
	var ops []map[string]any
	if err := json.Unmarshal([]byte(rc.patches[0].body), &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/resourceVersion" || ops[0]["value"] != "41" {
		t.Fatalf("no resourceVersion test first: %s", rc.patches[0].body)
	}
	body := rc.patches[0].body
	if !strings.Contains(body, "nginx:1.25") || strings.Contains(body, "pod-template-hash") || strings.Contains(body, "evil") {
		t.Fatalf("wrong template: %s", body)
	}
}

func TestRollbackToANamedRevision(t *testing.T) {
	rc := &rolloutCluster{}
	mgr := rolloutMgr(t, rc)
	run := func(args string) string {
		return ApplyTool{M: mgr}.Run(context.Background(), nil, json.RawMessage(args)).Content
	}
	if got := run(`{"action":"rollback","resource":"deployments","name":"web","revision":1}`); !strings.Contains(got, "to revision 1") ||
		!strings.Contains(rc.patches[len(rc.patches)-1].body, "nginx:1.23") {
		t.Fatalf("rollback to 1: %s", got)
	}
	for args, want := range map[string]string{
		`{"action":"rollback","resource":"deployments","name":"web","revision":3}`: "already at revision 3",
		`{"action":"rollback","resource":"deployments","name":"web","revision":7}`: "no revision 7 to roll back to (it has 3, 2, 1)",
		`{"action":"rollback","resource":"statefulsets","name":"web"}`:             "rollback needs resource deployments",
	} {
		if got := run(args); !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want %q", args, got, want)
		}
	}
	rc.paused = true
	if got := run(`{"action":"rollback","resource":"deployments","name":"web"}`); !strings.Contains(got, "is paused") {
		t.Errorf("paused: %s", got)
	}
	rc.paused = false
	rc.rsList = `{"items":[]}`
	if got := run(`{"action":"rollback","resource":"deployments","name":"web"}`); !strings.Contains(got, "no revision before 3") {
		t.Errorf("no history: %s", got)
	}
}

// Through the loop, the rollback is dry-run and asked about, and the patch
// approved is the one sent, even if the history changed in between.
func TestRollbackSendsThePatchItDryRan(t *testing.T) {
	rc := &rolloutCluster{}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	sess := newSession(t)
	tool := ApplyTool{M: mgr}
	args := json.RawMessage(`{"action":"rollback","resource":"deployments","name":"web","namespace":"prod"}`)
	if err := tool.Precheck(sess, args); err != nil {
		t.Fatal(err)
	}
	target := tool.Target(sess, args)
	if !strings.Contains(target, `spec.template.spec.containers[0].image: "nginx:1.27" → "nginx:1.25"`) {
		t.Fatalf("target %q", target)
	}
	rc.mu.Lock()
	rc.rsList = strings.ReplaceAll(rsItems, "nginx:1.25", "nginx:9.99")
	rc.mu.Unlock()
	if res := tool.Run(context.Background(), sess, args); res.IsError {
		t.Fatal(res.Content)
	}
	if len(rc.patches) != 2 || !rc.patches[0].dryRun || rc.patches[1].dryRun || rc.patches[0].body != rc.patches[1].body ||
		strings.Contains(rc.patches[1].body, "9.99") {
		t.Fatalf("patches: %+v", rc.patches)
	}
}

// deployStates answers each read of the Deployment with the next state, the
// last one from then on.
func deployStates(states ...string) (http.Handler, *atomic.Int32) {
	var n atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(states) {
			i = len(states) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, states[i])
	}), &n
}

// deployState is generation 5 of a Deployment wanting 3 replicas.
func deployState(observed, updated, available, total int, cond string) string {
	const gen, replicas = 5, 3
	return fmt.Sprintf(`{"kind":"Deployment","metadata":{"name":"web","generation":%d,
	 "annotations":{"deployment.kubernetes.io/revision":"4"}},"spec":{"replicas":%d},
	 "status":{"observedGeneration":%d,"replicas":%d,"updatedReplicas":%d,"availableReplicas":%d,"conditions":[%s]}}`,
		gen, replicas, observed, total, updated, available, cond)
}

func TestWaitForRollout(t *testing.T) {
	defer func(p time.Duration) { waitPoll = p }(waitPoll)
	waitPoll = 10 * time.Millisecond
	get := func(h http.Handler, args string) (string, bool) {
		res := GetTool{M: rolloutMgr(t, h)}.Run(context.Background(), nil, json.RawMessage(args))
		return res.Content, res.IsError
	}
	h, reads := deployStates(
		deployState(4, 3, 3, 3, ""),
		deployState(5, 1, 1, 4, ""),
		deployState(5, 3, 2, 3, ""),
		deployState(5, 3, 3, 3, ""),
	)
	got, isErr := get(h, `{"resource":"deployments","name":"web","wait_seconds":30}`)
	if isErr || !strings.Contains(got, "rolled out (revision 4)") || !strings.Contains(got, "3/3 updated, 3 available") || reads.Load() != 4 {
		t.Fatalf("wait: %v %s after %d reads", isErr, got, reads.Load())
	}

	h, _ = deployStates(deployState(5, 1, 1, 3,
		`{"type":"Progressing","reason":"ProgressDeadlineExceeded","message":"ReplicaSet web-x has timed out progressing."}`))
	if got, isErr := get(h, `{"resource":"deployments","name":"web","wait_seconds":30}`); !isErr || !strings.Contains(got, "exceeded its progress deadline") {
		t.Fatalf("failed rollout: %s", got)
	}

	h, _ = deployStates(deployState(5, 1, 1, 3, ""))
	start := time.Now()
	got, isErr = get(h, `{"resource":"deployments","name":"web","wait_seconds":1}`)
	if !isErr || !strings.Contains(got, "had not rolled out") || !strings.Contains(got, "waiting for updated replicas") {
		t.Fatalf("timed out wait: %s", got)
	}
	if took := time.Since(start); took < time.Second || took > 5*time.Second {
		t.Fatalf("waited %s for a 1s wait", took)
	}

	if got, isErr := get(h, `{"resource":"pods","name":"web","wait_seconds":5}`); !isErr || !strings.Contains(got, "one Deployment's rollout") {
		t.Fatalf("wait on pods: %s", got)
	}
}
