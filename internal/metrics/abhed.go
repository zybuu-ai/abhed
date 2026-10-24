package metrics

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/fence/seccomp"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Steps are the policy steps a decision names, as the loop records them; a
// step outside this list is counted as Other.
var Steps = []string{
	"hook", "deny", "destructive", "screen", "ask", "mode", "allow", "default", "monitor",
	"handover", "plan", "precheck", "unknown", "withheld", "owner", "args", "unanswered",
	"interrupted", "attachment", "state",
}

// OllamaOutcomes are how a person's start, install or pull of Ollama ended.
var OllamaOutcomes = []string{agent.OllamaOK, agent.OllamaFailed, agent.OllamaDeclined, agent.OllamaRefused, agent.OllamaCancelled, agent.OllamaManual}

// Methods are the HTTP methods a request is counted by; any other is Other.
var Methods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"}

// Approvers are who an approval or denial names as deciding.
var Approvers = []string{agent.ByPolicy, agent.ByReviewer, agent.ByUser, agent.BySessionScope, agent.ByHeadless, agent.BySystem}

// VerdictCodes are the supervisor's codes; Skipped and Unavailable stand for
// a call the judge did not, or could not, answer.
var VerdictCodes = []string{
	"ok", "unverified-target", "unverified-host", "unknown-program", "out-of-remit",
	"credential-egress", "honeytoken", "skipped", "unavailable",
}

// EgressKinds are what an egress decision is about.
var EgressKinds = []string{"auth", "connect", "dns", "http", "https", "import", "note", "request", "shell", "summary"}

// Abhed is the set of metrics `abhed serve` exports. Every label is from a
// fixed list or a configured name (a tool, a model, a route pattern); none
// carries a path, a command, a host or anything a person or model wrote.
type Abhed struct {
	Registry *Registry

	decisions     *Counter
	approvals     *Counter
	verdicts      *Counter
	verdictTime   *Histogram
	egress        *Counter
	egressBytes   *Counter
	toolTime      *Histogram
	modelCalls    *Counter
	modelTime     *Histogram
	firstToken    *Histogram
	tokens        *Counter
	sessions      *Counter
	sessionsEnded *Counter
	bound         *Counter
	fenceLimits   *Counter
	sandboxOps    *Counter
	recordTime    *Histogram
	recordErrors  *Counter
	requests      *Counter
	requestTime   *Histogram
	auditEvents   *Counter
	plans         *Counter
	scopes        *Counter
	claims        *Counter
	corrections   *Counter
	replans       *Counter
	repairs       *Counter
	refused       *Counter
	planted       *Counter
	fenceDenied   *Counter
	ollama        *Counter

	mu      sync.Mutex
	calls   map[string]*call
	running func() int
}

// call is what the decision of one tool call needs from its request.
type call struct {
	tool     string
	asked    bool
	approved bool
}

// maxCalls bounds the calls waiting for their decision or result; past it
// the oldest are forgotten, which costs a label, never a count.
const maxCalls = 10000

// New makes the metric set on a new registry.
func New() *Abhed {
	r := NewRegistry()
	m := &Abhed{
		Registry: r,
		decisions: r.Counter("abhed_policy_decisions_total",
			"Tool calls by the policy decision (allow, ask, deny), the step that made it, and the tool.", "decision", "step", "tool"),
		approvals: r.Counter("abhed_approvals_total",
			"Asked calls by how they were answered (approved, rejected, unanswered, interrupted, withheld, owner) and by whom.", "result", "by", "tool"),
		verdicts: r.Counter("abhed_supervisor_verdicts_total",
			"Supervisor verdicts by code, with the decision before and after it.", "code", "before", "after"),
		verdictTime: r.Histogram("abhed_supervisor_judge_seconds",
			"How long the supervisor took to judge a call.", DurationBuckets),
		egress: r.Counter("abhed_egress_decisions_total",
			"Egress decisions by kind and decision, a summary's repeats included.", "kind", "decision"),
		egressBytes: r.Counter("abhed_egress_bytes_total",
			"Bytes through the egress proxy, by direction.", "direction"),
		toolTime: r.Histogram("abhed_tool_duration_seconds",
			"How long an approved tool call ran, by tool and outcome (ok, error, not_run, unknown).", DurationBuckets, "tool", "outcome"),
		modelCalls: r.Counter("abhed_model_calls_total",
			"Model round trips by model, purpose and outcome (ok, error, cut_off).", "model", "purpose", "outcome"),
		modelTime: r.Histogram("abhed_model_call_seconds",
			"Model round-trip latency, by model and purpose.", DurationBuckets, "model", "purpose"),
		firstToken: r.Histogram("abhed_model_first_token_seconds",
			"Time to the model's first token, by model and purpose.", DurationBuckets, "model", "purpose"),
		tokens: r.Counter("abhed_model_tokens_total",
			"Model tokens by model and kind (in, out, cached).", "model", "kind"),
		sessions: r.Counter("abhed_sessions_started_total",
			"Sessions started."),
		sessionsEnded: r.Counter("abhed_sessions_ended_total",
			"Session runs ended, by reason.", "reason"),
		bound: r.Counter("abhed_sandbox_bound_total",
			"Sessions bound to a control-plane sandbox, by driver and tier.", "driver", "tier"),
		fenceLimits: r.Counter("abhed_fence_limits_total",
			"Fenced commands that ran into a limit, by limit (memory, processes).", "limit"),
		sandboxOps: r.Counter("abhed_sandbox_operations_total",
			"Control-plane sandbox operations by action and outcome (ok, failed).", "action", "outcome"),
		recordTime: r.Histogram("abhed_record_write_seconds",
			"How long a write to the record took, by record (events, audit).", DurationBuckets, "record"),
		recordErrors: r.Counter("abhed_record_write_errors_total",
			"Writes to the record that failed, by record (events, audit).", "record"),
		requests: r.Counter("abhed_http_requests_total",
			"API requests by route pattern, method and status code.", "route", "method", "code"),
		requestTime: r.Histogram("abhed_http_request_seconds",
			"API request latency by route pattern; streams last as long as they are open.", DurationBuckets, "route"),
		auditEvents: r.Counter("abhed_audit_events_total",
			"Control-plane audit events written, by source (api, sandbox) and outcome (requested, ok, failed).", "source", "outcome"),
		plans: r.Counter("abhed_plan_decisions_total",
			"Plan answers by decision (accept, keep-planning) and by whom (person, operator).", "decision", "via"),
		scopes: r.Counter("abhed_approval_scopes_granted_total",
			"Session-wide \"always allow\" scopes granted."),
		claims: r.Counter("abhed_answer_claims_total",
			"Checked answer claims by verdict (verified, unverified, contradicted).", "verdict"),
		corrections: r.Counter("abhed_supervisor_corrections_total",
			"Corrections the supervisor sent the model, by code.", "code"),
		replans: r.Counter("abhed_supervisor_replans_total",
			"Re-plan notes the supervisor sent after calls failed in a row."),
		repairs: r.Counter("abhed_record_repairs_total",
			"Torn record tails cut off and recorded when a session was opened."),
		refused: r.Counter("abhed_config_refused_total",
			"Settings a session refused or narrowed, by kind (refused, narrowed).", "kind"),
		planted: r.Counter("abhed_sandbox_planted_total",
			"State a command planted that the sandbox found and moved, by what (abhed_state, git).", "what"),
		fenceDenied: r.Counter("abhed_fence_denied_total",
			"System calls the fence refused fenced commands, by source (seccomp) and call.", "source", "call"),
		ollama: r.Counter("abhed_ollama_actions_total",
			"A person's attempts to start, install or pull a local Ollama, by op (start, install, pull) and outcome (ok, failed, declined, refused, cancelled, manual).", "op", "outcome"),
		calls: map[string]*call{},
	}
	r.GaugeFunc("abhed_sessions_live", "Sessions this process holds, as /v1/health counts them.", m.runningSessions)
	return m
}

// OllamaAction counts one attempt to start, install or pull a local Ollama.
// A recorded ollama.* event is counted as it is written; a caller counts here
// an attempt no session's record took.
func (m *Abhed) OllamaAction(op, outcome string) {
	m.ollama.Inc(oneOf(op, []string{"start", "install", "pull"}), oneOf(outcome, OllamaOutcomes))
}

// RunningSessions makes fn the source of the number of sessions this
// process holds; the last server to call it is the one counted.
func (m *Abhed) RunningSessions(fn func() int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = fn
}

func (m *Abhed) runningSessions() float64 {
	m.mu.Lock()
	fn := m.running
	m.mu.Unlock()
	if fn == nil {
		return 0
	}
	return float64(fn())
}

// RecordWrite counts one write to the named record.
func (m *Abhed) RecordWrite(record string, d time.Duration, err error) {
	m.recordTime.Observe(d.Seconds(), record)
	if err != nil {
		m.recordErrors.Inc(record)
	}
}

// Request counts one API request.
func (m *Abhed) Request(route, method string, status int, d time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	m.requests.Inc(route, oneOf(method, Methods), strconv.Itoa(status))
	m.requestTime.Observe(d.Seconds(), route)
}

// Audit counts one control-plane audit event and its write. A sandbox
// operation's outcome is counted under its action; the API's own are not.
func (m *Abhed) Audit(action, outcome string, sandbox bool, d time.Duration, err error) {
	m.RecordWrite("audit", d, err)
	if err != nil {
		return
	}
	source := "api"
	if sandbox {
		source = "sandbox"
	}
	m.auditEvents.Inc(source, outcome)
	if sandbox && outcome != "requested" {
		m.sandboxOps.Inc(action, outcome)
	}
}

// Observe counts what one recorded event says. It is an event tap: it
// returns at once and never blocks the appending goroutine for long.
func (m *Abhed) Observe(ev agent.Event) {
	switch string(ev.Type) {
	case string(agent.EvActionRequested):
		var p agent.ActionRequested
		if json.Unmarshal(ev.Payload, &p) == nil {
			m.request(ev.SessionID, p)
		}
	case string(agent.EvActionApproved), string(agent.EvActionDenied):
		var p map[string]string
		if json.Unmarshal(ev.Payload, &p) == nil {
			m.decided(ev.SessionID, ev.Type == agent.EvActionApproved, p)
		}
	case string(agent.EvObservation):
		var p agent.Observation
		if json.Unmarshal(ev.Payload, &p) == nil {
			m.observed(ev.SessionID, p)
		}
	case string(agent.EvMonitorVerdict):
		var p agent.MonitorVerdict
		if json.Unmarshal(ev.Payload, &p) == nil {
			m.verdict(p)
		}
	case string(agent.EvEgressDecision):
		var p struct {
			Kind     string `json:"kind"`
			Decision string `json:"decision"`
			BytesIn  int64  `json:"bytes_in"`
			BytesOut int64  `json:"bytes_out"`
			Repeats  int64  `json:"repeats"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			m.egress.Add(float64(1+max(p.Repeats, 0)), oneOf(p.Kind, EgressKinds), oneOf(p.Decision, []string{"allow", "deny", "would_deny"}))
			m.egressBytes.Add(float64(max(p.BytesIn, 0)), "in")
			m.egressBytes.Add(float64(max(p.BytesOut, 0)), "out")
		}
	case string(agent.EvModelCall):
		var p agent.ModelCall
		if json.Unmarshal(ev.Payload, &p) == nil {
			m.modelCall(p)
		}
	case string(agent.EvSessionStarted):
		m.sessions.Inc()
	case string(agent.EvSessionEnded):
		var p agent.SessionEnded
		_ = json.Unmarshal(ev.Payload, &p)
		m.sessionsEnded.Inc(name(string(p.Reason)))
		m.forget(ev.SessionID)
	case string(agent.EvSandboxBound):
		var p struct{ Driver, Tier string }
		_ = json.Unmarshal(ev.Payload, &p)
		m.bound.Inc(name(p.Driver), name(p.Tier))
	case string(agent.EvPlanDecided):
		var p agent.PlanDecided
		_ = json.Unmarshal(ev.Payload, &p)
		via := "person"
		if p.Via == "operator" {
			via = "operator"
		}
		m.plans.Inc(oneOf(p.Decision, []string{"accept", "keep-planning"}), via)
	case string(agent.EvApprovalScopeGranted):
		m.scopes.Inc()
	case string(agent.EvAnswerVerdict):
		var p agent.AnswerVerdict
		if json.Unmarshal(ev.Payload, &p) == nil {
			m.claims.Add(float64(max(p.Verified, 0)), "verified")
			m.claims.Add(float64(max(p.Unverified, 0)), "unverified")
			m.claims.Add(float64(max(p.Contradicted, 0)), "contradicted")
		}
	case string(agent.EvSupervisorCorrection):
		var p agent.SupervisorCorrection
		_ = json.Unmarshal(ev.Payload, &p)
		m.corrections.Inc(oneOf(p.Code, VerdictCodes))
	case string(agent.EvSupervisorReplan):
		m.replans.Inc()
	case string(agent.EvRecordRepaired):
		m.repairs.Inc()
	case string(agent.EvConfigRefused):
		m.refused.Inc("refused")
	case string(agent.EvConfigNarrowed):
		m.refused.Inc("narrowed")
	case string(agent.EvFenceStatePlanted):
		m.planted.Inc("abhed_state")
	case sandbox.EvGitPlanted:
		m.planted.Inc("git")
	case string(agent.EvFenceDenied):
		var p struct {
			Source string         `json:"source"`
			Calls  map[string]int `json:"calls"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			source := oneOf(p.Source, []string{"seccomp"})
			for c, n := range p.Calls {
				// socket(inet) and the like are counted under the call.
				c, _, _ = strings.Cut(c, "(")
				m.fenceDenied.Add(float64(max(n, 0)), source, oneOf(c, seccomp.RefusableCalls()))
			}
		}
	case string(agent.EvOllamaStart), string(agent.EvOllamaInstall), string(agent.EvOllamaPull):
		var p agent.OllamaAction
		_ = json.Unmarshal(ev.Payload, &p)
		m.OllamaAction(p.Op, p.Outcome)
	case string(agent.EvFenceLimit):
		var p struct {
			OOM          int64 `json:"oom"`
			OOMKill      int64 `json:"oom_kill"`
			OOMGroupKill int64 `json:"oom_group_kill"`
			PidsMax      int64 `json:"pids_max"`
			SessionPids  int64 `json:"session_pids_max"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			if p.OOM+p.OOMKill+p.OOMGroupKill > 0 {
				m.fenceLimits.Inc("memory")
			}
			if p.PidsMax+p.SessionPids > 0 {
				m.fenceLimits.Inc("processes")
			}
		}
	}
}

func (m *Abhed) request(session string, p agent.ActionRequested) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) >= maxCalls {
		clear(m.calls)
	}
	m.calls[session+"\x00"+p.CallID] = &call{tool: name(p.Tool), asked: p.RequiresApproval}
}

func (m *Abhed) decided(session string, approved bool, p map[string]string) {
	k := session + "\x00" + p["call_id"]
	m.mu.Lock()
	c := m.calls[k]
	if c == nil {
		c = &call{tool: Other}
	}
	if approved {
		c.approved = true
	} else {
		delete(m.calls, k)
	}
	m.mu.Unlock()
	step, by := oneOf(p["step"], Steps), oneOf(p["by"], Approvers)
	tool := c.tool
	if step == "unknown" {
		tool = "unknown" // the name is the model's, not a tool's
	}
	decision := "deny"
	switch {
	case c.asked:
		decision = "ask"
	case approved:
		decision = "allow"
	}
	m.decisions.Inc(decision, step, tool)
	if !c.asked {
		return
	}
	result := "approved"
	switch {
	case approved:
	case by == agent.BySystem && step != Other:
		result = step // unanswered, interrupted, withheld, owner, precheck
	default:
		result = "rejected"
	}
	m.approvals.Inc(result, by, tool)
}

func (m *Abhed) observed(session string, p agent.Observation) {
	k := session + "\x00" + p.CallID
	m.mu.Lock()
	c := m.calls[k]
	delete(m.calls, k)
	m.mu.Unlock()
	if c == nil || !c.approved {
		return
	}
	outcome := "ok"
	switch {
	case p.NotRun:
		outcome = "not_run"
	case p.OutcomeUnknown:
		outcome = "unknown"
	case p.IsError:
		outcome = "error"
	}
	m.toolTime.Observe(float64(max(p.DurationMS, 0))/1000, c.tool, outcome)
}

func (m *Abhed) forget(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.calls {
		if len(k) > len(session) && k[:len(session)] == session && k[len(session)] == 0 {
			delete(m.calls, k)
		}
	}
}

func (m *Abhed) verdict(p agent.MonitorVerdict) {
	code := oneOf(p.Code, VerdictCodes)
	switch {
	case p.Skipped != "":
		code = "skipped"
	case p.Unavailable:
		code = "unavailable"
	}
	decisions := []string{"allow", "ask", "deny"}
	m.verdicts.Inc(code, oneOf(p.Before, decisions), oneOf(p.After, decisions))
	if p.Skipped == "" {
		m.verdictTime.Observe(float64(max(p.LatencyMS, 0)) / 1000)
	}
}

func (m *Abhed) modelCall(p agent.ModelCall) {
	model, purpose := name(p.Model), name(p.Purpose)
	if purpose == "" {
		purpose = "turn"
	}
	outcome := "ok"
	switch {
	case p.Error != "":
		outcome = "error"
	case p.CutOff:
		outcome = "cut_off"
	}
	m.modelCalls.Inc(model, purpose, outcome)
	m.modelTime.Observe(float64(max(p.LatencyMS, 0))/1000, model, purpose)
	if p.FirstTokenMS > 0 {
		m.firstToken.Observe(float64(p.FirstTokenMS)/1000, model, purpose)
	}
	m.tokens.Add(float64(max(p.TokensIn, 0)), model, "in")
	m.tokens.Add(float64(max(p.TokensOut, 0)), model, "out")
	m.tokens.Add(float64(max(p.TokensCached, 0)), model, "cached")
}

// oneOf is v when the list has it, Other otherwise.
func oneOf(v string, list []string) string {
	for _, s := range list {
		if v == s {
			return v
		}
	}
	return Other
}

// name keeps a configured name (a tool, a model, a driver) as a label when
// it looks like one, and is Other when it does not.
func name(v string) string {
	if len(v) > maxValue {
		return Other
	}
	for _, c := range v {
		ok := c == '_' || c == '-' || c == '.' || c == ':' || c == '/' || c == '@' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return Other
		}
	}
	return v
}
