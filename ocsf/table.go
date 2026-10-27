package ocsf

import (
	"encoding/json"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// rule is how one record type becomes an OCSF event: fn fills it in, or skip
// says why the type is left out.
type rule struct {
	fn   func(*builder)
	skip string
	// classes are the classes fn can choose, for the docs and tests.
	classes []int
}

// table is every type the record writes. A type added to the record without
// a row here fails TestEveryRecordTypeIsMapped.
var table = map[agent.EventType]rule{
	// Session and agent lifecycle: Application Lifecycle.
	agent.EvSessionStarted:  {fn: lifecycle(3, "Start"), classes: []int{6002}},
	agent.EvSessionResumed:  {fn: lifecycle(5, "Restart"), classes: []int{6002}},
	agent.EvSessionEnded:    {fn: mapSessionEnded, classes: []int{6002}},
	agent.EvSessionWoken:    {fn: lifecycle(99, "Wake"), classes: []int{6002}},
	agent.EvSubagentSpawned: {fn: lifecycle(3, "Start"), classes: []int{6002}},
	agent.EvSubagentReturn:  {fn: lifecycle(4, "Stop"), classes: []int{6002}},
	"sandbox.restarted":     {fn: lifecycle(5, "Restart"), classes: []int{6002}},

	// The conversation and the agent's calls: API Activity, with ai_operation.
	agent.EvUserMessage:          {fn: message(1), classes: []int{6003}},
	agent.EvAgentMessage:         {fn: message(2), classes: []int{6003}},
	agent.EvAgentReasoning:       {fn: message(2), classes: []int{6003}},
	agent.EvSubagentMessage:      {fn: message(1), classes: []int{6003}},
	agent.EvModelCall:            {fn: mapModelCall, classes: []int{6003}},
	agent.EvActionRequested:      {fn: mapRequested, classes: []int{6003, 1001, 4007}},
	agent.EvActionApproved:       {fn: mapDecided(true), classes: []int{6003}},
	agent.EvActionDenied:         {fn: mapDecided(false), classes: []int{6003}},
	agent.EvSubagentAction:       {fn: mapSubagentAction, classes: []int{6003}},
	agent.EvObservation:          {fn: mapObservation, classes: []int{6003}},
	agent.EvSubagentAsk:          {fn: apiOther("Ask"), classes: []int{6003}},
	agent.EvSubagentBackgrounded: {fn: apiOther("Background"), classes: []int{6003}},
	agent.EvSubagentNotice:       {fn: apiOther("Notice"), classes: []int{6003}},
	agent.EvHookFired:            {fn: mapHook, classes: []int{6003}},
	agent.EvMCPStatus:            {fn: mapMCPStatus, classes: []int{6003}},
	agent.EvOllamaPull:           {fn: mapOllama, classes: []int{6003}},
	agent.EvToolchainInstall:     {fn: mapToolchain, classes: []int{6003}},
	agent.EvToolchainRegistry:    {fn: apiOther("Open registries"), classes: []int{6003}},
	agent.EvTerminalInput:        {fn: apiOther("Terminal input"), classes: []int{6003}},
	agent.EvCommandInvoked:       {fn: apiOther("Command"), classes: []int{6003}},
	agent.EvMemoryLoaded:         {fn: apiActivity(2, "Read"), classes: []int{6003}},
	agent.EvModelSwitched:        {fn: apiActivity(3, "Update"), classes: []int{6003}},
	agent.EvModelFallback:        {fn: apiActivity(3, "Update"), classes: []int{6003}},
	agent.EvPlanProposed:         {fn: apiActivity(1, "Create"), classes: []int{6003}},
	agent.EvPlanDecided:          {fn: mapPlanDecided, classes: []int{6003}},
	agent.EvPlanStep:             {fn: apiOther("Plan step"), classes: []int{6003}},
	agent.EvChangeAccepted:       {fn: apiOther("Accept"), classes: []int{6003}},
	agent.EvWorktreeTaken:        {fn: apiOther("Take"), classes: []int{6003}},
	agent.EvDebugLaunched:        {fn: apiOther("Debug launch"), classes: []int{6003}},
	agent.EvDebugStopped:         {fn: apiOther("Debug stop"), classes: []int{6003}},
	agent.EvLSPStarted:           {fn: apiOther("Language server start"), classes: []int{6003}},
	agent.EvLSPStopped:           {fn: apiOther("Language server stop"), classes: []int{6003}},
	agent.EvCompletionEnabled:    {fn: apiOther("Completion enabled"), classes: []int{6003}},
	agent.EvCompletionSummary:    {fn: apiOther("Completion summary"), classes: []int{6003}},
	agent.EvNextEditEnabled:      {fn: apiOther("Next edit enabled"), classes: []int{6003}},
	agent.EvNextEditSummary:      {fn: apiOther("Next edit summary"), classes: []int{6003}},
	agent.EvCommitRequested:      {fn: apiOther("Commit message requested"), classes: []int{6003}},
	agent.EvCommitMessage:        {fn: apiOther("Commit message generated"), classes: []int{6003}},
	agent.EvTaskCancelled:        {fn: apiOther("Cancel"), classes: []int{6003}},
	agent.EvMessageDropped:       {fn: apiOther("Drop"), classes: []int{6003}},
	agent.EvSessionRenamed:       {fn: apiActivity(3, "Update"), classes: []int{6003}},
	agent.EvSessionNamed:         {fn: apiActivity(3, "Update"), classes: []int{6003}},
	agent.EvSessionBranched:      {fn: apiActivity(1, "Create"), classes: []int{6003}},
	agent.EvForked:               {fn: apiActivity(1, "Create"), classes: []int{6003}},
	agent.EvTurnContinued:        {fn: apiOther("Continue"), classes: []int{6003}},
	driver.EventSnapshot:         {fn: apiActivity(1, "Create"), classes: []int{6003}},
	driver.EventRestore:          {fn: apiActivity(3, "Update"), classes: []int{6003}},

	// Files the person or the harness wrote or read: File System Activity.
	agent.EvInputMention:  {fn: mapFile(2, "Read"), classes: []int{1001}},
	agent.EvMemoryWritten: {fn: mapFile(3, "Update"), classes: []int{1001}},
	agent.EvManualEdit:    {fn: mapFile(3, "Update"), classes: []int{1001}},
	agent.EvFileRestored:  {fn: mapFileRestored, classes: []int{1001}},
	agent.EvCheckpoint:    {fn: mapFile(99, "Checkpoint"), classes: []int{1001}},

	// Commands: Process Activity.
	agent.EvProcessLaunched: {fn: mapProcess(1, "Launch"), classes: []int{1007}},
	agent.EvOllamaStart:     {fn: mapOllama, classes: []int{1007}},
	agent.EvOllamaInstall:   {fn: mapOllama, classes: []int{1007}},
	agent.EvShellStarted:    {fn: mapProcess(1, "Launch"), classes: []int{1007}},
	agent.EvShellEnded:      {fn: mapProcess(2, "Terminate"), classes: []int{1007}},

	// The network: Network Activity and HTTP Activity.
	egress.EventName:          {fn: mapEgress, classes: []int{4001, 4002}},
	driver.EventForward:       {fn: mapForward, classes: []int{4001}},
	driver.EventForwardListen: {fn: mapForward, classes: []int{4001}},
	driver.EventExpose:        {fn: mapForward, classes: []int{4001}},

	// Policy, mode, configuration and the sandbox's setup: Device Config State Change.
	agent.EvModeChanged:          {fn: configChange, classes: []int{5019}},
	agent.EvPermissionChanged:    {fn: configChange, classes: []int{5019}},
	agent.EvPolicyLayer:          {fn: configChange, classes: []int{5019}},
	agent.EvStartupTrusted:       {fn: configChange, classes: []int{5019}},
	agent.EvConfigRefused:        {fn: mapConfigAttempt, classes: []int{5019}},
	agent.EvConfigNarrowed:       {fn: mapConfigAttempt, classes: []int{5019}},
	agent.EvApprovalScopeGranted: {fn: configChange, classes: []int{5019}},
	agent.EvWorkspaceDirAdded:    {fn: configChange, classes: []int{5019}},
	agent.EvSandboxBound:         {fn: configChange, classes: []int{5019}},
	agent.EvFenceQualified:       {fn: configChange, classes: []int{5019}},
	agent.EvWakeSet:              {fn: configChange, classes: []int{5019}},

	// Judgements and integrity: Detection Finding.
	agent.EvMonitorVerdict:       {fn: mapMonitor, classes: []int{2004}},
	agent.EvAnswerVerdict:        {fn: mapAnswer, classes: []int{2004}},
	agent.EvSupervisorCorrection: {fn: finding("Supervisor correction", 2), classes: []int{2004}},
	agent.EvSupervisorReplan:     {fn: finding("Supervisor replan", 2), classes: []int{2004}},
	agent.EvFenceLimit:           {fn: finding("Fence limit", 2), classes: []int{2004}},
	sandbox.EvFenceStatePlanted:  {fn: finding("State planted in the workspace", 4), classes: []int{2004}},
	sandbox.EvFenceDenied:        {fn: mapFenceDenied, classes: []int{2004}},
	sandbox.EvGitPlanted:         {fn: finding("Git folder planted", 4), classes: []int{2004}},
	sandbox.EvGitLinked:          {fn: finding("Git folder linked", 3), classes: []int{2004}},
	sandbox.EvGitWalkBounded:     {fn: finding("Git walk bounded", 2), classes: []int{2004}},
	agent.EvRecordRepaired:       {fn: finding("Record repaired", 2), classes: []int{2004}},

	// The harness's own bookkeeping: Base Event.
	agent.EvCompactStarted:    {fn: mapBase, classes: []int{0}},
	agent.EvCompactDone:       {fn: mapBase, classes: []int{0}},
	agent.EvContextOffloaded:  {fn: mapBase, classes: []int{0}},
	agent.EvPlanUpdated:       {fn: mapBase, classes: []int{0}},
	agent.EvTodoUpdated:       {fn: mapBase, classes: []int{0}},
	agent.EvSuggestionOffered: {fn: mapBase, classes: []int{0}},

	// Fragments of a reply: the whole text is its agent.message or agent.reasoning.
	agent.EvAgentDelta:          {skip: "a fragment of a reply; the whole text is the agent.message that follows"},
	agent.EvAgentReasoningDelta: {skip: "a fragment of the model's reasoning; the whole text is the agent.reasoning that follows"},
}

// Mapping says how a record type is written: the OCSF classes it can become,
// or why it is not mapped. A type the record does not know is a Base Event.
func Mapping(t agent.EventType) (classes []int, notMapped string, known bool) {
	r, ok := table[t]
	if !ok {
		return []int{0}, "", false
	}
	return r.classes, r.skip, true
}

// Types lists every record type the table holds, sorted.
func Types() []agent.EventType {
	out := make([]agent.EventType, 0, len(table))
	for t := range table {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func mapBase(b *builder) { b.classify(0, 99, "Other") }

func lifecycle(activity int, name string) func(*builder) {
	return func(b *builder) {
		b.classify(6002, activity, name)
		app := map[string]any{"name": "Abhed", "vendor_name": "Zybuu"}
		if v := b.m.opts.ProductVersion; v != "" {
			app["version"] = v
		}
		switch b.l.Type {
		case agent.EvSubagentSpawned, agent.EvSubagentReturn:
			app["name"] = "Abhed subagent"
			if s := b.str("session"); s != "" {
				app["uid"] = s
			}
			b.set("message", strings.TrimSpace(string(b.l.Type)+" "+b.str("description")))
		case "sandbox.restarted":
			app["name"] = "Abhed sandbox"
			if s := b.str("sandbox_id"); s != "" {
				app["uid"] = s
			}
		}
		b.set("app", app)
	}
}

func mapSessionEnded(b *builder) {
	lifecycle(4, "Stop")(b)
	reason := b.str("reason")
	switch agent.TerminalReason(reason) {
	case agent.TermCompleted, agent.TermWakeLimit:
		b.status(1, "", b.str("detail"))
	case agent.TermError, agent.TermRetryExhausted, agent.TermStalled:
		b.status(2, "", b.str("detail"))
	default:
		b.status(99, reason, b.str("detail"))
	}
	b.set("message", "session ended: "+reason)
}

func apiActivity(activity int, name string) func(*builder) {
	return func(b *builder) {
		b.classify(6003, activity, name)
		b.actor(b.str("by"))
		b.api("", b.str("call_id"))
	}
}

func apiOther(name string) func(*builder) { return apiActivity(99, name) }

func message(role int) func(*builder) {
	return func(b *builder) {
		b.classify(6003, 99, "Message")
		b.actor("")
		b.api("", "")
		b.ai("", role, 0, 0)
	}
}

func mapModelCall(b *builder) {
	b.classify(6003, 99, "Model call")
	b.actor("")
	b.api("model", "")
	in, _ := b.num("tokens_in")
	out, _ := b.num("tokens_out")
	b.ai(b.str("model"), 2, in, out)
	if ms, ok := b.num("latency_ms"); ok {
		b.set("duration", ms)
	}
	if e := b.str("error"); e != "" {
		b.status(2, "", e)
	} else {
		b.status(1, "", "")
	}
}

// fileTools are the calls that read or change one file by its path.
var fileTools = map[string]struct {
	activity int
	name     string
}{
	"read": {2, "Read"}, "write": {3, "Update"}, "edit": {3, "Update"}, "memory_write": {3, "Update"},
}

// sshTools are the calls that reach a configured host.
var sshTools = map[string]struct {
	activity int
	name     string
}{
	"ssh": {1, "Open"}, "ssh_connect": {1, "Open"}, "ssh_put": {6, "Traffic"}, "ssh_get": {6, "Traffic"},
}

func mapRequested(b *builder) {
	tool := b.str("tool")
	args, _ := b.p["args"].(map[string]any)
	path, _ := args["path"].(string)
	if ft, ok := fileTools[tool]; ok && path != "" {
		b.classify(1001, ft.activity, ft.name)
		b.actor("")
		b.device()
		b.set("file", fileObject(path, ""))
	} else if st, ok := sshTools[tool]; ok {
		b.classify(4007, st.activity, st.name)
		b.actor("")
		dst := map[string]any{}
		if h, _ := args["host"].(string); h != "" {
			dst["name"] = h
		}
		b.set("dst_endpoint", dst)
		b.set("src_endpoint", b.source())
	} else {
		b.classify(6003, 99, "Call")
		b.actor("")
		b.api(tool, b.str("call_id"))
		b.ai("", 4, 0, 0)
	}
	b.status(99, "Requested", b.str("reason"))
	b.set("message", "requested "+tool)
}

func mapDecided(allowed bool) func(*builder) {
	return func(b *builder) {
		b.classify(6003, 99, "Decision")
		who := b.str("approver")
		if who == "" {
			who = b.str("by")
		}
		b.actor(who)
		b.api("", b.str("call_id"))
		disp := 2
		if allowed {
			disp = 1
			if b.str("by") == "reviewer" {
				disp = 8
			}
		}
		b.decision(allowed, disp)
		b.policy()
		if allowed {
			b.status(1, "", b.str("reason"))
			b.set("message", "allowed at "+b.str("step"))
		} else {
			b.status(2, "", b.str("reason"))
			b.severity(3)
			b.set("message", "denied at "+b.str("step"))
		}
	}
}

// policy names the rule, layer or scope that decided, when the line has one.
func (b *builder) policy() {
	name := b.str("rule")
	if name == "" {
		name = b.str("scope")
	}
	if name == "" {
		return
	}
	p := map[string]any{"name": name, "is_applied": true}
	if layer := b.str("layer"); layer != "" {
		p["desc"] = "policy layer " + layer
	}
	b.set("policy", p)
	b.profile("security_control")
}

func mapSubagentAction(b *builder) {
	allowed := b.str("decision") == "allowed"
	b.classify(6003, 99, "Decision")
	who := b.str("approver")
	if who == "" {
		who = b.str("by")
	}
	b.actor(who)
	b.api(b.str("tool"), b.str("call_id"))
	disp := 2
	if allowed {
		disp = 1
	}
	b.decision(allowed, disp)
	b.policy()
	if !allowed {
		b.severity(3)
	}
	b.set("message", "subagent call "+b.str("decision"))
}

func mapObservation(b *builder) {
	b.classify(6003, 99, "Result")
	b.actor("")
	b.api(b.str("tool"), b.str("call_id"))
	b.ai("", 3, 0, 0)
	if ms, ok := b.num("duration_ms"); ok {
		b.set("duration", ms)
	}
	switch {
	case b.flag("not_run"):
		b.status(99, "Not run", "")
	case b.flag("outcome_unknown"):
		b.status(0, "", "")
	case b.flag("is_error"):
		b.status(2, "", "")
	default:
		b.status(1, "", "")
	}
	b.set("message", "result of "+b.str("tool"))
}

func mapHook(b *builder) {
	b.classify(6003, 99, "Hook")
	b.actor(b.str("extension"))
	b.api(b.str("event"), "")
	switch b.str("verdict") {
	case "block":
		b.decision(false, 2)
		b.severity(3)
	case "annotate", "ask":
		b.profile("security_control")
		b.set("action_id", 3)
		b.set("action", "Observed")
	}
	b.set("message", "hook "+b.str("extension")+": "+b.str("verdict"))
}

func mapMCPStatus(b *builder) {
	b.classify(6003, 99, "Restart")
	b.actor(b.str("by"))
	b.api("mcp."+b.str("op"), "")
	b.set("resources", []any{map[string]any{"name": b.str("server"), "type": "mcp_server"}})
	if b.str("status") == "error" {
		b.status(2, "", b.str("error"))
	} else {
		b.status(1, "", "")
	}
	b.set("message", "mcp "+b.str("server")+" "+b.str("op")+": "+b.str("status"))
}

// mapOllama is a person's start, install or pull of a local Ollama: the
// server's start and an install as Process Activity (Launch), a pull as
// API Activity (Create) of the model; declined, refused and failed
// attempts as a failure with the reason.
func mapOllama(b *builder) {
	op := b.str("op")
	if op == "pull" {
		b.classify(6003, 1, "Create")
		b.api("ollama.pull", "")
		b.set("resources", []any{map[string]any{"name": b.str("model"), "type": "model"}})
	} else {
		b.classify(1007, 1, "Launch")
		proc := map[string]any{}
		if pid, ok := b.num("pid"); ok {
			proc["pid"] = pid
		}
		if c := b.str("command"); c != "" {
			proc["cmd_line"] = c
		}
		b.set("process", proc)
		b.device()
	}
	b.actor(b.str("by"))
	switch outcome := b.str("outcome"); outcome {
	case "ok":
		b.status(1, "", "")
	case "manual":
		b.status(99, "Manual", "left to the person: "+b.str("command"))
	default:
		b.status(2, outcome, firstOf(b.str("error"), b.str("problem")))
	}
	msg := "ollama " + op
	if m := b.str("model"); m != "" {
		msg += " " + m
	}
	b.set("message", msg+": "+b.str("outcome"))
}

// mapToolchain is a language toolchain installed for a session's commands:
// API Activity (Create) of the toolchain, with the download and its
// published checksum; declined, refused and failed as a failure.
func mapToolchain(b *builder) {
	b.classify(6003, 1, "Create")
	b.api("toolchain.install", "")
	b.set("resources", []any{map[string]any{"name": b.str("lang") + " " + b.str("version"), "type": "toolchain",
		"uid": b.str("digest"), "data": map[string]any{"url": b.str("url"), "platform": b.str("platform"), "source": b.str("source")}}})
	b.actor(b.str("by"))
	if outcome := b.str("outcome"); outcome == "ok" {
		b.status(1, "", "")
	} else {
		b.status(2, outcome, b.str("error"))
	}
	b.set("message", "toolchain install "+b.str("lang")+" "+b.str("version")+": "+b.str("outcome"))
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func mapPlanDecided(b *builder) {
	b.classify(6003, 99, "Decision")
	b.actor(b.str("by"))
	b.api("", "")
	b.set("message", "plan "+b.str("decision"))
}

func fileObject(path, sha string) map[string]any {
	name := path
	if i := strings.LastIndexAny(path, `/\`); i >= 0 && i < len(path)-1 {
		name = path[i+1:]
	}
	f := map[string]any{"name": name, "path": path, "type_id": 1, "type": "Regular File"}
	if sha != "" {
		f["hashes"] = []any{map[string]any{"algorithm_id": 3, "algorithm": "SHA-256", "value": sha}}
	}
	return f
}

func mapFile(activity int, name string) func(*builder) {
	return func(b *builder) {
		b.classify(1001, activity, name)
		b.actor(b.str("by"))
		b.device()
		sha := b.str("sha256")
		if sha == "" {
			sha = b.str("after_sha256")
		}
		b.set("file", fileObject(b.str("path"), sha))
		b.set("message", string(b.l.Type)+" "+b.str("path"))
	}
}

func mapFileRestored(b *builder) {
	switch {
	case b.str("before_sha256") == "":
		mapFile(1, "Create")(b)
	case b.str("after_sha256") == "":
		mapFile(4, "Delete")(b)
	default:
		mapFile(3, "Update")(b)
	}
}

func mapProcess(activity int, name string) func(*builder) {
	return func(b *builder) {
		b.classify(1007, activity, name)
		b.actor("")
		b.device()
		proc := map[string]any{}
		if pid, ok := b.num("pid"); ok {
			proc["pid"] = pid
		}
		if c := b.str("command"); c != "" {
			proc["cmd_line"] = c
		}
		if s := b.str("shell_id"); s != "" {
			proc["uid"] = s
		}
		if s := b.str("sandbox"); s != "" {
			proc["sandbox"] = s
		}
		b.set("process", proc)
		if code, ok := b.num("exit_code"); ok {
			b.set("exit_code", code)
		}
		b.set("message", string(b.l.Type)+" for "+b.str("call_id"))
	}
}

// httpActivities are HTTP Activity's activity ids by method.
var httpActivities = map[string]int{"CONNECT": 1, "DELETE": 2, "GET": 3, "HEAD": 4, "OPTIONS": 5, "POST": 6, "PUT": 7, "TRACE": 8, "PATCH": 9}

func mapEgress(b *builder) {
	kind, method := b.str("kind"), strings.ToUpper(b.str("method"))
	decision := b.str("decision")
	allowed := decision != "deny"
	dst := map[string]any{}
	if h := b.str("host"); h != "" {
		dst["hostname"] = h
	}
	if ip := b.str("ip"); ip != "" {
		dst["ip"] = ip
	}
	port, hasPort := b.num("port")
	if hasPort && port > 0 {
		dst["port"] = port
	}
	if (kind == "http" || kind == "https") && method != "" {
		act, ok := httpActivities[method]
		name := strings.ToUpper(method[:1]) + strings.ToLower(method[1:])
		if !ok {
			act, name = 99, "Other"
		}
		b.classify(4002, act, name)
		u := map[string]any{"hostname": b.str("host")}
		if p := b.str("path"); p != "" {
			u["path"] = p
		}
		if hasPort && port > 0 {
			u["port"] = port
		}
		req := map[string]any{"url": u}
		if _, ok := httpActivities[method]; ok {
			req["http_method"] = method
		}
		b.set("http_request", req)
	} else {
		switch {
		case kind == "summary":
			b.classify(4001, 6, "Traffic")
		case !allowed:
			b.classify(4001, 5, "Refuse")
		default:
			b.classify(4001, 1, "Open")
		}
	}
	b.set("dst_endpoint", dst)
	b.set("src_endpoint", b.source())
	b.set("connection_info", map[string]any{"direction_id": 2, "direction": "Outbound"})
	in, _ := b.num("bytes_in")
	out, _ := b.num("bytes_out")
	if in > 0 || out > 0 {
		b.set("traffic", map[string]any{"bytes_in": in, "bytes_out": out, "bytes": in + out})
	}
	if n, ok := b.num("repeats"); ok && n > 0 {
		b.set("count", n)
	}
	switch decision {
	case "deny":
		b.decision(false, 2)
		b.severity(3)
	case "would_deny":
		// Audit mode: let through, and noted as a denial would have been.
		b.decision(true, 15)
		b.severity(2)
	default:
		b.decision(true, 1)
	}
	if r := b.str("rule"); r != "" {
		b.set("firewall_rule", map[string]any{"name": r})
	}
	if reason := b.str("reason"); reason != "" {
		b.set("status_detail", reason)
	}
	b.set("message", strings.TrimSpace(kind+" "+b.str("host")+" "+decision))
}

func mapForward(b *builder) {
	switch b.str("action") {
	case "close":
		b.classify(4001, 2, "Close")
	case "listen", "open":
		b.classify(4001, 7, "Listen")
	default:
		b.classify(4001, 99, "Other")
	}
	// The sandbox's port is where forwarded connections go; the host's
	// listening address, when the line has one, is where they come in.
	dst := map[string]any{}
	if port, ok := b.num("port"); ok && port > 0 {
		dst["port"] = port
	}
	if s := b.str("sandbox_id"); s != "" {
		dst["uid"] = s
	}
	b.set("dst_endpoint", dst)
	b.set("src_endpoint", b.source())
	if addr := b.str("address"); addr != "" {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			src := map[string]any{"ip": host}
			if n, err := strconv.Atoi(port); err == nil {
				src["port"] = n
			}
			b.set("src_endpoint", src)
		}
	}
	b.set("message", string(b.l.Type)+" "+b.str("action"))
}

func configChange(b *builder) {
	b.classify(5019, 1, "Log")
	b.device()
	b.set("message", strings.TrimSpace(string(b.l.Type)+" "+b.str("to")))
}

func mapConfigAttempt(b *builder) {
	configChange(b)
	if b.l.Type == agent.EvConfigRefused {
		b.status(2, "", b.str("reason"))
		b.severity(2)
	} else {
		b.status(1, "", b.str("reason"))
	}
	b.set("message", string(b.l.Type)+" "+b.str("key")+": "+b.str("decision"))
}

// finding is a Detection Finding at severity sev, titled title.
func finding(title string, sev int) func(*builder) {
	return func(b *builder) {
		b.classify(2004, 1, "Create")
		b.severity(sev)
		info := map[string]any{"uid": b.l.ID, "title": title,
			"analytic": map[string]any{"name": string(b.l.Type), "type_id": 1, "type": "Rule"}}
		if r := b.str("reason"); r != "" {
			info["desc"] = r
		}
		b.set("finding_info", info)
		b.set("message", title)
	}
}

// mapFenceDenied is the system calls the fence's seccomp filter refused a
// command: a security control's detection, Denied and Blocked, its count the
// refusals in all.
func mapFenceDenied(b *builder) {
	finding("System calls refused by the fence", 3)(b)
	b.decision(false, 2)
	calls, _ := b.p["calls"].(map[string]any)
	names := make([]string, 0, len(calls))
	var total int64
	for name, n := range calls {
		names = append(names, name)
		if v, ok := n.(json.Number); ok {
			if i, err := v.Int64(); err == nil {
				total += i
			}
		}
	}
	sort.Strings(names)
	if total > 0 {
		b.set("count", total)
	}
	what := strings.Join(names, ", ")
	if what == "" {
		what = "system calls"
	}
	info := b.ev["finding_info"].(map[string]any)
	info["desc"] = strings.TrimSpace(b.str("source") + " refused " + what)
	msg := "fence refused " + what
	if c := b.str("call_id"); c != "" {
		msg += " for " + c
	}
	b.set("message", msg)
}

func mapMonitor(b *builder) {
	finding("Supervisor verdict", 1)(b)
	before, after := b.str("before"), b.str("after")
	if after != "" && after != before {
		b.severity(3)
	}
	info := b.ev["finding_info"].(map[string]any)
	if r := b.str("rationale"); r != "" {
		info["desc"] = r
	}
	b.set("message", "supervisor "+b.str("code")+": "+before+" -> "+after)
}

func mapAnswer(b *builder) {
	finding("Answer verdict", 1)(b)
	if n, _ := b.num("contradicted"); n > 0 {
		b.severity(3)
	} else if b.str("outcome") == "unverified" {
		b.severity(2)
	}
	b.set("message", "answer "+b.str("outcome"))
}
