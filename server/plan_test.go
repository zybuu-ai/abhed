package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// planServer is a server whose model proposes a plan with a runbook of
// steps, then makes each call in calls in turn.
func planServer(t *testing.T, runbook string, calls ...string) (*Server, *durableMem, string) {
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
	s := New(Options{Workspace: ws, Config: cfg, Adapter: def, Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{}), Store: st})
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
