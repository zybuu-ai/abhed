package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// callsSkill is the session's model: it calls the skill once, then answers.
type callsSkill struct {
	mu sync.Mutex
	n  int
}

func (*callsSkill) Name() string                           { return "scripted" }
func (*callsSkill) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (*callsSkill) CountTokens(model.Request) (int, error) { return 0, nil }
func (a *callsSkill) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	a.mu.Lock()
	a.n++
	first := a.n == 1
	a.mu.Unlock()
	ch := make(chan model.Chunk, 3)
	if first {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{ID: "c1", Name: "skill", Args: json.RawMessage(`{"name":"gather"}`)}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// pipeModel answers a pipeline's model steps and keeps the prompts it was sent.
type pipeModel struct {
	mu      sync.Mutex
	prompts []string
}

func (*pipeModel) Name() string                           { return "step" }
func (*pipeModel) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (*pipeModel) CountTokens(model.Request) (int, error) { return 0, nil }
func (m *pipeModel) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	m.mu.Lock()
	for _, msg := range req.Messages {
		m.prompts = append(m.prompts, msg.Content)
	}
	m.mu.Unlock()
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "summary"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// askLog records what was put to it, and whether two asks overlapped, and answers no.
type askLog struct {
	mu      sync.Mutex
	seen    []policy.Result
	open    int
	overlap bool
}

func (a *askLog) Approve(_ context.Context, _ string, _ json.RawMessage, res policy.Result) (bool, error) {
	a.mu.Lock()
	a.seen = append(a.seen, res)
	a.open++
	a.overlap = a.overlap || a.open > 1
	a.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	a.mu.Lock()
	a.open--
	a.mu.Unlock()
	return false, nil
}

const stepSecret = "tok-5c1e7a9b"

type pipeRun struct {
	ws    string
	store *agent.MemStore
	model *pipeModel
	reply string
}

// runPipeline runs one session whose model calls a skill declaring pipeline,
// with WS in it standing for the workspace.
func runPipeline(t *testing.T, pipelineJSON string, pol *policy.Engine, appr agent.Approver) pipeRun {
	t.Helper()
	root, ws := t.TempDir(), t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	dir := filepath.Join(root, "gather")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: gather\ndescription: use when gathering\n---\nWrite the answer.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.json"), []byte(strings.ReplaceAll(pipelineJSON, "WS", ws)), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, errs := skills.Load([]string{root})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("STEP_TOKEN", stepSecret); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("key="+stepSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	holder := &agent.LoopHolder{}
	sm := &pipeModel{}
	registry := tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{})
	registry.Add(skills.Tool{R: reg, RunPipeline: pipelineRunner(sm, holder), Input: func() string { return "question" }})

	store := agent.NewMemStore()
	rec := agent.NewRecorder(store, "s1", "")
	rec.Redact = vault.Redactor()
	loop := agent.NewLoop(&callsSkill{}, registry, pol, appr, sess, rec, agent.DefaultConfig())
	holder.Set(loop)
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	reply := ""
	for _, m := range loop.Messages() {
		if m.Role == model.RoleTool {
			reply = m.Content
		}
	}
	return pipeRun{ws: ws, store: store, model: sm, reply: reply}
}

// stepEvents returns the pipeline step's decision and the requested event.
func stepEvents(t *testing.T, store *agent.MemStore) (agent.ActionRequested, []agent.Event) {
	t.Helper()
	evs, _ := store.Events("s1")
	var req agent.ActionRequested
	var mine []agent.Event
	for _, e := range evs {
		if e.Type == agent.EvActionRequested {
			var a agent.ActionRequested
			_ = json.Unmarshal(e.Payload, &a)
			if a.Via != "" {
				req = a
			}
		}
		if strings.Contains(string(e.Payload), `"call_id":"step_`) {
			mine = append(mine, e)
		}
	}
	if req.Via != "skill gather pipeline" {
		t.Fatalf("the step was not recorded as issued by the pipeline: %+v", req)
	}
	return req, mine
}

func decided(evs []agent.Event) (agent.EventType, map[string]any) {
	for _, e := range evs {
		if e.Type == agent.EvActionDenied || e.Type == agent.EvActionApproved {
			var d map[string]any
			_ = json.Unmarshal(e.Payload, &d)
			return e.Type, d
		}
	}
	return "", nil
}

func onePipeline(tool, args string) string {
	return `{"stages":[{"name":"act","steps":[{"kind":"tool","tool":"` + tool + `","args":` + args + `,"output":"out","required":true}]}]}`
}

func TestPipelineStepDenyRuleRefusesAndRecords(t *testing.T) {
	pol := policy.New(policy.ModeBypass)
	if err := pol.AddDeny("bash(touch *)"); err != nil {
		t.Fatal(err)
	}
	r := runPipeline(t, onePipeline("bash", `{"command":"touch pwned","description":"x"}`), pol, agent.AutoApprove{Yes: true})
	if _, err := os.Stat(filepath.Join(r.ws, "pwned")); err == nil {
		t.Fatal("a pipeline step ran a command a deny rule refuses")
	}
	req, evs := stepEvents(t, r.store)
	if req.Tool != "bash" {
		t.Fatalf("recorded %q, want bash", req.Tool)
	}
	if typ, d := decided(evs); typ != agent.EvActionDenied || d["step"] != "deny" {
		t.Fatalf("want a deny by rule on record, got %s %v", typ, d)
	}
}

func TestPipelineStepAskWithNoApproverIsRefused(t *testing.T) {
	for name, appr := range map[string]agent.Approver{"headless": agent.AutoApprove{Yes: false}, "none": nil} {
		t.Run(name, func(t *testing.T) {
			r := runPipeline(t, onePipeline("write", `{"path":"WS/planted.txt","content":"x"}`), policy.New(policy.ModeDefault), appr)
			if _, err := os.Stat(filepath.Join(r.ws, "planted.txt")); err == nil {
				t.Fatal("a step needing approval ran with no one to approve it")
			}
			_, evs := stepEvents(t, r.store)
			if typ, d := decided(evs); typ != agent.EvActionDenied || d["step"] != "default" {
				t.Fatalf("want the asked step denied on record, got %s %v", typ, d)
			}
		})
	}
}

func TestPipelineDestructiveStepAsksForConfirmation(t *testing.T) {
	appr := &askLog{}
	r := runPipeline(t, onePipeline("bash", `{"command":"rm -rf keep","description":"x"}`), policy.New(policy.ModeBypass), appr)
	if len(appr.seen) != 1 || appr.seen[0].Step != "destructive" {
		t.Fatalf("a destructive step was not put to the approver: %+v", appr.seen)
	}
	_, evs := stepEvents(t, r.store)
	if typ, _ := decided(evs); typ != agent.EvActionDenied {
		t.Fatalf("want the declined step denied on record, got %s", typ)
	}
}

func TestPipelinePlanModeRefusesMutatingStep(t *testing.T) {
	r := runPipeline(t, onePipeline("write", `{"path":"WS/planted.txt","content":"x"}`), policy.New(policy.ModePlan), agent.AutoApprove{Yes: true})
	if _, err := os.Stat(filepath.Join(r.ws, "planted.txt")); err == nil {
		t.Fatal("plan mode let a pipeline step change a file")
	}
	_, evs := stepEvents(t, r.store)
	if typ, d := decided(evs); typ != agent.EvActionDenied || d["step"] != "mode" {
		t.Fatalf("want a plan-mode deny on record, got %s %v", typ, d)
	}
}

func TestPipelineModelStepSeesRedactedToolOutput(t *testing.T) {
	p := `{"stages":[
	  {"name":"read","steps":[{"kind":"tool","tool":"read","args":{"path":"WS/notes.txt"},"output":"notes","required":true}]},
	  {"name":"sum","steps":[{"kind":"model","prompt":"Summarise: {{notes}}","output":"summary"}]}]}`
	r := runPipeline(t, p, policy.New(policy.ModeDefault), agent.AutoApprove{Yes: false})
	if len(r.model.prompts) == 0 {
		t.Fatalf("the model step never ran: %s", r.reply)
	}
	for _, prompt := range r.model.prompts {
		if strings.Contains(prompt, stepSecret) {
			t.Fatalf("a model step was sent a secret value: %q", prompt)
		}
	}
	if !strings.Contains(strings.Join(r.model.prompts, "\n"), "[secret:STEP_TOKEN]") {
		t.Fatalf("the model step did not see the redacted output: %q", r.model.prompts)
	}
	if strings.Contains(r.reply, stepSecret) {
		t.Fatal("the secret reached the session model through the pipeline")
	}
}

func TestPipelineStepInRecordAndHawkEYE(t *testing.T) {
	r := runPipeline(t, onePipeline("read", `{"path":"WS/notes.txt"}`), policy.New(policy.ModeDefault), agent.AutoApprove{Yes: false})
	req, evs := stepEvents(t, r.store)
	if typ, _ := decided(evs); typ != agent.EvActionApproved {
		t.Fatalf("want the read-only step approved, got %s", typ)
	}
	observed := false
	for _, e := range evs {
		observed = observed || e.Type == agent.EvObservation
	}
	if !observed {
		t.Fatal("the step's result is not in the record")
	}
	all, _ := r.store.Events("s1")
	var found *hawkeye.Call
	rep := hawkeye.Analyze("s1", all)
	for i := range rep.Calls {
		if rep.Calls[i].CallID == req.CallID {
			found = &rep.Calls[i]
		}
	}
	if found == nil || !found.Ran || found.Via != "skill gather pipeline" || found.Decision != "allowed" {
		t.Fatalf("HawkEYE does not show the pipeline step: %+v", found)
	}
	if !strings.Contains(hawkeye.Text(rep), "via skill gather pipeline") {
		t.Fatal("the text report does not say the call came from the pipeline")
	}
}

func TestPipelineWithNoLoopIsRefused(t *testing.T) {
	run := pipelineRunner(&pipeModel{}, &agent.LoopHolder{})
	s := &skills.Skill{Name: "gather", Pipeline: json.RawMessage(onePipeline("read", `{"path":"x"}`))}
	if _, err := run(context.Background(), s, "q"); !errors.Is(err, agent.ErrNoLoop) {
		t.Fatalf("a pipeline ran with no loop to check its steps: %v", err)
	}
}

func TestPipelineCallingASkillIsRefused(t *testing.T) {
	r := runPipeline(t, onePipeline("skill", `{"name":"gather"}`), policy.New(policy.ModeBypass), agent.AutoApprove{Yes: true})
	if !strings.Contains(r.reply, "cannot run a skill") {
		t.Fatalf("a pipeline that calls a skill was not refused: %q", r.reply)
	}
}

// Steps in one stage run together, but a person answers one ask at a time.
func TestPipelineAsksComeOneAtATime(t *testing.T) {
	appr := &askLog{}
	p := `{"stages":[{"name":"act","steps":[
	  {"kind":"tool","tool":"bash","args":{"command":"rm -rf a","description":"x"}},
	  {"kind":"tool","tool":"bash","args":{"command":"rm -rf b","description":"x"}},
	  {"kind":"tool","tool":"bash","args":{"command":"rm -rf c","description":"x"}}]}]}`
	runPipeline(t, p, policy.New(policy.ModeBypass), appr)
	if len(appr.seen) != 3 || appr.overlap {
		t.Fatalf("asks: %d, overlapped: %v", len(appr.seen), appr.overlap)
	}
}
