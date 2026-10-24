package ocsf

import (
	"encoding/json"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// sample is one record line of a type, as the record writes it.
type sample struct {
	typ     agent.EventType
	actor   agent.Actor
	payload string
}

// samples holds a line of every type the table maps, with the payload fields
// docs/architecture/10-data-model.md names for it. Some types have more than
// one, where the payload decides the class or the activity.
var samples = []sample{
	{agent.EvSessionStarted, agent.ActorSystem, `{"workspace":"/w","model":"m-1","mode":"default","provider":"openai","surface":"cli"}`},
	{agent.EvSessionResumed, agent.ActorSystem, `{"surface":"acp","via":"load","through_seq":4,"provider":"openai","model":"m-1"}`},
	{agent.EvSessionEnded, agent.ActorSystem, `{"reason":"completed","turns":2,"tokens_in":10,"tokens_out":5,"tokens_cached":0,"compactions":0}`},
	{agent.EvSessionEnded, agent.ActorSystem, `{"reason":"user_interrupt","turns":1,"tokens_in":1,"tokens_out":1,"tokens_cached":0,"compactions":0,"detail":"interrupted, background shells stopped"}`},
	{agent.EvSessionWoken, agent.ActorSystem, `{"by":"policy","wake_mode":"auto","task_ids":["t1"],"wakes_last_hour":1}`},
	{agent.EvSubagentSpawned, agent.ActorSystem, `{"description":"look around","session":"s-child","call_id":"c-9","model":"m-1"}`},
	{agent.EvSubagentReturn, agent.ActorSystem, `{"description":"look around","session":"s-child","reason":"completed","end_seq":12}`},
	{agent.EvMCPStatus, agent.ActorUser, `{"server":"files","op":"restart","status":"connected","by":"user"}`},
	{agent.EvMCPStatus, agent.ActorUser, `{"server":"files","op":"restart","status":"error","error":"exit 1","by":"user"}`},
	{"sandbox.restarted", agent.ActorSystem, `{"sandbox_id":"sb-1","generation":2}`},
	{agent.EvUserMessage, agent.ActorUser, `{"text":"fix the build"}`},
	{agent.EvAgentMessage, agent.ActorAgent, `{"text":"done"}`},
	{agent.EvAgentReasoning, agent.ActorAgent, `{"text":"thinking","turn":1}`},
	{agent.EvSubagentMessage, agent.ActorUser, `{"session":"s-child","text":"also check docs","by":"user"}`},
	{agent.EvModelCall, agent.ActorSystem, `{"turn":1,"model":"m-1","tokens_in":120,"tokens_out":30,"tokens_cached":0,"first_token_ms":80,"latency_ms":900,"tool_calls":1}`},
	{agent.EvModelCall, agent.ActorSystem, `{"turn":2,"model":"m-1","tokens_in":0,"tokens_out":0,"tokens_cached":0,"first_token_ms":0,"latency_ms":30000,"tool_calls":0,"error":"timeout","retryable":true}`},
	{agent.EvActionRequested, agent.ActorAgent, `{"call_id":"c-1","tool":"bash","args":{"command":"go test ./..."},"requires_approval":true}`},
	{agent.EvActionRequested, agent.ActorAgent, `{"call_id":"c-2","tool":"read","args":{"path":"/w/main.go"},"requires_approval":false}`},
	{agent.EvActionRequested, agent.ActorAgent, `{"call_id":"c-3","tool":"edit","args":{"path":"/w/main.go","old":"a","new":"b"},"requires_approval":true}`},
	{agent.EvActionRequested, agent.ActorAgent, `{"call_id":"c-4","tool":"ssh","args":{"host":"db-1","command":"uptime"},"requires_approval":true}`},
	{agent.EvActionApproved, agent.ActorSystem, `{"call_id":"c-1","step":"allow","reason":"rule","by":"policy","rule":"bash(go test *)"}`},
	{agent.EvActionApproved, agent.ActorUser, `{"call_id":"c-3","step":"ask","reason":"approved","by":"reviewer","approver":"ana@example.com","granted_scope":"edit(/w/**)"}`},
	{agent.EvActionDenied, agent.ActorSystem, `{"call_id":"c-4","step":"deny","reason":"denied by rule","by":"policy","rule":"ssh(*)","layer":"team"}`},
	{agent.EvSubagentAction, agent.ActorUser, `{"session":"s-child","call_id":"c-7","tool":"bash","decision":"denied","step":"ask","reason":"no","by":"reviewer"}`},
	{agent.EvObservation, agent.ActorTool, `{"call_id":"c-1","tool":"bash","content":"ok","is_error":false,"truncated":false,"exit_code":0,"duration_ms":1200,"sandbox":"fence"}`},
	{agent.EvObservation, agent.ActorTool, `{"call_id":"c-5","tool":"bash","content":"fail","is_error":true,"truncated":false,"exit_code":2,"duration_ms":40}`},
	{agent.EvObservation, agent.ActorSystem, `{"call_id":"c-6","tool":"bash","content":"Not run","is_error":true,"truncated":false,"duration_ms":0,"not_run":true}`},
	{agent.EvSubagentAsk, agent.ActorAgent, `{"session":"s-child","request_id":"r-1","call_id":"c-7","tool":"bash","args":{"command":"rm x"},"subject":"rm x"}`},
	{agent.EvSubagentBackgrounded, agent.ActorUser, `{"session":"s-child","task_id":"s-child","description":"look","background":true,"by":"user"}`},
	{agent.EvSubagentNotice, agent.ActorSystem, `{"task_id":"s-child","session":"s-child","status":"completed","content":"summary"}`},
	{agent.EvHookFired, agent.ActorSystem, `{"extension":"guard","event":"pre_tool","verdict":"block"}`},
	{agent.EvHookFired, agent.ActorSystem, `{"extension":"notes","event":"pre_tool","verdict":"annotate"}`},
	{agent.EvTerminalInput, agent.ActorUser, `{"call_id":"c-8","line":"ls"}`},
	{agent.EvCommandInvoked, agent.ActorUser, `{"name":"review","source":"workspace","sha256":"ab","args":""}`},
	{agent.EvMemoryLoaded, agent.ActorSystem, `{"files":[{"path":"ABHED.md","scope":"project","sha256":"cd"}]}`},
	{agent.EvModelSwitched, agent.ActorUser, `{"provider":"vllm","model":"m-2","from":"m-1"}`},
	{agent.EvModelFallback, agent.ActorSystem, `{"from":"m-2","to":"m-3","reason":"unreachable"}`},
	{agent.EvPlanProposed, agent.ActorAgent, `{"text":"1. test","hash":"ef"}`},
	{agent.EvPlanDecided, agent.ActorUser, `{"decision":"accept","to_mode":"default","plan_hash":"ef"}`},
	{agent.EvPlanStep, agent.ActorSystem, `{"call_id":"c-1","plan_hash":"ef","step":1,"of":2,"tool":"bash","by":"plan"}`},
	{agent.EvChangeAccepted, agent.ActorUser, `{"path":"/w/main.go","by":"user"}`},
	{agent.EvWorktreeTaken, agent.ActorUser, `{"worktree":".abhed-worktrees/1","branch":"b","taken":["a.go"]}`},
	{agent.EvTaskCancelled, agent.ActorUser, `{"task_id":"s-child","by":"user"}`},
	{agent.EvMessageDropped, agent.ActorSystem, `{"queue_id":"q1","text":"later","queued_at":"2026-10-19T10:00:00Z","reason":"shutdown"}`},
	{agent.EvSessionRenamed, agent.ActorUser, `{"title":"build fix","by":"user"}`},
	{agent.EvSessionNamed, agent.ActorUser, `{"name":"fix"}`},
	{agent.EvSessionBranched, agent.ActorUser, `{"from":"s-0","through_seq":9}`},
	{agent.EvForked, agent.ActorUser, `{"through_seq":3}`},
	{agent.EvTurnContinued, agent.ActorSystem, `{"through_seq":7}`},
	{"sandbox.snapshot", agent.ActorSystem, `{"sandbox_id":"sb-1","generation":1,"snapshot":"snap-1"}`},
	{"sandbox.restore", agent.ActorSystem, `{"sandbox_id":"sb-1","generation":1,"snapshot":"snap-1"}`},
	{agent.EvInputMention, agent.ActorUser, `{"path":"/w/README.md","sha256":"0f343b0931126a20f133d67c2b018a3b5f4b5d1b9e1b8a2f0d1c3b4a5e6f7081","bytes":12,"truncated":false}`},
	{agent.EvMemoryWritten, agent.ActorAgent, `{"path":"/home/u/.abhed/memory/note.md","kind":"note","by":"agent"}`},
	{agent.EvManualEdit, agent.ActorUser, `{"path":"/w/main.go","before_sha256":"aa","after_sha256":"bb","by":"user"}`},
	{agent.EvFileRestored, agent.ActorUser, `{"path":"/w/a.go","before_sha256":"aa","after_sha256":"bb","checkpoint":4,"by":"user"}`},
	{agent.EvFileRestored, agent.ActorUser, `{"path":"/w/new.go","after_sha256":"bb","checkpoint":4,"by":"user"}`},
	{agent.EvFileRestored, agent.ActorUser, `{"path":"/w/gone.go","before_sha256":"aa","checkpoint":4,"by":"user"}`},
	{agent.EvCheckpoint, agent.ActorSystem, `{"path":"/w/main.go","sha256":"aa","turn":1,"mode":420}`},
	{agent.EvProcessLaunched, agent.ActorSystem, `{"call_id":"c-1","source":"call","pid":4242,"tier":"fence","sandbox":"f-1","landlock_abi":6,"seccomp":true,"network":false}`},
	{agent.EvShellStarted, agent.ActorSystem, `{"shell_id":"sh-1","call_id":"c-1","command":"npm run dev","sandbox":"none"}`},
	{agent.EvShellEnded, agent.ActorSystem, `{"shell_id":"sh-1","call_id":"c-1","state":"exited","exit_code":0,"duration_ms":5000}`},
	{"egress.decision", agent.ActorSystem, `{"call_id":"c-1","kind":"connect","host":"proxy.golang.org","port":443,"ip":"142.250.0.1","decision":"allow","rule":"*.golang.org","reason":"","bytes_in":2048,"bytes_out":512}`},
	{"egress.decision", agent.ActorSystem, `{"call_id":"c-1","kind":"connect","host":"evil.example","port":443,"decision":"deny","rule":"","reason":"no rule allows evil.example","bytes_in":0,"bytes_out":0}`},
	{"egress.decision", agent.ActorSystem, `{"call_id":"c-1","kind":"http","host":"example.org","port":80,"method":"GET","path":"/index.html","decision":"would_deny","rule":"","reason":"audit","bytes_in":100,"bytes_out":50}`},
	{"egress.decision", agent.ActorSystem, `{"call_id":"c-1","kind":"summary","host":"example.org","port":80,"decision":"deny","rule":"","reason":"rate","bytes_in":0,"bytes_out":0,"repeats":40}`},
	{"sandbox.forward", agent.ActorSystem, `{"sandbox_id":"sb-1","generation":1,"port":8000,"action":"open"}`},
	{"sandbox.forward_listen", agent.ActorSystem, `{"sandbox_id":"sb-1","generation":1,"port":8000,"action":"listen","address":"127.0.0.1:40000","wide":false}`},
	{"sandbox.expose", agent.ActorSystem, `{"sandbox_id":"sb-1","generation":1,"port":8000,"exposure_id":"x1","action":"close","owner":"ana"}`},
	{agent.EvModeChanged, agent.ActorUser, `{"from":"default","to":"plan","by":"user","via":"slash"}`},
	{agent.EvPermissionChanged, agent.ActorUser, `{"op":"add","list":"allow","rule":"bash(make *)","scope":"session","by":"user"}`},
	{agent.EvPolicyLayer, agent.ActorSystem, `{"layer":"team","revision":3,"sha256":"aa","deny":["ssh(*)"]}`},
	{agent.EvStartupTrusted, agent.ActorSystem, `{"reason":"trusted workspace"}`},
	{agent.EvConfigRefused, agent.ActorSystem, `{"layer":"workspace","source":".abhed/config.json","key":"record.dir","decision":"set_aside","reason":"only the managed configuration moves the record","principal":{"kind":"os-user","os_user":"u","uid":"501"}}`},
	{agent.EvConfigNarrowed, agent.ActorSystem, `{"layer":"user","key":"web_search.enabled","value":"false","decision":"narrowed","principal":{"kind":"os-user"}}`},
	{agent.EvApprovalScopeGranted, agent.ActorUser, `{"scope":"edit(/w/**)","by":"user"}`},
	{agent.EvWorkspaceDirAdded, agent.ActorUser, `{"path":"../lib","canonical":"/lib","access":"read","by":"user"}`},
	{agent.EvSandboxBound, agent.ActorSystem, `{"sandbox_id":"sb-1","generation":1,"driver":"podman","tier":"container","scope":"session"}`},
	{agent.EvFenceQualified, agent.ActorSystem, `{"mode":"full","landlock_abi":6}`},
	{agent.EvWakeSet, agent.ActorUser, `{"wake":"notify","by":"user"}`},
	{agent.EvMonitorVerdict, agent.ActorSystem, `{"call_id":"c-1","before":"allow","after":"ask","code":"exfil","confidence":0.8,"rationale":"sends the key out","latency_ms":30,"version":"r1"}`},
	{agent.EvMonitorVerdict, agent.ActorSystem, `{"call_id":"c-2","before":"allow","after":"allow","code":"ok","confidence":0.9,"latency_ms":20,"version":"r1"}`},
	{agent.EvAnswerVerdict, agent.ActorSystem, `{"mode":"check","source":"text","verified":1,"unverified":0,"contradicted":1,"outcome":"unverified","turn":2}`},
	{agent.EvSupervisorCorrection, agent.ActorSystem, `{"call_id":"c-4","code":"scope","decision":"deny","text":"Supervisor: scope."}`},
	{agent.EvSupervisorReplan, agent.ActorSystem, `{"failures":3,"text":"restate"}`},
	{agent.EvFenceLimit, agent.ActorSystem, `{"call_id":"c-1","sandbox":"f-1","oom":1,"oom_kill":1,"oom_group_kill":0,"pids_max":0,"session_pids_max":0,"memory_max_mb":4096,"max_procs":512}`},
	{"fence.denied", agent.ActorSystem, `{"call_id":"c-1","sandbox":"f-1","source":"seccomp","calls":{"ptrace":2,"socket(unix)":1}}`},
	{"fence.state_planted", agent.ActorSystem, `{"when":"after_command","entries":[{"path":".abhed","outcome":"moved"}],"call_id":"c-1"}`},
	{"sandbox.git_planted", agent.ActorSystem, `{"call_id":"c-1","sandbox":"bwrap","workspace":"/w","entries":[{"path":".git/commondir","outcome":"moved"}],"reason":"commondir points out of the workspace"}`},
	{"sandbox.git_linked", agent.ActorSystem, `{"call_id":"c-1","sandbox":"bwrap","workspace":"/w","paths":[".git/hooks"],"refused":true}`},
	{"sandbox.git_walk_bounded", agent.ActorSystem, `{"call_id":"c-1","sandbox":"bwrap","workspace":"/w","bound":1000,"refused":false,"reason":"workspace folder bound"}`},
	{agent.EvRecordRepaired, agent.ActorSystem, `{"reason":"torn last line","truncated_bytes":17}`},
	{agent.EvCompactStarted, agent.ActorSystem, `{"before_tokens":9000,"after_tokens":0,"trigger":"auto"}`},
	{agent.EvCompactDone, agent.ActorSystem, `{"before_tokens":9000,"after_tokens":2000,"summary":"s","trigger":"auto"}`},
	{agent.EvContextOffloaded, agent.ActorSystem, `{"call_id":"c-1","bytes":100000}`},
	{agent.EvPlanUpdated, agent.ActorAgent, `{"items":[]}`},
	{agent.EvTodoUpdated, agent.ActorAgent, `{"items":[{"id":"1","text":"test","status":"pending"}]}`},
	{agent.EvSuggestionOffered, agent.ActorSystem, `{"text":"run the tests","turn":2}`},
	{agent.EvAgentDelta, agent.ActorAgent, `{"text":"do","n":0}`},
	{agent.EvAgentReasoningDelta, agent.ActorAgent, `{"text":"thi","n":0}`},
	{agent.EvOllamaStart, agent.ActorUser, `{"op":"start","outcome":"ok","provider":"local","model":"qwen3:8b","base":"http://127.0.0.1:11434","command":"open -a Ollama","pid":4242,"consent":"asked","by":"user","surface":"tui"}`},
	{agent.EvOllamaInstall, agent.ActorUser, `{"op":"install","outcome":"failed","provider":"local","model":"qwen3:8b","command":"brew install ollama","source":"the Homebrew formula ollama","consent":"asked","problem":"install_failed","error":"brew install ollama failed: exit status 1","by":"user","surface":"cli"}`},
	{agent.EvOllamaInstall, agent.ActorUser, `{"op":"install","outcome":"manual","provider":"local","command":"open https://ollama.com/download/mac","consent":"asked","by":"user","surface":"server"}`},
	{agent.EvOllamaPull, agent.ActorUser, `{"op":"pull","outcome":"ok","provider":"local","model":"qwen3:8b","base":"http://127.0.0.1:11434","command":"ollama pull qwen3:8b","source":"https://registry.ollama.ai","bytes":5225388164,"consent":"asked","by":"user","surface":"acp"}`},
	{agent.EvToolchainInstall, agent.ActorSystem, `{"lang":"go","version":"1.22.5","platform":"linux-arm64","outcome":"ok","why":"pinned by go.mod","source":"the Go project's releases (go.dev/dl)","url":"https://go.dev/dl/go1.22.5.linux-arm64.tar.gz","digest":"sha256:8d7e8f0c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a","bytes":65000000,"consent":"call","by":"agent","surface":"tool"}`},
	{agent.EvToolchainInstall, agent.ActorUser, `{"lang":"python","version":"3.12","outcome":"refused","error":"installing toolchains is turned off here (sandbox.toolchains off)","by":"user","surface":"acp"}`},
	{agent.EvOllamaPull, agent.ActorUser, `{"op":"pull","outcome":"refused","provider":"local","model":"qwen3:8b","problem":"assist_off","error":"Abhed is set not to install or download anything here","by":"user","surface":"server"}`},
}

// sampleLines are the samples as one session's lines, seq from 1.
func sampleLines() []Line {
	at := time.Date(2026, 10, 19, 11, 58, 2, 123456789, time.UTC)
	out := make([]Line, len(samples))
	for i, s := range samples {
		var compact json.RawMessage
		var v any
		if err := json.Unmarshal([]byte(s.payload), &v); err != nil {
			panic(string(s.typ) + ": " + err.Error())
		}
		compact, _ = json.Marshal(v)
		out[i] = Line{Event: agent.Event{
			ID: "ev-" + itoa(i+1), SessionID: "s-1", Seq: int64(i + 1), Type: s.typ, Actor: s.actor,
			Trust: agent.Trusted, CreatedAt: at.Add(time.Duration(i) * time.Second), Payload: compact,
		}}
	}
	return out
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
