//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// reqs are the requests sent, for a test that checks what the model saw.
	reqs []model.Request
}

func (*stepAdapter) Name() string                           { return "step" }
func (*stepAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (*stepAdapter) CountTokens(model.Request) (int, error) { return 0, nil }

func (s *stepAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
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
	l.Movable = true
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
	// The pause outlasts shellQuietRelease, so the first line shows on its own.
	id := r.start(t, "printf 'one\\n'; sleep 2; printf 'two\\n'; exit 3")
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
	// What the model was sent, the shell's output and its end notice included,
	// carries the name, never the value: the record redacting is not enough.
	if _, err := r.l.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	named := false
	for _, req := range a.reqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "tok-9f8e7d") {
				t.Fatalf("a request to the model carried the value: %q", m.Content)
			}
			named = named || strings.Contains(m.Content, "[secret:API_TOKEN]")
		}
	}
	if !named {
		t.Fatal("no request carried the shell's redacted output")
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
	asked := &bashAskCounter{}
	l, _, _ := taskTree(t, a, asked, store, store, false)
	NewBackground(l, BackgroundPolicy{MaxShells: DefaultMaxShells, MaxLive: 4, Settle: 20 * time.Millisecond, Wake: WakeNotify})
	t.Cleanup(func() { l.Background.Close(TermSessionClosed) })
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// Refused before anyone is asked: it used to be approved, then refused.
	if n := asked.bash.Load(); n != 0 {
		t.Fatalf("the person was asked %d times about a start that could not happen", n)
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
	denied := payloads[map[string]string](evs, EvActionDenied)
	if len(denied) != 1 || denied[0]["step"] != "precheck" || !strings.Contains(denied[0]["reason"], "a subagent cannot start a background command") {
		t.Fatalf("the subagent's call was not refused with its reason: %+v", denied)
	}
}

// bashAskCounter approves everything and counts the bash asks.
type bashAskCounter struct{ bash atomic.Int32 }

func (c *bashAskCounter) Approve(_ context.Context, tool string, _ json.RawMessage, _ policy.Result) (bool, error) {
	if tool == "bash" {
		c.bash.Add(1)
	}
	return true, nil
}

// shell_output redacts across reads: a secret split by the read cursor is
// held back until it is whole, and a part of one left by a gap is not shown.
func TestShellReadRedactsAcrossReads(t *testing.T) {
	const secret = "sk-test-0123456789abcdef"
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("API_KEY", secret); err != nil {
		t.Fatal(err)
	}
	l, _ := suggestLoop(t, &suggestStub{})
	l.Recorder.Redact = vault.Redactor()
	b := &Background{loop: l}

	sh := &shellState{}
	first, _ := sh.redactRead(b, tools.ShellRead{Text: "key: sk-test-0123"}, false)
	second, _ := sh.redactRead(b, tools.ShellRead{Text: "456789abcdef ok\n"}, true)
	got := first + second
	if strings.Contains(got, "0123") || strings.Contains(got, "abcdef") || !strings.Contains(got, "[secret:API_KEY]") || !strings.HasSuffix(got, " ok\n") {
		t.Fatalf("across reads: %q + %q", first, second)
	}

	// At a gap, a fixed length is skipped, enough for any cut secret's end.
	gap := &shellState{carry: "sk-test-01"}
	tail := strings.Repeat("more output\n", 30)
	out, skipped := gap.redactRead(b, tools.ShellRead{Text: "23456789abcdef " + tail, Dropped: 100}, true)
	in := len("23456789abcdef ") + len(tail)
	if strings.Contains(out, "abcdef") || out != ("23456789abcdef " + tail)[shellHold(len(secret)):] || skipped != int64(len("sk-test-01"))+int64(shellHold(len(secret))) || in-shellHold(len(secret)) != len(out) {
		t.Fatalf("after a gap: %q, skipped %d", out, skipped)
	}

	// While the shell runs, a read holds back a fixed tail, whatever it says;
	// once the shell has gone quiet, the read shows it all.
	running := &shellState{}
	long := strings.Repeat("x", 300) + "server ready\n"
	if out, _ := running.redactRead(b, tools.ShellRead{Text: long}, false); out != long[:len(long)-shellHold(len(secret))] {
		t.Fatalf("running read: %q", out)
	}
	if out, _ := running.redactRead(b, tools.ShellRead{Quiet: shellQuietRelease}, false); out != long[len(long)-shellHold(len(secret)):] {
		t.Fatalf("quiet read: %q", out)
	}
	plain := &shellState{}
	if out, _ := plain.redactRead(b, tools.ShellRead{Text: "server ready\n", Quiet: shellQuietRelease}, false); out != "server ready\n" {
		t.Fatalf("plain output of a quiet shell held back: %q", out)
	}
	if out, _ := plain.redactRead(b, tools.ShellRead{Text: "key: sk-test-0123456789abcdef\n", Quiet: shellQuietRelease}, false); out != "key: [secret:API_KEY]\n" {
		t.Fatalf("a quiet shell's secret: %q", out)
	}
}

// A command's output reaches the model redacted as the record keeps it, not
// only in the record.
func TestCommandSecretRedactedForTheModel(t *testing.T) {
	a := &stepAdapter{steps: []func(model.Request) scriptedTurn{
		func(model.Request) scriptedTurn {
			return scriptedTurn{calls: []model.ToolCall{{ID: "c1", Name: "bash", Args: json.RawMessage(
				`{"command":"echo token=$API_TOKEN","description":"use it","secrets":["API_TOKEN"]}`)}}}
		},
	}}
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, a)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	named := false
	for _, req := range a.reqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "tok-9f8e7d") {
				t.Fatalf("a request to the model carried the value: %q", m.Content)
			}
			named = named || strings.Contains(m.Content, "[secret:API_TOKEN]")
		}
	}
	if !named {
		t.Fatal("the command's output did not reach the model redacted")
	}
}

// shell.started names the tier the command was built under: a sandbox chosen
// as its first command is built was read before, and named the one before.
func TestShellStartedNamesTheTierItWasBuiltUnder(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	chosen := "none"
	_, err := shellHost{b: r.l.Background}.StartShell(context.Background(), tools.ShellRequest{
		Command: "true", Tier: "none", RanUnder: func() string { return chosen },
		Build: func(ctx context.Context) (*exec.Cmd, error) {
			chosen = "process" // the sandbox is chosen as the command is built
			return exec.CommandContext(ctx, "true"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := payloads[ShellStarted](r.events(t), EvShellStarted)
	if len(started) != 1 || started[0].Sandbox != "process" {
		t.Fatalf("shell.started: %+v", started)
	}
}

// A read of a shell still running ends with how to stop it; an ended one's
// does not.
func TestShellOutputSaysHowToStopARunningShell(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	id := r.start(t, "sleep 30")
	if res := r.run(t, "shell_output", map[string]any{"shell_id": id}); !strings.Contains(res.Content, "stop it with shell_kill (shell_id "+id+")") {
		t.Fatalf("running: %s", res.Content)
	}
	r.run(t, "shell_kill", map[string]any{"shell_id": id})
	if res := r.run(t, "shell_output", map[string]any{"shell_id": id}); strings.Contains(res.Content, "shell_kill") {
		t.Fatalf("ended: %s", res.Content)
	}
}

// Ctrl-B moves a running foreground command to the background: the call
// returns with what it wrote so far and the shell's id, the command goes on
// as a background shell, recorded as moved, and its later output is read
// with shell_output.
func TestForegroundCommandMovesToTheBackground(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	if n := r.l.MoveToBackground(); n != 0 {
		t.Fatalf("moved %d with nothing running", n)
	}
	ctx, forget := r.l.withDetach(r.l.withShellHost(r.l.asParent(context.Background()), "c9"))
	defer forget()
	tool, _ := r.l.Tools.Get("bash")
	raw, _ := json.Marshal(map[string]any{"command": "echo before; sleep 1; echo after", "description": "slow"})
	done := make(chan tools.Result, 1)
	go func() { done <- tool.Run(ctx, r.l.Session, raw) }()
	waitFor(t, "the command to run", func() bool { return r.l.MoveToBackground() == 1 })
	var res tools.Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not return when moved")
	}
	id := shellIDIn.FindString(res.Content)
	if res.IsError || id == "" || !strings.Contains(res.Content, "moved this command to the background") {
		t.Fatalf("result: %+v", res)
	}
	started := payloads[ShellStarted](r.events(t), EvShellStarted)
	if len(started) != 1 || !started[0].FromForeground || started[0].CallID != "c9" {
		t.Fatalf("shell.started: %+v", started)
	}
	waitFor(t, "the moved shell to end", func() bool { return hasEvent(r.events(t), EvShellEnded) })
	// The move can land before or after "before" is read; either way each
	// line is shown once, in the call's result or in shell_output.
	out := r.run(t, "shell_output", map[string]any{"shell_id": id}).Content
	if !strings.Contains(out, "after") || !strings.Contains(out, "exited 0") || strings.Count(res.Content+out, "before") != 1 {
		t.Fatalf("result: %s\nshell_output: %s", res.Content, out)
	}
}

// A command that ends normally is not moved, and a moved one outlives the
// call that started it but not the session.
func TestForegroundCommandNotMovedRunsAsBefore(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	ctx, forget := r.l.withDetach(r.l.withShellHost(r.l.asParent(context.Background()), "c1"))
	defer forget()
	tool, _ := r.l.Tools.Get("bash")
	raw, _ := json.Marshal(map[string]any{"command": "echo hi", "description": "quick"})
	res := tool.Run(ctx, r.l.Session, raw)
	if res.IsError || !strings.Contains(res.Content, "hi") || strings.Contains(res.Content, "background") {
		t.Fatalf("%+v", res)
	}
	if hasEvent(r.events(t), EvShellStarted) || r.l.MoveToBackground() != 0 {
		t.Fatal("a finished command was moved")
	}
}

// A moved command is stopped by shell_kill, as any background shell is.
func TestMovedCommandStopsOnShellKill(t *testing.T) {
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
	ctx, forget := r.l.withDetach(r.l.withShellHost(r.l.asParent(context.Background()), "c2"))
	defer forget()
	tool, _ := r.l.Tools.Get("bash")
	raw, _ := json.Marshal(map[string]any{"command": "sleep 30", "description": "long"})
	done := make(chan tools.Result, 1)
	go func() { done <- tool.Run(ctx, r.l.Session, raw) }()
	waitFor(t, "the command to run", func() bool { return r.l.MoveToBackground() == 1 })
	id := shellIDIn.FindString((<-done).Content)
	start := time.Now()
	r.run(t, "shell_kill", map[string]any{"shell_id": id})
	waitFor(t, "the moved shell to end", func() bool { return hasEvent(r.events(t), EvShellEnded) })
	if time.Since(start) > 10*time.Second {
		t.Fatal("shell_kill did not stop the moved command")
	}
	ended := payloads[ShellEnded](r.events(t), EvShellEnded)
	if len(ended) != 1 || ended[0].State != ShellKilled {
		t.Fatalf("shell.ended: %+v", ended)
	}
}

// gatedAdapter answers once gate is closed, or ends with the call.
type gatedAdapter struct{ gate chan struct{} }

func (g *gatedAdapter) Name() string { return "gated" }
func (g *gatedAdapter) Profile() model.Profile {
	return model.Profile{Name: "gated", ContextWindow: 100000}
}
func (g *gatedAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (g *gatedAdapter) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	go func() {
		defer close(ch)
		select {
		case <-g.gate:
			ch <- model.Chunk{Type: model.ChunkText, Text: "child done"}
			ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
		case <-ctx.Done():
			ch <- model.Chunk{Type: model.ChunkError, Err: ctx.Err()}
		}
	}()
	return ch, nil
}

// Ctrl-B moves a running foreground subagent to the background: the call
// returns as a background start does, the parent's record says the person
// moved it, and its result arrives later as a background task's, with its
// return marked background.
func TestForegroundSubagentMovesToTheBackground(t *testing.T) {
	store := NewMemStore()
	g := &gatedAdapter{gate: make(chan struct{})}
	l, _, _ := taskTree(t, g, AutoApprove{Yes: true}, store, store, false)
	NewBackground(l, BackgroundPolicy{MaxShells: DefaultMaxShells, MaxLive: 4, Settle: 20 * time.Millisecond, Wake: WakeNotify})
	l.Movable = true
	t.Cleanup(func() { l.Background.Close(TermSessionClosed) })
	ctx, forget := l.withDetach(l.withShellHost(l.asParent(context.Background()), "c1"))
	defer forget()
	tool, _ := l.Tools.Get("task")
	raw, _ := json.Marshal(map[string]string{"prompt": "slow work", "description": "slow"})
	done := make(chan tools.Result, 1)
	go func() { done <- tool.Run(ctx, l.Session, raw) }()
	waitFor(t, "the subagent to run", func() bool { return l.MoveToBackground() == 1 })
	var res tools.Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not return when moved")
	}
	if res.IsError || !strings.Contains(res.Content, "Started in background: task_id") || !strings.Contains(res.Content, "moved this subagent") {
		t.Fatalf("result: %+v", res)
	}
	if n := len(l.Background.Tasks()); n != 1 {
		t.Fatalf("%d background tasks", n)
	}
	close(g.gate)
	waitFor(t, "its return", func() bool {
		evs, _ := store.Events("parent")
		for _, r := range payloads[map[string]any](evs, EvSubagentReturn) {
			if r["background"] == true && r["task_id"] != nil {
				return true
			}
		}
		return false
	})
	evs, _ := store.Events("parent")
	moved := payloads[map[string]any](evs, EvSubagentBackgrounded)
	if len(moved) != 1 || moved[0]["by"] != ByUser || moved[0]["task_id"] == nil {
		t.Fatalf("subagent.backgrounded: %+v", moved)
	}
	if open := unreturned(evs); len(open) != 0 {
		t.Fatalf("still owed: %v", open)
	}
	waitFor(t, "its notice", func() bool { return l.Background.Pending() > 0 || hasEvent(evs, EvSubagentNotice) })
}

// A subagent moved to the background with no return yet is owed, so a crash
// reconciles it as a background child's.
func TestMovedSubagentIsOwedUntilItReturns(t *testing.T) {
	spawned, _ := json.Marshal(map[string]any{"session": "s-kid", "description": "slow"})
	moved, _ := json.Marshal(map[string]any{"session": "s-kid", "task_id": "s-kid", "background": true})
	evs := []Event{{Seq: 1, Type: EvSubagentSpawned, Payload: spawned}, {Seq: 2, Type: EvSubagentBackgrounded, Payload: moved}}
	if open := unreturned(evs); len(open) != 1 || open[0] != "s-kid" {
		t.Fatalf("owed: %v", open)
	}
}

// Not moved, a subagent run where it could be moves nothing: its summary is
// the call's result, as before.
func TestMovableSubagentNotMovedReturnsItsSummary(t *testing.T) {
	store := NewMemStore()
	g := &gatedAdapter{gate: make(chan struct{})}
	close(g.gate)
	l, _, _ := taskTree(t, g, AutoApprove{Yes: true}, store, store, false)
	NewBackground(l, BackgroundPolicy{MaxShells: DefaultMaxShells, MaxLive: 4, Settle: 20 * time.Millisecond, Wake: WakeNotify})
	l.Movable = true
	t.Cleanup(func() { l.Background.Close(TermSessionClosed) })
	ctx, forget := l.withDetach(l.withShellHost(l.asParent(context.Background()), "c1"))
	defer forget()
	tool, _ := l.Tools.Get("task")
	raw, _ := json.Marshal(map[string]string{"prompt": "quick", "description": "quick"})
	if res := tool.Run(ctx, l.Session, raw); res.IsError || !strings.Contains(res.Content, "child done") {
		t.Fatalf("%+v", res)
	}
	if len(l.Background.Tasks()) != 0 {
		t.Fatal("a subagent not moved became a background task")
	}
}

// After a gap, the skip never cuts through a whole stored value: one that
// straddles the skip point is skipped whole or shown redacted, for any
// lead-in length.
func TestShellGapSkipKeepsWholeValues(t *testing.T) {
	const standIn = "sv-standin-7b3d1e9f0a"
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("STAND_IN", standIn); err != nil {
		t.Fatal(err)
	}
	l, _ := suggestLoop(t, &suggestStub{})
	l.Recorder.Redact = vault.Redactor()
	b := &Background{loop: l}
	tail := strings.Repeat("#", 300)
	for lead := 200; lead <= 260; lead++ {
		for _, final := range []bool{true, false} {
			sh := &shellState{}
			r := tools.ShellRead{Text: strings.Repeat(".", lead) + standIn + tail, Dropped: 1}
			if !final {
				r.Quiet = shellQuietRelease
			}
			out, _ := sh.redactRead(b, r, final)
			shown := strings.ReplaceAll(out, "[secret:STAND_IN]", "")
			for i := 0; i+3 <= len(standIn); i++ {
				if strings.Contains(shown, standIn[i:i+3]) {
					t.Fatalf("lead-in %d: part %q of the value shown in %q", lead, standIn[i:i+3], out)
				}
			}
		}
	}
}
