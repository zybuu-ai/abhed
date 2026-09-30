package ui

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// timers is a fake time.AfterFunc driven by a fake clock.
type timers struct {
	mu    sync.Mutex
	clock *fakeClock
	due   []struct {
		at time.Time
		f  func()
	}
}

func (t *timers) after(d time.Duration, f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.due = append(t.due, struct {
		at time.Time
		f  func()
	}{t.clock.Now().Add(d), f})
}

// advance moves the clock and runs the timers now due.
func (t *timers) advance(d time.Duration) {
	t.clock.advance(d)
	t.mu.Lock()
	var run []func()
	keep := t.due[:0]
	for _, e := range t.due {
		if !e.at.After(t.clock.Now()) {
			run = append(run, e.f)
		} else {
			keep = append(keep, e)
		}
	}
	t.due = keep
	t.mu.Unlock()
	for _, f := range run {
		f()
	}
}

// dialogRig opens a dialog on a rig with a fake clock and returns a way to
// read its answer.
type dialogRig struct {
	*rig
	clock  *fakeClock
	timers *timers
	answer chan string
}

func openDialog(t *testing.T, spec DialogSpec) *dialogRig {
	g := newRig(t, 80, 30)
	clock := newFakeClock()
	tm := &timers{clock: clock}
	g.lr.d.mu.Lock()
	g.lr.d.now = clock.Now
	g.lr.d.after = tm.after
	g.lr.d.mu.Unlock()
	dr := &dialogRig{rig: g, clock: clock, timers: tm, answer: make(chan string, 1)}
	go func() {
		id, err := g.lr.Dialog(context.Background(), spec)
		if err != nil {
			id = "error: " + err.Error()
		}
		dr.answer <- id
	}()
	g.waitText(spec.Ask)
	return dr
}

// key sends k at the current fake time and waits for it to be applied.
func (dr *dialogRig) key(k string) {
	dr.keys(k)
	dr.settle()
}

func (dr *dialogRig) answered() (string, bool) {
	select {
	case id := <-dr.answer:
		return id, true
	case <-time.After(60 * time.Millisecond):
		return "", false
	}
}

func approvalSpec() DialogSpec {
	return DialogSpec{
		Kind: DialogApproval, Title: "Edit(hello.txt)", Ask: "Make this edit to hello.txt?",
		Choices: []Choice{{ID: "yes", Label: "Yes", Key: 'y'}, {ID: "always", Label: "Yes, and don't ask again for edit(hello.txt) this session"},
			{ID: "no", Label: "No, and tell Abhed what to do instead (esc)", Key: 'n'}},
		Cancel: "no",
	}
}

// Nothing is selected when an approval appears: Enter alone, however
// deliberate, answers nothing.
func TestDialogBareEnterNeverApproves(t *testing.T) {
	dr := openDialog(t, approvalSpec())
	dr.clock.advance(5 * time.Second)
	dr.key("\r")
	if id, ok := dr.answered(); ok {
		t.Fatalf("a bare Enter answered %q", id)
	}
	dr.waitText("choose with a number")
	if strings.Contains(dr.term.Text(), "❯") {
		t.Fatalf("a choice is highlighted before any was made:\n%s", dr.term.Dump())
	}
}

// ↓ then Enter within 10, 100 or 290 ms of the dialog appearing does not
// answer it — nor does any other key in that time.
func TestDialogIgnoresEveryKeyAtFirst(t *testing.T) {
	for _, at := range []time.Duration{10, 100, 290} {
		dr := openDialog(t, approvalSpec())
		dr.clock.advance(at * time.Millisecond)
		dr.key("\x1b[B")
		dr.key("\r")
		dr.key("1")
		dr.key("y")
		dr.timers.advance(time.Second)
		if id, ok := dr.answered(); ok {
			t.Fatalf("at %d ms a key answered %q", at, id)
		}
	}
}

// After the first 300 ms, ↓ then Enter at once is still not an answer: Enter
// needs 300 ms since the last arrow.
func TestDialogEnterNeedsAPauseAfterAnArrow(t *testing.T) {
	dr := openDialog(t, approvalSpec())
	dr.clock.advance(time.Second)
	dr.key("\x1b[B") // to "yes"
	dr.key("\x1b[B") // to "always"
	dr.clock.advance(50 * time.Millisecond)
	dr.key("\r")
	if id, ok := dr.answered(); ok {
		t.Fatalf("arrow then Enter answered %q", id)
	}
	dr.waitText("press Enter again")
	dr.clock.advance(400 * time.Millisecond)
	dr.key("\r")
	if id, ok := dr.answered(); !ok || id != "always" {
		t.Fatalf("a deliberate Enter answered %q, %v", id, ok)
	}
}

// A number answers only when it stands alone: a key within 300 ms before or
// after it makes it typing. A held key repeats far faster, so it never
// answers.
func TestDialogNumberMustStandAlone(t *testing.T) {
	dr := openDialog(t, approvalSpec())
	dr.clock.advance(time.Second)
	// Held: "1" repeating every 30 ms.
	for i := 0; i < 30; i++ {
		dr.key("1")
		dr.clock.advance(30 * time.Millisecond)
		dr.timers.advance(0)
	}
	dr.timers.advance(time.Second)
	if id, ok := dr.answered(); ok {
		t.Fatalf("a held key answered %q", id)
	}
	// Typing: "1" then another key 100 ms later.
	dr.clock.advance(time.Second)
	dr.key("1")
	dr.clock.advance(100 * time.Millisecond)
	dr.key("x")
	dr.timers.advance(time.Second)
	if id, ok := dr.answered(); ok {
		t.Fatalf("typing answered %q", id)
	}
	// Deliberate: alone before and after.
	dr.clock.advance(time.Second)
	dr.key("1")
	dr.timers.advance(approvalGuard)
	if id, ok := dr.answered(); !ok || id != "yes" {
		t.Fatalf("a deliberate 1 answered %q, %v", id, ok)
	}
}

// Only the choices offered can be chosen.
func TestDialogOnlyOfferedChoices(t *testing.T) {
	spec := approvalSpec()
	spec.Choices = []Choice{spec.Choices[0], spec.Choices[2]} // no "always"
	dr := openDialog(t, spec)
	dr.clock.advance(time.Second)
	dr.key("3")
	dr.timers.advance(time.Second)
	if id, ok := dr.answered(); ok {
		t.Fatalf("3 of 2 answered %q", id)
	}
	dr.clock.advance(time.Second)
	dr.key("2")
	dr.timers.advance(approvalGuard)
	if id, _ := dr.answered(); id != "no" {
		t.Fatalf("2 answered %q", id)
	}
}

// Esc declines once the guard has passed; Ctrl-C declines at once.
func TestDialogEscAndCtrlCDecline(t *testing.T) {
	dr := openDialog(t, approvalSpec())
	dr.clock.advance(time.Second)
	dr.key("\x1b")
	if id, _ := dr.answered(); id != "no" {
		t.Fatalf("Esc answered %q", id)
	}
	dr2 := openDialog(t, approvalSpec())
	dr2.key("\x03")
	if id, _ := dr2.answered(); id != "no" {
		t.Fatalf("Ctrl-C answered %q", id)
	}
}

// The dialog and its answer stay in the transcript.
func TestDialogRecordStays(t *testing.T) {
	spec := approvalSpec()
	spec.Body = []Block{viewBlock(&commandBlock{command: "rm -rf build"})}
	spec.Outcome = func(id string) string { return map[string]string{"yes": "Approved"}[id] }
	dr := openDialog(t, spec)
	dr.clock.advance(time.Second)
	dr.key("1")
	dr.timers.advance(approvalGuard)
	dr.answered()
	dr.waitText("⎿ ✓ Approved")
	all := dr.term.All()
	for _, want := range []string{"● Edit(hello.txt)", "$ rm -rf build"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "❯") {
		t.Fatalf("the choices were left on screen:\n%s", all)
	}
}

// At 40 columns every choice is shown whole: what "always" would allow is
// never cut off.
func TestDialogChoicesWrapAtFortyColumns(t *testing.T) {
	g := newRig(t, 40, 30)
	spec := approvalSpec()
	spec.Choices[1].Label = "Yes, and don't ask again for edit(src/internal/deeply/nested/file_name.go) this session"
	go func() { _, _ = g.lr.Dialog(context.Background(), spec) }()
	g.waitText(spec.Ask)
	flat := strings.Join(strings.Fields(strings.ReplaceAll(g.term.Text(), "│", "")), "")
	if !strings.Contains(flat, "edit(src/internal/deeply/nested/file_name.go)thissession") {
		t.Fatalf("the scope is cut at 40 columns:\n%s", g.term.Dump())
	}
}

// The approver: a destructive command offers no "always" and needs a second
// yes, whose default is no; an answer is reported to the loop.
func TestDialogApproverDestructiveConfirms(t *testing.T) {
	g := newRig(t, 80, 30)
	clock := newFakeClock()
	tm := &timers{clock: clock}
	g.lr.d.mu.Lock()
	g.lr.d.now, g.lr.d.after = clock.Now, tm.after
	g.lr.d.mu.Unlock()
	a := &DialogApprover{Base: NewApprover(io.Discard), Reader: g.lr, Render: NewRenderer(io.Discard, false)}
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	args, _ := json.Marshal(map[string]string{"command": "rm -rf build"})
	go func() {
		ok, err := a.Approve(context.Background(), "bash", args, policy.Result{Decision: policy.Ask, Step: "destructive", Reason: "rm -rf — always requires confirmation"})
		done <- result{ok, err}
	}()
	g.waitText("Run this command?")
	if strings.Contains(g.term.Text(), "don't ask again") {
		t.Fatalf("a destructive command offered always:\n%s", g.term.Dump())
	}
	clock.advance(time.Second)
	g.keys("1")
	g.settle()
	tm.advance(approvalGuard)
	g.waitText("Really run it?")
	// Enter takes the default, which is No.
	clock.advance(time.Second)
	g.keys("\r")
	select {
	case r := <-done:
		if r.ok || r.err != nil {
			t.Fatalf("Enter on the confirm approved: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no answer:\n%s", g.term.Dump())
	}
}

// "Always" grants the scope for the session, reported to the loop, and the
// next call in it is not asked.
func TestDialogApproverAlways(t *testing.T) {
	g := newRig(t, 80, 30)
	clock := newFakeClock()
	tm := &timers{clock: clock}
	g.lr.d.mu.Lock()
	g.lr.d.now, g.lr.d.after = clock.Now, tm.after
	g.lr.d.mu.Unlock()
	a := &DialogApprover{Base: NewApprover(io.Discard), Reader: g.lr, Render: NewRenderer(io.Discard, false)}
	res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(go test *)", Reason: "running a command needs approval in default mode"}
	args := json.RawMessage(`{"command":"go test ./..."}`)
	done := make(chan bool, 1)
	ctx, answer := agent.ExpectAnswer(context.Background())
	go func() {
		ok, _ := a.Approve(ctx, "bash", args, res)
		done <- ok
	}()
	g.waitText("policy step: default")
	clock.advance(time.Second)
	g.keys("2")
	g.settle()
	tm.advance(approvalGuard)
	if ok := <-done; !ok || answer.Granted != "bash(go test *)" {
		t.Fatalf("always: ok %v, answer %+v", ok, answer)
	}
	ctx2, answer2 := agent.ExpectAnswer(context.Background())
	if ok, _ := a.Approve(ctx2, "bash", args, res); !ok || answer2.By != agent.BySessionScope {
		t.Fatalf("the second call was asked again: %+v", answer2)
	}
	g.waitText("Allowed for this session by bash(go test *)")
}

// The dialog says who asked, as 1.2.2's prompt did, and why: the reason and
// the policy step that fired.
func TestDialogApproverSaysWhoAskedAndWhy(t *testing.T) {
	g := newRig(t, 80, 30)
	a := &DialogApprover{Base: NewApprover(io.Discard), Reader: g.lr, Render: NewRenderer(io.Discard, false)}
	ctx, cancel := context.WithCancel(agent.WithSubagent(context.Background(), "researcher"))
	defer cancel()
	go func() {
		_, _ = a.Approve(ctx, "bash", json.RawMessage(`{"command":"make test"}`),
			policy.Result{Decision: policy.Ask, Step: "ask", Reason: "matched ask rule bash(make *)"})
	}()
	g.waitText("Run this command?")
	for _, want := range []string{"asked by subagent: researcher", "matched ask rule bash(make *) · policy step: ask", "$ make test"} {
		if !strings.Contains(g.term.Text(), want) {
			t.Errorf("missing %q:\n%s", want, g.term.Dump())
		}
	}
}

// Approvals have no default, by decision: the dialog the approver builds
// selects nothing, a Yes or "always" default is refused before it is drawn,
// and Enter alone never approves, however long after the question appeared.
func TestApprovalHasNoDefault(t *testing.T) {
	a := &DialogApprover{Base: NewApprover(io.Discard), Render: NewRenderer(io.Discard, false)}
	for _, res := range []policy.Result{
		{Decision: policy.Ask, Step: "default", Scope: "bash(go test *)", Reason: "r"},
		{Decision: policy.Ask, Step: "destructive", Reason: "r"},
	} {
		spec := a.spec(context.Background(), "bash", json.RawMessage(`{"command":"go test ./..."}`), res, "Bash(go test ./...)")
		if spec.Default != "" {
			t.Fatalf("the approval dialog has a default, %q", spec.Default)
		}
		for _, c := range spec.Choices {
			withDefault := spec
			withDefault.Default = c.ID
			if c.ID != ChoiceNo {
				if _, err := withDefault.Normalized(); err == nil {
					t.Errorf("an approval with default %q was accepted", c.ID)
				}
			}
		}
	}

	g := newRig(t, 80, 30)
	clock := newFakeClock()
	g.lr.d.mu.Lock()
	g.lr.d.now = clock.Now
	g.lr.d.mu.Unlock()
	a.Reader = g.lr
	done := make(chan bool, 1)
	go func() {
		ok, _ := a.Approve(context.Background(), "bash", json.RawMessage(`{"command":"go test ./..."}`),
			policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(go test *)", Reason: "r"})
		done <- ok
	}()
	g.waitText("Run this command?")
	for i := 0; i < 5; i++ {
		clock.advance(2 * time.Second)
		g.keys("\r")
		g.settle()
	}
	select {
	case ok := <-done:
		t.Fatalf("Enter alone answered the approval (approved %v)", ok)
	case <-time.After(200 * time.Millisecond):
	}
	if strings.Contains(g.term.Text(), "❯") {
		t.Fatalf("a choice is selected before any was made:\n%s", g.term.Dump())
	}
	clock.advance(2 * time.Second)
	g.keys("\x1b")
	if ok := <-done; ok {
		t.Fatal("Esc approved")
	}
}

// With no key before it, a number still does not count in the first 300 ms
// the dialog is on screen: the guard, not the quiet rule, holds it.
func TestDialogLoneNumberInsideTheGuard(t *testing.T) {
	for _, at := range []time.Duration{10, 100, 290} {
		dr := openDialog(t, approvalSpec())
		dr.clock.advance(at * time.Millisecond)
		dr.key("1")
		dr.clock.advance(time.Second)
		dr.timers.advance(time.Second)
		if id, ok := dr.answered(); ok {
			t.Fatalf("a lone 1 at %d ms answered %q", at, id)
		}
	}
}

// After the guard, a key 100 ms before a number, or before Enter, makes it
// typing, even with silence after.
func TestDialogNeedsQuietBefore(t *testing.T) {
	dr := openDialog(t, approvalSpec())
	dr.clock.advance(time.Second)
	dr.key("x")
	dr.clock.advance(100 * time.Millisecond)
	dr.key("1")
	dr.clock.advance(time.Second)
	dr.timers.advance(time.Second)
	if id, ok := dr.answered(); ok {
		t.Fatalf("x then 1 answered %q", id)
	}

	dr2 := openDialog(t, approvalSpec())
	dr2.clock.advance(time.Second)
	dr2.key("\x1b[B") // select Yes
	dr2.clock.advance(time.Second)
	dr2.key("x")
	dr2.clock.advance(100 * time.Millisecond)
	dr2.key("\r")
	if id, ok := dr2.answered(); ok {
		t.Fatalf("Enter 100 ms after a key answered %q", id)
	}
	dr2.clock.advance(time.Second)
	dr2.key("\r")
	if id, _ := dr2.answered(); id != "yes" {
		t.Fatalf("a deliberate Enter answered %q", id)
	}
}

// Esc inside the guard is ignored like every other key.
func TestDialogEscInsideTheGuard(t *testing.T) {
	dr := openDialog(t, approvalSpec())
	dr.clock.advance(100 * time.Millisecond)
	dr.key("\x1b")
	if id, ok := dr.answered(); ok {
		t.Fatalf("Esc inside the guard answered %q", id)
	}
}

// A paste is never a choice, whatever it holds.
func TestDialogPasteIsNeverAChoice(t *testing.T) {
	for _, body := range []string{"1", "y", "2", "\r"} {
		dr := openDialog(t, approvalSpec())
		dr.clock.advance(time.Second)
		dr.key("\x1b[200~" + body + "\x1b[201~")
		dr.clock.advance(time.Second)
		dr.timers.advance(time.Second)
		if id, ok := dr.answered(); ok {
			t.Fatalf("a paste of %q answered %q", body, id)
		}
	}
}
