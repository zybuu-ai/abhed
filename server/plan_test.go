package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// planServer is a server whose model proposes a plan with a runbook of
// steps, then makes each call in calls in turn.
func planServer(t *testing.T, runbook string, calls ...string) (*Server, *durableMem, string) {
	return planServerWith(t, nil, runbook, calls...)
}

// planServerWith is planServer with its options adjusted by tune.
func planServerWith(t *testing.T, tune func(*Options), runbook string, calls ...string) (*Server, *durableMem, string) {
	t.Helper()
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		i := int(n.Add(1))
		name, args := "", ""
		switch {
		case i == 1:
			name, args = "exit_plan", `{"plan":"1. notes","runbook":`+strings.ReplaceAll(runbook, "WS", ws)+`}`
		case i-2 < len(calls):
			name, args = "write", strings.ReplaceAll(calls[i-2], "WS", ws)
		}
		if name == "" {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.Itoa(i)+
			`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Suggest.Enabled = false
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{"a": {Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}}
	def, err := cfg.Model.Providers["a"].Adapter()
	if err != nil {
		t.Fatal(err)
	}
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	opts := Options{Workspace: ws, Config: cfg, Adapter: def, Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{}), Store: st}
	if tune != nil {
		tune(&opts)
	}
	s := New(opts)
	return s, st, ws
}

func proposedHash(t *testing.T, evs []agent.Event) string {
	t.Helper()
	for _, ev := range evs {
		if ev.Type == agent.EvPlanProposed {
			var p agent.PlanProposed
			_ = json.Unmarshal(ev.Payload, &p)
			if p.Runbook != nil {
				return p.Runbook.Hash
			}
		}
	}
	t.Fatal("no plan with a runbook was proposed")
	return ""
}

// The workbench answers a proposed plan: a wrong hash or a mode the server
// is not configured for is refused; approving binds the runbook and changes
// the mode via plan-exit, and the listed step then runs without an ask.
func TestWorkbenchPlanDecision(t *testing.T) {
	s, st, ws := planServer(t, `[{"tool":"write","target":"WS/notes.txt","args":{"content":"x"}}]`,
		`{"path":"WS/notes.txt","content":"x"}`)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"plan it","mode":"plan"}`))
	hash := proposedHash(t, turnsEnded(t, st, id, 1))
	path := "/v1/sessions/" + id + "/plan"
	if w := call(t, s, "POST", path, `{"decision":"approve","plan_hash":"`+strings.Repeat("0", 64)+`"}`); w.Code != http.StatusConflict {
		t.Fatalf("wrong hash: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", path, `{"decision":"accept-edits","plan_hash":"`+hash+`"}`); w.Code != http.StatusForbidden {
		t.Fatalf("accept-edits under default: %d %s", w.Code, w.Body.String())
	}
	w := call(t, s, "POST", path, `{"decision":"approve","plan_hash":"`+hash+`"}`)
	var out struct{ Mode, Message string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || out.Mode != "default" || !strings.Contains(out.Message, hash[:12]) {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", path, `{"decision":"approve","plan_hash":"`+hash+`"}`); w.Code != http.StatusConflict {
		t.Fatalf("answered twice: %d", w.Code)
	}
	body, _ := json.Marshal(map[string]string{"prompt": out.Message})
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", string(body)); w.Code >= 300 {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	evs := turnsEnded(t, st, id, 2)
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "x" {
		t.Fatalf("the step did not run: %v", err)
	}
	var kinds []string
	for _, ev := range evs {
		switch ev.Type {
		case agent.EvPlanDecided, agent.EvModeChanged, agent.EvPlanStep:
			kinds = append(kinds, string(ev.Type)+":"+string(ev.Payload))
		}
	}
	joined := strings.Join(kinds, "\n")
	for _, want := range []string{`"via":"plan-exit"`, `"plan_hash":"` + hash, "plan.step", `"by":"plan"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("no %s in:\n%s", want, joined)
		}
	}
}

// A runbook with a step that cannot be undone is not bound until that is confirmed.
func TestWorkbenchPlanNeedsUndoConfirm(t *testing.T) {
	s, st, _ := planServer(t, `[{"tool":"bash","command":"rm -rf build"}]`)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"plan it","mode":"plan"}`))
	hash := proposedHash(t, turnsEnded(t, st, id, 1))
	path := "/v1/sessions/" + id + "/plan"
	if w := call(t, s, "POST", path, `{"decision":"approve","plan_hash":"`+hash+`"}`); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("unconfirmed: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", path, `{"decision":"approve","plan_hash":"`+hash+`","confirm_no_undo":true}`); w.Code != http.StatusOK {
		t.Fatalf("confirmed: %d %s", w.Code, w.Body.String())
	}
}

// The workbench's plan card shows each runbook step as it will run, asks
// again about steps with no undo, and sends the answer for this plan's hash.
func TestIDEPlanCard(t *testing.T) {
	harness := `import { El } from './dom.mjs';
globalThis.__root = new El('div');
globalThis.__posts = []; globalThis.__sent = [];
El.prototype.addEventListener = function(type, f){ (this.on = this.on || {})[type] = f; };
Object.defineProperty(El.prototype, 'firstChild', {get(){ return this.childNodes[0] || null; }});
El.prototype.removeChild = function(n){ this.childNodes.splice(this.childNodes.indexOf(n), 1); n.parentNode = null; return n; };
El.prototype.remove = function(){ const p = this.parentNode; if(p) p.removeChild(this); };
El.prototype.focus = function(){};
let current = 's1';
const asks = new Map();
const q = {value: '', focus(){}};
const $ = () => q;
const tx = () => __root;
const add = n => { __root.appendChild(n); };
const focusSoon = () => {}, notify = () => {};
globalThis.requestAnimationFrame = () => {};
const api = async (path, opts) => { __posts.push(JSON.parse(opts.body)); return {mode: 'default', message: 'carry it out'}; };
const send = () => { __sent.push(q.value); q.value = ''; };
`
	if out, err := runConsoleCases(t, "ide-plan", harness, "ide_plan_cases.mjs"); err != nil {
		t.Fatalf("the plan card failed:\n%s", out)
	}
}

// Under an approval gate the runbook is one request the gate decides: the
// owner's own approval is refused and binds nothing, one approver is held,
// and the second settles it, recorded with who answered.
func TestWorkbenchPlanThroughTheApprovalGate(t *testing.T) {
	gate := &fourEyes{approvers: map[string]bool{"bob": true, "carol": true}}
	s, st, _ := planServerWith(t, func(o *Options) {
		o.Config.Auth.Mode = "oidc"
		o.Auth = &auth.Middleware{PublicPaths: PublicPaths(), Verifier: tokens{
			"alice": {Subject: "alice", Tenant: "default"}, "bob": {Subject: "bob", Tenant: "default"},
			"carol": {Subject: "carol", Tenant: "default"}, "dave": {Subject: "dave", Tenant: "default"},
		}}
		o.ApprovalGate = gate
	}, `[{"tool":"write","target":"WS/notes.txt","args":{"content":"x"}}]`)
	send := func(token, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}
	id := sessionOf(t, send("alice", "/v1/sessions", `{"prompt":"plan it","mode":"plan"}`))
	hash := proposedHash(t, turnsEnded(t, st, id, 1))
	path, body := "/v1/sessions/"+id+"/plan", `{"decision":"approve","plan_hash":"`+hash+`"}`
	bound := func() bool { _, _, ok := s.anyLive(t).Loop.BoundRunbook(); return ok }

	if w := send("alice", path, body); w.Code != http.StatusForbidden || bound() {
		t.Fatalf("the owner approved their own runbook: %d %s", w.Code, w.Body)
	}
	if w := send("dave", path, body); w.Code != http.StatusNotFound {
		t.Fatalf("a non-approver reached the plan: %d", w.Code)
	}
	if w := send("bob", path, body); w.Code != http.StatusAccepted || bound() {
		t.Fatalf("one approval of two: %d %s", w.Code, w.Body)
	}
	if _, ok := s.anyLive(t).Loop.PendingPlan(); !ok {
		t.Fatal("a held answer let the plan go")
	}
	if w := send("carol", path, body); w.Code != http.StatusOK || !bound() {
		t.Fatalf("the second approval: %d %s", w.Code, w.Body)
	}
	evs, _ := st.Events(id)
	var decided []agent.PlanDecided
	for _, e := range evs {
		if e.Type == agent.EvPlanDecided {
			var p agent.PlanDecided
			_ = json.Unmarshal(e.Payload, &p)
			decided = append(decided, p)
		}
	}
	if len(decided) != 1 || decided[0].By != "carol" || decided[0].PlanHash != hash {
		t.Fatalf("plan.decided %+v", decided)
	}
	for _, a := range gate.seen {
		if a.Request != "plan:"+hash || a.Tool != "exit_plan" || !slices.Equal(a.StepTools, []string{"write"}) {
			t.Fatalf("the gate was told %+v", a)
		}
	}
}

// The approval queue lists a waiting runbook as one request, plan:<hash>,
// and POST /approve answers it through the gate as POST /plan does: the
// owner's own approval is refused, a step with no undo must be confirmed,
// one approver is held, and the second binds it, with the plan's message
// queued for the owner's next run.
func TestQueueAnswersARunbook(t *testing.T) {
	gate := &fourEyes{approvers: map[string]bool{"bob": true, "carol": true}}
	s, st, _ := planServerWith(t, func(o *Options) {
		o.Config.Auth.Mode = "oidc"
		o.Auth = &auth.Middleware{PublicPaths: PublicPaths(), Verifier: tokens{
			"alice": {Subject: "alice", Tenant: "default"}, "bob": {Subject: "bob", Tenant: "default"},
			"carol": {Subject: "carol", Tenant: "default"},
		}}
		o.ApprovalGate = gate
	}, `[{"tool":"write","target":"WS/notes.txt","args":{"content":"x"}},{"tool":"bash","command":"rm -rf build"}]`)
	send := func(token, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}
	id := sessionOf(t, send("alice", "/v1/sessions", `{"prompt":"plan it","mode":"plan"}`))
	hash := proposedHash(t, turnsEnded(t, st, id, 1))

	asks := s.PendingAsks(func(string, string) bool { return true })
	if len(asks) != 1 || asks[0].Request != "plan:"+hash || asks[0].RequestID != "plan:"+hash || asks[0].Tool != "exit_plan" ||
		asks[0].Owner != "alice" || !strings.Contains(string(asks[0].Args), `"2. bash rm -rf build (no undo:`) || !strings.Contains(string(asks[0].Args), "1. write ") || !strings.Contains(asks[0].Reason, "1 cannot be undone") ||
		asks[0].Since.IsZero() || !slices.Equal(asks[0].StepTools, []string{"write", "bash"}) {
		t.Fatalf("queue %+v", asks)
	}
	path := "/v1/sessions/" + id + "/approve"
	approve := func(token string, undo bool) *httptest.ResponseRecorder {
		return send(token, path, fmt.Sprintf(`{"approved":true,"request_id":"plan:%s","confirm_no_undo":%v}`, hash, undo))
	}
	live := s.anyLive(t)
	bound := func() bool { _, _, ok := live.Loop.BoundRunbook(); return ok }
	if w := approve("alice", true); w.Code != http.StatusForbidden || bound() {
		t.Fatalf("the owner approved their own runbook: %d %s", w.Code, w.Body)
	}
	if w := approve("bob", false); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("a step with no undo was not confirmed: %d %s", w.Code, w.Body)
	}
	if w := send("bob", path, `{"approved":true,"scope":"bash(*)","request_id":"plan:`+hash+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a scope on a runbook: %d", w.Code)
	}
	if w := approve("bob", true); w.Code != http.StatusAccepted || bound() {
		t.Fatalf("one approval of two: %d %s", w.Code, w.Body)
	}
	if w := approve("carol", true); w.Code != http.StatusNoContent || !bound() {
		t.Fatalf("the second approval: %d %s", w.Code, w.Body)
	}
	if q := live.Loop.Queued(); len(q) != 1 || !strings.Contains(q[0].Text, hash[:12]) {
		t.Fatalf("the plan's message was not queued for the owner: %+v", q)
	}
	if len(s.PendingAsks(func(string, string) bool { return true })) != 0 {
		t.Fatal("an answered runbook is still listed")
	}
	// The owner's next message runs with the plan's message read first.
	if w := send("alice", "/v1/sessions/"+id+"/messages", `{"prompt":"go"}`); w.Code >= 300 {
		t.Fatalf("message: %d %s", w.Code, w.Body)
	}
	evs := turnsEnded(t, st, id, 2)
	read := false
	for _, e := range evs {
		read = read || e.Type == agent.EvUserMessage && strings.Contains(string(e.Payload), hash[:12])
	}
	if !read {
		t.Fatal("the queued plan message was not read by the next run")
	}
	for _, e := range evs {
		if e.Type == agent.EvPlanDecided && !strings.Contains(string(e.Payload), `"by":"carol"`) {
			t.Fatalf("plan.decided %s", e.Payload)
		}
	}
}

// An answer the record does not take changes nothing: the session stays in
// plan mode, no runbook is bound, and the plan still waits.
func TestWorkbenchPlanAnswerNotRecorded(t *testing.T) {
	s, st, _ := planServer(t, `[{"tool":"write","target":"WS/notes.txt","args":{"content":"x"}}]`)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"plan it","mode":"plan"}`))
	hash := proposedHash(t, turnsEnded(t, st, id, 1))
	live := s.anyLive(t)
	live.Loop.Recorder.Gate = func() error { return errors.New("the record is gone") }
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/plan", `{"decision":"approve","plan_hash":"`+hash+`"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	if _, _, bound := live.Loop.BoundRunbook(); bound || live.Loop.Policy.Mode != "plan" {
		t.Fatalf("bound %v, mode %s", bound, live.Loop.Policy.Mode)
	}
	if _, ok := live.Loop.PendingPlan(); !ok {
		t.Fatal("the plan no longer waits")
	}
}
