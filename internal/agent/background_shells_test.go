//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// stepAdapter answers each turn from the request it is given, so a call can
// name a shell id an earlier result returned.
type stepAdapter struct {
	mu    sync.Mutex
	steps []func(req model.Request) scriptedTurn
	seen  int
}

func (*stepAdapter) Name() string                           { return "step" }
func (*stepAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (*stepAdapter) CountTokens(model.Request) (int, error) { return 0, nil }

func (s *stepAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	s.mu.Lock()
	turn := scriptedTurn{text: "done"}
	if s.seen < len(s.steps) {
		turn = s.steps[s.seen](req)
	}
	s.seen++
	s.mu.Unlock()
	ch := make(chan model.Chunk, len(turn.calls)+4)
	if turn.text != "" {
		ch <- model.Chunk{Type: model.ChunkText, Text: turn.text}
	}
	for i := range turn.calls {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &turn.calls[i]}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

var shellIDIn = regexp.MustCompile(`sh_[0-9a-z]+`)

// lastShellID is the shell id in the conversation's latest tool result.
func lastShellID(req model.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if m := req.Messages[i]; m.Role == model.RoleTool {
			return shellIDIn.FindString(m.Content)
		}
	}
	return ""
}

type shellRig struct {
	l     *Loop
	store *MemStore
}

func newShellRig(t *testing.T, wake WakeMode, mode policy.Mode, pol BackgroundPolicy, a model.Adapter) *shellRig {
	t.Helper()
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("API_TOKEN", "tok-9f8e7d"); err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "parent", "")
	rec.Redact = vault.Redactor()
	reg := tools.NewRegistry(tools.Bash{Secrets: vault.Env, SecretNames: []string{"API_TOKEN"}}, ShellOutput{}, ShellKill{}, TaskStatus{})
	pe := policy.New(mode)
	_ = pe.AddAllow("secret(API_TOKEN)")
	if a == nil {
		a = &scriptedAdapter{}
	}
	l := NewLoop(a, reg, pe, AutoApprove{Yes: true}, sess, rec, DefaultConfig())
	pol.Wake = wake
	if pol.MaxShells == 0 {
		pol.MaxShells = DefaultMaxShells
	}
	pol.MaxLive, pol.Settle = 4, 20*time.Millisecond
	NewBackground(l, pol)
	t.Cleanup(func() { l.Background.Close(TermSessionClosed) })
	return &shellRig{l: l, store: store}
}

func (r *shellRig) events(t *testing.T) []Event {
	t.Helper()
	evs, err := r.store.Events("parent")
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// run calls a tool as the loop would for call c1, without policy.
func (r *shellRig) run(t *testing.T, name string, args any) tools.Result {
	t.Helper()
	tool, ok := r.l.Tools.Get(name)
	if !ok {
		t.Fatalf("no %s tool", name)
	}
	raw, _ := json.Marshal(args)
	return tool.Run(r.l.withShellHost(r.l.asParent(context.Background()), "c1"), r.l.Session, raw)
}

func (r *shellRig) start(t *testing.T, command string) string {
	t.Helper()
	res := r.run(t, "bash", map[string]any{"command": command, "description": "bg", "run_in_background": true})
	if res.IsError || !strings.HasPrefix(res.Content, "Started in background: sh_") {
		t.Fatalf("start: %+v", res)
	}
	return shellIDIn.FindString(res.Content)
}

func (r *shellRig) pid(t *testing.T, id string) int {
	t.Helper()
	sh, ok := r.l.Background.shell(id)
	if !ok {
		t.Fatalf("no shell %s", id)
	}
	return sh.shell.proc.Pid()
}

// spawner starts two children that print their pids, then waits for them.
const spawner = "sleep 30 & echo pid:$!; sleep 30 & echo pid:$!; echo ready; wait"

var pidIn = regexp.MustCompile(`pid:(\d+)`)

// children waits for a spawner shell and returns its pids and its children's,
// checking the shell leads a process group of its own, never this one's.
func (r *shellRig) children(t *testing.T, id string) []int {
	t.Helper()
	var out string
	waitFor(t, "ready", func() bool {
		out += r.run(t, "shell_output", map[string]any{"shell_id": id}).Content
		return strings.Contains(out, "ready")
	})
	leader := r.pid(t, id)
	if pgid, err := syscall.Getpgid(leader); err != nil || pgid != leader || pgid == syscall.Getpgrp() {
		t.Fatalf("shell %d is not in a group of its own: pgid %d (ours %d)", leader, pgid, syscall.Getpgrp())
	}
	pids := []int{leader}
	for _, m := range pidIn.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(m[1])
		pids = append(pids, n)
	}
	if len(pids) != 3 {
		t.Fatalf("pids %v from %q", pids, out)
	}
	return pids
}

// allGone reports that none of pids, each a process this test started, is
// running. It probes by exact pid with signal 0, which delivers nothing.
func allGone(pids []int) bool {
	for _, p := range pids {
		if p > 1 && !errors.Is(syscall.Kill(p, 0), syscall.ESRCH) {
			return false
		}
	}
	return true
}

// A background command returns at once; its output is read in parts, each
// read returning only what is new, and its exit code is reported and recorded.
func TestShellStartReadAndExit(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	begun := time.Now()
	id := r.start(t, "printf 'one\\n'; sleep 0.5; printf 'two\\n'; exit 3")
	if time.Since(begun) > 400*time.Millisecond {
		t.Fatal("the start waited for the command")
	}
	waitFor(t, "the first line", func() bool {
		return strings.Contains(r.run(t, "shell_output", map[string]any{"shell_id": id}).Content, "one")
	})
	res := r.run(t, "shell_output", map[string]any{"shell_id": id, "wait_ms": 5000})
	if !strings.Contains(res.Content, "two") || strings.Contains(res.Content, "one") {
		t.Fatalf("second read is not just the new output: %q", res.Content)
	}
	if !strings.Contains(res.Content, "exited 3") || res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("exit not reported: %+v", res)
	}
	started := payloads[ShellStarted](r.events(t), EvShellStarted)
	if len(started) != 1 || started[0].ShellID != id || started[0].CallID != "c1" || !strings.Contains(started[0].Command, "printf") || started[0].Sandbox != "none" {
		t.Fatalf("shell.started: %+v", started)
	}
	waitFor(t, "shell.ended", func() bool { return len(payloads[ShellEnded](r.events(t), EvShellEnded)) == 1 })
	end := payloads[ShellEnded](r.events(t), EvShellEnded)[0]
	if end.ExitCode != 3 || end.State != ShellExited || end.OutputBytes != 8 || end.Truncated {
		t.Fatalf("shell.ended: %+v", end)
	}
	ti, ok := r.l.Background.Task(id)
	if !ok || ti.Kind != KindShell || ti.Status != ShellExited || ti.ExitCode == nil || *ti.ExitCode != 3 || ti.LastLine != "two" {
		t.Fatalf("task info: %+v", ti)
	}
}

// Killing a shell ends every process it started: its whole process group.
func TestShellKillLeavesNoProcess(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	id := r.start(t, spawner)
	pids := r.children(t, id)
	if allGone(pids) {
		t.Fatal("the processes are gone before the kill")
	}
	res := r.run(t, "shell_kill", map[string]any{"shell_id": id})
	if res.IsError || !strings.Contains(res.Content, "killed") {
		t.Fatalf("kill: %+v", res)
	}
	waitFor(t, "every process of the shell to end", func() bool { return allGone(pids) })
	if ti, _ := r.l.Background.Task(id); ti.Status != ShellKilled {
		t.Fatalf("status %q, want killed", ti.Status)
	}
	if end := payloads[ShellEnded](r.events(t), EvShellEnded); len(end) != 1 || end[0].Reason != string(TermCancelledByParent) {
		t.Fatalf("shell.ended: %+v", end)
	}
	// The agent's own kill returned the end; no notice repeats it.
	if r.l.Background.Pending() != 0 {
		t.Fatal("a notice was queued for the agent's own kill")
	}
	if res := r.run(t, "shell_kill", map[string]any{"shell_id": id}); !strings.Contains(res.Content, "killed") {
		t.Fatalf("a second kill: %+v", res)
	}
}

// Output past the cap is dropped from the front, and a read says so.
func TestShellOutputCapped(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{ShellOutputCap: 1024}, nil)
	id := r.start(t, "head -c 5000 /dev/zero | tr '\\0' 'x'; echo; echo tail-line")
	res := r.run(t, "shell_output", map[string]any{"shell_id": id, "wait_ms": 5000})
	if !strings.Contains(res.Content, "bytes of earlier output were dropped") || !res.Truncated {
		t.Fatalf("no truncation marker: %q", res.Content[:min(200, len(res.Content))])
	}
	if !strings.Contains(res.Content, "tail-line") || len(res.Content) > 1024+300 {
		t.Fatalf("the read is not the last 1024 bytes: %d bytes", len(res.Content))
	}
	waitFor(t, "shell.ended", func() bool { return len(payloads[ShellEnded](r.events(t), EvShellEnded)) == 1 })
	if end := payloads[ShellEnded](r.events(t), EvShellEnded)[0]; !end.Truncated || end.OutputBytes != 5011 {
		t.Fatalf("shell.ended: %+v", end)
	}
}

// Plan mode refuses a background command, by policy and by the host.
func TestShellRefusedInPlanMode(t *testing.T) {
	a := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]any{"command": "echo hi", "description": "bg", "run_in_background": true})}},
		{text: "done"},
	}}
	r := newShellRig(t, WakeNotify, policy.ModePlan, BackgroundPolicy{}, a)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := r.events(t)
	if !hasEvent(evs, EvActionDenied) || hasEvent(evs, EvShellStarted) || len(r.l.Background.Tasks()) != 0 {
		t.Fatal("plan mode started a background command")
	}
	_, err := shellHost{b: r.l.Background}.StartShell(context.Background(), tools.ShellRequest{Command: "echo hi"})
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("host under plan mode: %v", err)
	}
}

// A deny rule applies to a background command as to a foreground one.
func TestShellDenyRuleApplies(t *testing.T) {
	a := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]any{"command": "echo forbidden-word", "description": "bg", "run_in_background": true})}},
		{text: "done"},
	}}
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, a)
	if err := r.l.Policy.AddDeny("bash(*forbidden-word*)"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := r.events(t)
	denied := payloads[map[string]string](evs, EvActionDenied)
	if len(denied) != 1 || denied[0]["rule"] != "bash(*forbidden-word*)" || hasEvent(evs, EvShellStarted) {
		t.Fatalf("the deny rule did not refuse the background command: %v", denied)
	}
	req := payloads[ActionRequested](evs, EvActionRequested)
	if len(req) != 1 || !strings.Contains(string(req[0].Args), `"run_in_background":true`) {
		t.Fatalf("run_in_background was not kept in the canonical arguments: %+v", req)
	}
}

// A secret's value written by a background command is redacted wherever it
// shows: the read the model gets, the record, the notice and the listing.
func TestShellSecretsRedacted(t *testing.T) {
	a := &stepAdapter{steps: []func(model.Request) scriptedTurn{
		func(model.Request) scriptedTurn {
			return scriptedTurn{calls: []model.ToolCall{{ID: "c1", Name: "bash", Args: json.RawMessage(
				`{"command":"echo token=$API_TOKEN","description":"use it","secrets":["API_TOKEN"],"run_in_background":true}`)}}}
		},
		func(req model.Request) scriptedTurn {
			return scriptedTurn{calls: []model.ToolCall{{ID: "c2", Name: "shell_output", Args: json.RawMessage(
				`{"shell_id":"` + lastShellID(req) + `","wait_ms":5000}`)}}}
		},
	}}
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, a)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := r.events(t)
	read := false
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "tok-9f8e7d") {
			t.Fatalf("the value reached the record: %s", e.Payload)
		}
		if e.Type == EvObservation && strings.Contains(string(e.Payload), "token=[secret:API_TOKEN]") {
			read = true
		}
	}
	if !read {
		t.Fatal("the command did not get the secret, or its output was not redacted")
	}
	for _, ti := range r.l.Background.Tasks() {
		if strings.Contains(ti.LastLine, "tok-9f8e7d") || ti.LastLine != "token=[secret:API_TOKEN]" {
			t.Fatalf("listing: %+v", ti)
		}
	}
	for _, n := range r.l.Background.peekNotices() {
		if strings.Contains(n.Content, "tok-9f8e7d") {
			t.Fatal("the notice holds the value")
		}
	}
}

// Closing the session, and a stop of all background work, kill its shells.
func TestShellKilledBySessionEndAndStop(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	stopped := r.start(t, "sleep 30 & wait")
	r.l.Background.CancelAll(TermUserInterrupt)
	if ti, _ := r.l.Background.Task(stopped); ti.Status != ShellKilled || ti.Reason != string(TermUserInterrupt) {
		t.Fatalf("after a stop: %+v", ti)
	}
	pids := r.children(t, r.start(t, spawner))
	r.l.Background.Close(TermSessionClosed)
	waitFor(t, "every process of the shell to end", func() bool { return allGone(pids) })
	ends := payloads[ShellEnded](r.events(t), EvShellEnded)
	if len(ends) != 2 || ends[1].Reason != string(TermSessionClosed) || ends[1].State != ShellKilled {
		t.Fatalf("shell.ended: %+v", ends)
	}
	if _, err := (shellHost{b: r.l.Background}).StartShell(context.Background(), tools.ShellRequest{Command: "true"}); err == nil {
		t.Fatal("a shell started after the session closed")
	}
}

// The process's exit ends every background shell it started.
func TestShellEndedOnProcessExit(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	pids := r.children(t, r.start(t, spawner))
	tools.EndBackgroundShells(5 * time.Second)
	waitFor(t, "every process of the shell to end", func() bool { return allGone(pids) })
}

// No more shells than the limit run at once; one ending frees its slot.
func TestShellConcurrentLimit(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{MaxShells: 2}, nil)
	first := r.start(t, "sleep 30")
	r.start(t, "sleep 30")
	res := r.run(t, "bash", map[string]any{"command": "sleep 30", "description": "bg", "run_in_background": true})
	if !res.IsError || !strings.Contains(res.Content, "limit reached (2 of 2") {
		t.Fatalf("a third shell: %+v", res)
	}
	r.run(t, "shell_kill", map[string]any{"shell_id": first})
	r.start(t, "sleep 30")
	// Shells do not take the subagents' slots.
	if err := r.l.Background.reserve(); err != nil {
		t.Fatalf("shells took a task slot: %v", err)
	}
	r.l.Background.unreserve()
}

// Where the run waits for its background work (-p), a shell is joined: the
// run takes its end as a notice, rebuilt by Fork as a task_status result.
func TestShellJoinedWhenWakeIsOff(t *testing.T) {
	a := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]any{"command": "sleep 0.3; echo built", "description": "build", "run_in_background": true})}},
		{text: "started"},
	}}
	r := newShellRig(t, WakeOff, policy.ModeBypass, BackgroundPolicy{}, a)
	reason, err := r.l.Run(context.Background(), "go")
	if err != nil || reason != TermCompleted {
		t.Fatalf("run: %s %v", reason, err)
	}
	ns := payloads[Notice](r.events(t), EvSubagentNotice)
	if len(ns) != 1 || ns[0].Kind != KindShell || ns[0].Status != ShellExited || !strings.Contains(ns[0].Content, "Last line: built") {
		t.Fatalf("notice: %+v", ns)
	}
	if len(a.gotRequests) != 3 {
		t.Fatalf("the run did not go on with the shell's end: %d requests", len(a.gotRequests))
	}
	messagesEqualFork(t, &bgRig{l: r.l, store: r.store})
}

// Without a host, as in a subagent, a background command is refused.
func TestShellNeedsAHost(t *testing.T) {
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := tools.Bash{}.Run(context.Background(), sess,
		json.RawMessage(`{"command":"echo hi","description":"x","run_in_background":true}`))
	if !res.IsError || !strings.Contains(res.Content, "not available here") {
		t.Fatalf("%+v", res)
	}
}

// A subagent cannot start a background shell: its call is refused with a
// reason, and nothing starts on the parent's session.
func TestSubagentCannotStartABackgroundShell(t *testing.T) {
	store := NewMemStore()
	a := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("task", map[string]string{"prompt": "serve", "description": "serve"})}},
		{calls: []model.ToolCall{call("bash", map[string]any{"command": "sleep 30", "description": "bg", "run_in_background": true})}},
		{text: "could not"},
		{text: "done"},
	}}
	l, _, _ := taskTree(t, a, AutoApprove{Yes: true}, store, store, false)
	NewBackground(l, BackgroundPolicy{MaxShells: DefaultMaxShells, MaxLive: 4, Settle: 20 * time.Millisecond, Wake: WakeNotify})
	t.Cleanup(func() { l.Background.Close(TermSessionClosed) })
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	parent, _ := store.Events("parent")
	if hasEvent(parent, EvShellStarted) || len(l.Background.Tasks()) != 0 {
		t.Fatalf("a subagent started a shell on the parent: %s", types(parent))
	}
	var child string
	for _, s := range payloads[spawnPayload](parent, EvSubagentSpawned) {
		child = s.Session
	}
	evs, _ := store.Events(child)
	obs := payloads[Observation](evs, EvObservation)
	if len(obs) != 1 || !obs[0].IsError || !strings.Contains(obs[0].Content, "a subagent cannot start a background command") {
		t.Fatalf("the subagent's call was not refused with its reason: %+v", obs)
	}
}
