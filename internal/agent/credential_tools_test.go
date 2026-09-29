package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	ssh "github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// seenApprover says yes and keeps what each approval prompt showed.
type seenApprover struct {
	mu   sync.Mutex
	args []string
}

func (a *seenApprover) Approve(_ context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.args = append(a.args, tool+" "+string(args)+" "+res.Reason)
	return true, nil
}

// k8s_login and ssh_connect take credentials by name. Whatever a model sends
// instead, the old token argument, a credential where the name belongs, or
// arguments too malformed to read, never reaches the record, the approval
// prompt, or the history sent back to the model. No redactor is set: this
// holds by design, not because the value happened to be in the store.
func TestCredentialToolsKeepValuesOutOfRecordAndHistory(t *testing.T) {
	const token, password = "tok-3f9a-cluster-secret", "pw-77c1-vm-secret"
	leaks := []string{token, password, "LEAK-legacy-token", "LEAK-in-name", "LEAK-malformed", "LEAK-legacy-pw"}

	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"major":"1","minor":"29"}`)
	}))
	defer cluster.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.Certificate().Raw}), 0o600)

	vault := map[string]string{"OCP_TOKEN": token, "VM_PASSWORD": password}
	secret := func(name string) (string, error) {
		if v, ok := vault[name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("no secret named %s", name)
	}
	reg := tools.NewRegistry(
		k8s.LoginTool{M: k8s.NewManager(k8s.Config{CAFile: ca,
			Clusters: []k8s.LoginCluster{{Name: "prod", Server: cluster.URL}}}), Secret: secret},
		ssh.ConnectTool{Secret: secret},
	)
	pol := policy.New(policy.ModeDefault)
	_ = pol.AddAllow("secret(OCP_TOKEN)", "secret(VM_PASSWORD)")

	raw := func(id, name, args string) model.ToolCall {
		return model.ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}
	}
	turns := []scriptedTurn{
		{calls: []model.ToolCall{raw("c1", "k8s_login", `{"cluster":"prod","token_secret":"OCP_TOKEN"}`)}},
		{calls: []model.ToolCall{raw("c2", "k8s_login", `{"cluster":"prod","token":"LEAK-legacy-token","token_secret":"OCP_TOKEN"}`)}},
		{calls: []model.ToolCall{raw("c3", "k8s_login", `{"cluster":"prod","token_secret":"sha256~LEAK-in-name"}`)}},
		{calls: []model.ToolCall{raw("c4", "k8s_login", `{"cluster":"a","Cluster":"b","token":"LEAK-malformed"}`)}},
		{calls: []model.ToolCall{raw("c5", "ssh_connect", `{"addr":"127.0.0.1:1","user":"u","password_secret":"VM_PASSWORD","accept_host_key":true}`)}},
		{calls: []model.ToolCall{raw("c6", "ssh_connect", `{"addr":"127.0.0.1:1","password":"LEAK-legacy-pw","password_secret":"LEAK-in-name"}`)}},
		{text: "done"},
	}
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	approver := &seenApprover{}
	adapter := &scriptedAdapter{turns: turns}
	l := NewLoop(adapter, reg, pol, approver, sess, NewRecorder(store, "s1", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "log in to the cluster and the vm"); err != nil {
		t.Fatal(err)
	}

	evs, _ := store.Events("s1")
	loggedIn := false
	for _, e := range evs {
		for _, leak := range leaks {
			if strings.Contains(string(e.Payload), leak) {
				t.Errorf("%s reached the record in %s: %s", leak, e.Type, e.Payload)
			}
		}
		loggedIn = loggedIn || e.Type == EvObservation && strings.Contains(string(e.Payload), "Authenticated to")
	}
	if !loggedIn {
		t.Fatal("the login by name did not work, so the test proves nothing")
	}
	// Where the token went is in the record and in the prompt, from config.
	where := false
	for _, e := range evs {
		if e.Type == EvActionRequested && strings.Contains(string(e.Payload), `"call_id":"c1"`) {
			var req ActionRequested
			_ = json.Unmarshal(e.Payload, &req)
			where = strings.Contains(req.Target, "cluster prod at "+cluster.URL) &&
				strings.Contains(req.Reason, cluster.URL)
		}
	}
	if !where {
		t.Error("the record does not name the cluster and server the token went to")
	}
	if len(approver.args) == 0 || !strings.Contains(approver.args[0], "cluster prod at "+cluster.URL) {
		t.Errorf("the approval prompt does not name the cluster and server: %v", approver.args)
	}
	for _, a := range approver.args {
		for _, leak := range leaks {
			if strings.Contains(a, leak) {
				t.Errorf("%s was shown for approval: %s", leak, a)
			}
		}
	}
	for _, req := range adapter.gotRequests {
		for _, m := range req.Messages {
			text := m.Content
			for _, c := range m.ToolCalls {
				text += " " + string(c.Args)
			}
			for _, leak := range leaks {
				if strings.Contains(text, leak) {
					t.Errorf("%s was sent back to the model: %s", leak, text)
				}
			}
		}
	}
}

// A secret named for a login needs its own allow rule, as one named for a
// command does: bypass approves calls, it does not hand out credentials.
func TestCredentialToolSecretNeedsItsOwnRule(t *testing.T) {
	var contacted atomic.Bool
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Store(true)
		fmt.Fprint(w, `{"major":"1","minor":"29"}`)
	}))
	defer cluster.Close()
	secret := func(string) (string, error) { return "tok-5d2e-unruled", nil }
	reg := tools.NewRegistry(k8s.LoginTool{M: k8s.NewManager(k8s.Config{Clusters: []k8s.LoginCluster{
		{Name: "prod", Server: cluster.URL, InsecureSkipTLSVerify: true}}}), Secret: secret})
	turns := []scriptedTurn{
		{calls: []model.ToolCall{call("k8s_login", map[string]string{"cluster": "prod", "token_secret": "OCP_TOKEN"})}},
		{text: "done"},
	}
	sess, _ := tools.NewSession(t.TempDir())
	store := NewMemStore()
	l := NewLoop(&scriptedAdapter{turns: turns}, reg, policy.New(policy.ModeBypass), AutoApprove{Yes: true},
		sess, NewRecorder(store, "s1", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "log in"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s1")
	if !hasEvent(evs, EvActionDenied) || contacted.Load() {
		t.Fatal("k8s_login used a secret no rule allows")
	}
}

// markerApprover answers as told and notes whether the helper had already run
// when it was asked.
type markerApprover struct {
	yes    bool
	marker string
	asked  int
	ranBy  bool // the helper had run when the first approval was asked
}

func (a *markerApprover) Approve(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
	if a.asked++; a.asked == 1 {
		_, err := os.Stat(a.marker)
		a.ranBy = err == nil
	}
	return a.yes, nil
}

// A kubeconfig's exec credential helper is a command from the operator's
// config, run outside the sandbox. Building the approval for a k8s_apply must
// not run it: a call that is denied, refused in plan mode, or rejected runs
// nothing, and an approved one runs it once, when it first sends a request.
func TestExecHelperRunsOnlyForARequestThatIsSent(t *testing.T) {
	var deletes atomic.Int32
	cluster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer exec-tok-51c" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		deletes.Add(1)
		fmt.Fprint(w, `{"kind":"Status","status":"Success"}`)
	}))
	defer cluster.Close()

	setup := func(t *testing.T) (kubeconfig, marker string) {
		dir := t.TempDir()
		marker = filepath.Join(dir, "helper-ran")
		helper := filepath.Join(dir, "helper.sh")
		script := "#!/bin/sh\necho ran >> '" + marker + "'\n" +
			`echo '{"apiVersion":"client.authentication.k8s.io/v1","status":{"token":"exec-tok-51c"}}'` + "\n"
		if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		kubeconfig = filepath.Join(dir, "config")
		body := "apiVersion: v1\nclusters:\n- cluster:\n    server: " + cluster.URL + "\n  name: c\n" +
			"contexts:\n- context:\n    cluster: c\n    user: u\n    namespace: prod\n  name: ctx\n" +
			"current-context: ctx\nusers:\n- name: u\n  user:\n    exec:\n      command: " + helper + "\n"
		if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return kubeconfig, marker
	}
	del := func(id string) model.ToolCall {
		return call("k8s_apply", map[string]string{"action": "delete", "resource": "pods", "name": "web-" + id, "context": "ctx"})
	}
	run := func(t *testing.T, pol *policy.Engine, approver Approver, calls int) (string, *MemStore) {
		kube, marker := setup(t)
		reg := tools.NewRegistry(k8s.ApplyTool{M: k8s.NewManager(k8s.Config{Kubeconfig: kube})})
		var turns []scriptedTurn
		for i := 0; i < calls; i++ {
			c := del(fmt.Sprint(i))
			c.ID = fmt.Sprint("c", i)
			turns = append(turns, scriptedTurn{calls: []model.ToolCall{c}})
		}
		turns = append(turns, scriptedTurn{text: "done"})
		sess, _ := tools.NewSession(t.TempDir())
		store := NewMemStore()
		if ma, ok := approver.(*markerApprover); ok {
			ma.marker = marker
		}
		l := NewLoop(&scriptedAdapter{turns: turns}, reg, pol, approver, sess, NewRecorder(store, "s1", ""), DefaultConfig())
		if _, err := l.Run(context.Background(), "delete it"); err != nil {
			t.Fatal(err)
		}
		return marker, store
	}
	ran := func(marker string) int {
		data, err := os.ReadFile(marker)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "ran")
	}

	denyRule := policy.New(policy.ModeDefault)
	_ = denyRule.AddDeny("k8s_apply")
	for name, tc := range map[string]struct {
		pol      *policy.Engine
		approver Approver
	}{
		"denied by a rule": {denyRule, AutoApprove{Yes: true}},
		"plan mode":        {policy.New(policy.ModePlan), AutoApprove{Yes: true}},
		"rejected":         {policy.New(policy.ModeDefault), &markerApprover{yes: false}},
	} {
		t.Run(name, func(t *testing.T) {
			marker, store := run(t, tc.pol, tc.approver, 1)
			evs, _ := store.Events("s1")
			if !hasEvent(evs, EvActionDenied) {
				t.Fatal("the call was not refused, so the test proves nothing")
			}
			if n := ran(marker); n != 0 {
				t.Fatalf("the exec credential helper ran %d times for a call that was refused", n)
			}
			if ma, ok := tc.approver.(*markerApprover); ok && ma.ranBy {
				t.Fatal("the helper ran before the person answered")
			}
		})
	}

	t.Run("approved", func(t *testing.T) {
		before := deletes.Load()
		ma := &markerApprover{yes: true}
		marker, store := run(t, policy.New(policy.ModeDefault), ma, 2)
		if ma.ranBy {
			t.Fatal("the helper ran before the person answered")
		}
		if n := ran(marker); n != 1 {
			t.Fatalf("the helper ran %d times for two approved writes, want once", n)
		}
		if got := deletes.Load() - before; got != 2 {
			evs, _ := store.Events("s1")
			t.Fatalf("%d writes reached the cluster with the helper's token, want 2: %v", got, evs)
		}
	})
}
