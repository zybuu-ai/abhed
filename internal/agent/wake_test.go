package agent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// wakeHost stands in for a surface: it starts wake runs on the loop.
type wakeHost struct {
	mu     sync.Mutex
	can    bool
	why    string
	runs   []TerminalReason
	idle   []IdleEvent
	wg     sync.WaitGroup
	loop   *Loop
	refuse bool
}

func (h *wakeHost) hooks() BackgroundHooks {
	return BackgroundHooks{
		CanWake: func() (bool, string) { return h.can, h.why },
		Wake: func(ids []string) bool {
			if h.refuse {
				return false
			}
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				reason, _ := h.loop.RunWoken(context.Background(), Wake{By: "policy", TaskIDs: ids})
				h.mu.Lock()
				h.runs = append(h.runs, reason)
				h.mu.Unlock()
			}()
			return true
		},
		Idle: func(ev IdleEvent) {
			h.mu.Lock()
			h.idle = append(h.idle, ev)
			h.mu.Unlock()
		},
	}
}

func hostFor(r *bgRig, can bool) *wakeHost {
	h := &wakeHost{can: can, why: "host_busy", loop: r.l}
	r.l.Background.SetHooks(h.hooks())
	return h
}

func typesOf(evs []Event, keep ...EventType) []EventType {
	var out []EventType
	for _, e := range evs {
		for _, k := range keep {
			if e.Type == k {
				out = append(out, e.Type)
			}
		}
	}
	return out
}

// In notify a child outlives the run that started it, even when the run's
// context is cancelled as its goroutine returns; its result is recorded and
// appended while idle, after the run's end, with no model call, and the
// closing end follows once nothing is left.
func TestChildOutlivesRunAndIdleNotify(t *testing.T) {
	r := newBGRig(t, WakeNotify, "one")
	h := hostFor(r, false)
	ctx, cancel := context.WithCancel(context.Background())
	reason, err := r.l.Run(ctx, "go")
	cancel() // as a server's run goroutine does when it returns
	if err != nil || reason != TermCompleted {
		t.Fatalf("run: %s %v", reason, err)
	}
	end, _ := LastEnd(r.events(t))
	if end.Background != 1 || end.Settled {
		t.Fatalf("the run's end: %+v", end)
	}
	waitFor(t, "the child's call", func() bool { return r.m.childrenInCall() == 1 })
	calls := r.m.calls.Load()
	time.Sleep(100 * time.Millisecond) // well past the settle window the run's end opened
	r.m.release("one")
	waitFor(t, "the closing end", func() bool { e, _ := LastEnd(r.events(t)); return e.Settled })
	if r.m.calls.Load() != calls { // the child's own call was already under way
		t.Fatalf("model calls after the run: %d", r.m.calls.Load()-calls)
	}
	got := typesOf(r.events(t), EvSessionEnded, EvSubagentReturn, EvSubagentNotice)
	want := []EventType{EvSessionEnded, EvSubagentReturn, EvSubagentNotice, EvSessionEnded}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order: %v", got)
	}
	n := payloads[Notice](r.events(t), EvSubagentNotice)
	if n[0].Delivery != "idle" || n[0].Wake != "notify" {
		t.Fatalf("notice: %+v", n[0])
	}
	msgs := r.l.Messages()
	if last := msgs[len(msgs)-1]; last.ToolCallID != n[0].CallID || !strings.Contains(last.Content, "result of one") {
		t.Fatalf("the conversation does not end with the result: %+v", last)
	}
	if r.l.Background.Unacted() != 1 {
		t.Fatalf("unacted: %d", r.l.Background.Unacted())
	}
	h.mu.Lock()
	if len(h.idle) == 0 || len(h.idle[0].Notices) != 1 {
		t.Fatalf("the surface was not told: %+v", h.idle)
	}
	h.mu.Unlock()
	messagesEqualFork(t, r)
	// The next run sees it, and the count resets.
	if _, err := r.l.Run(context.Background(), "and?"); err != nil {
		t.Fatal(err)
	}
	if r.l.Background.Unacted() != 0 {
		t.Fatal("a run did not take the unacted results")
	}
}

// In auto an idle result starts a wake run: session.woken is recorded before
// the notice and the model call, and the turns and budget move.
func TestAutoWakeRecordsWokenThenRuns(t *testing.T) {
	r := newBGRig(t, WakeAuto, "one")
	r.l.Background.policy.MaxWakesPerHour = 4
	h := hostFor(r, true)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	turns, spent := r.l.turns, r.l.Budget.Spent()
	r.m.release("one")
	waitFor(t, "the wake run", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.runs) == 1 })
	h.wg.Wait()
	evs := r.events(t)
	var order []EventType
	for _, e := range evs {
		switch e.Type {
		case EvSessionWoken, EvSubagentNotice, EvModelCall:
			order = append(order, e.Type)
		}
	}
	tail := order[len(order)-3:]
	if !reflect.DeepEqual(tail, []EventType{EvSessionWoken, EvSubagentNotice, EvModelCall}) {
		t.Fatalf("order: %v", order)
	}
	n := payloads[Notice](evs, EvSubagentNotice)
	w := payloads[SessionWoken](evs, EvSessionWoken)
	if n[0].Delivery != "wake" || n[0].Wake != "auto" || w[0].By != "policy" || w[0].WakesLastHour != 1 || len(w[0].TaskIDs) != 1 {
		t.Fatalf("notice %+v woken %+v", n[0], w[0])
	}
	if r.l.turns != turns+1 || r.l.Budget.Spent() <= spent || len(r.m.saw) != 1 {
		t.Fatalf("turns %d→%d, spent %d→%d, saw %v", turns, r.l.turns, spent, r.l.Budget.Spent(), r.m.saw)
	}
	waitFor(t, "the closing end", func() bool { e, _ := LastEnd(r.events(t)); return e.Background == 0 })
	messagesEqualFork(t, r)
}

// When a wake may not start, the result is recorded as skipped with the
// reason and handled as notify: no model call.
func TestWakeSkipped(t *testing.T) {
	for _, c := range []struct {
		name, why string
		set       func(r *bgRig, h *wakeHost)
	}{
		{"limit", "wake_limit", func(r *bgRig, _ *wakeHost) { r.l.Background.policy.MaxWakesPerHour = 0 }},
		{"budget", "budget", func(r *bgRig, _ *wakeHost) { r.l.Budget.Spend(2_000_000) }},
		{"max turns", "max_turns", func(r *bgRig, _ *wakeHost) { r.l.Config.MaxTurns = r.l.turns }},
		{"host", "host_busy", func(_ *bgRig, h *wakeHost) { h.can = false }},
		{"host refused", "host", func(_ *bgRig, h *wakeHost) { h.refuse = true }},
		{"after interrupt", "last_run_user_interrupt", func(r *bgRig, _ *wakeHost) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _ = r.l.Run(ctx, "stop")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newBGRig(t, WakeAuto, "one")
			r.l.Background.policy.MaxWakesPerHour = 4
			h := hostFor(r, true)
			if _, err := r.l.Run(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the child's call", func() bool { return r.m.childrenInCall() == 1 })
			c.set(r, h)
			calls := r.m.calls.Load()
			r.m.release("one")
			waitFor(t, "the notice", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 1 })
			n := payloads[Notice](r.events(t), EvSubagentNotice)[0]
			if n.Wake != "skipped:"+c.why || n.Delivery != "idle" {
				t.Fatalf("notice: %+v", n)
			}
			time.Sleep(100 * time.Millisecond)
			if r.m.calls.Load() != calls || len(payloads[SessionWoken](r.events(t), EvSessionWoken)) != 0 {
				t.Fatalf("a skipped wake ran the model: %d calls", r.m.calls.Load()-calls)
			}
		})
	}
}

// Results arriving within the settle window share one wake run.
func TestWakeCoalescesWithinSettle(t *testing.T) {
	r := newBGRig(t, WakeAuto, "a", "b")
	r.l.Background.policy.MaxWakesPerHour = 4
	r.l.Background.policy.Settle = 300 * time.Millisecond
	h := hostFor(r, true)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.m.release("a")
	time.Sleep(50 * time.Millisecond)
	r.m.release("b")
	waitFor(t, "the wake run", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.runs) == 1 })
	h.wg.Wait()
	w := payloads[SessionWoken](r.events(t), EvSessionWoken)
	if len(w) != 1 || len(w[0].TaskIDs) != 2 {
		t.Fatalf("woken: %+v", w)
	}
	messagesEqualFork(t, r)
}

// A wake run stops at its turn cap as wake_limit, and the session's other
// children keep running.
func TestWakeRunEndsAtWakeLimitKeepsChildren(t *testing.T) {
	r := newBGRig(t, WakeAuto, "a", "b")
	r.l.Background.policy.MaxWakesPerHour = 4
	r.l.Background.policy.WakeMaxTurns = 1
	r.m.workOnNotice = true
	h := hostFor(r, true)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.m.release("a")
	waitFor(t, "the wake run", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.runs) == 1 })
	h.wg.Wait()
	if h.runs[0] != TermWakeLimit || r.l.Background.Live() != 1 {
		t.Fatalf("wake run ended %s with %d live", h.runs[0], r.l.Background.Live())
	}
	r.m.release("b")
}

// An explicit stop cancels every child, during a run and while idle; send
// now ends only the run, and the children carry on.
func TestStopCancelsAllChildrenSendNowKeepsThem(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a", "b")
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// Send now: the run's context is cancelled; the children are not.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if reason, _ := r.l.Run(ctx, "redirect"); reason != TermUserInterrupt || r.l.Background.Live() != 2 {
		t.Fatalf("send now: %s with %d live", reason, r.l.Background.Live())
	}
	if n := r.l.Background.CancelAll(TermUserInterrupt); n != 2 || r.l.Background.Live() != 0 {
		t.Fatalf("stop cancelled %d, %d live", n, r.l.Background.Live())
	}
	for _, ret := range payloads[map[string]any](r.events(t), EvSubagentReturn) {
		if ret["reason"] != string(TermUserInterrupt) {
			t.Fatalf("returned: %v", ret)
		}
	}
	waitFor(t, "the notices", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 2 })
	for _, n := range payloads[Notice](r.events(t), EvSubagentNotice) {
		if n.Status != "cancelled" {
			t.Fatalf("notice: %+v", n)
		}
	}
}

// Exactly one closing end, and only when no run, notice or wake remains.
func TestSettledEndRecordedOnce(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a", "b")
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.m.release("a")
	waitFor(t, "a's notice", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 1 })
	if e, _ := LastEnd(r.events(t)); e.Settled {
		t.Fatal("settled while b still runs")
	}
	r.m.release("b")
	waitFor(t, "the closing end", func() bool { e, _ := LastEnd(r.events(t)); return e.Settled })
	r.l.Background.kick()
	time.Sleep(100 * time.Millisecond)
	settled := 0
	for _, e := range payloads[SessionEnded](r.events(t), EvSessionEnded) {
		if e.Settled {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("%d closing ends", settled)
	}
}

// A result that arrives after a run last looked, before it lets go of the
// conversation, is delivered while idle rather than waiting for a message.
func TestNoticeArrivingAtRunEndIsDelivered(t *testing.T) {
	r := newBGRig(t, WakeNotify)
	r.l.beforeUnlock = func() {
		r.l.beforeUnlock = nil
		b := r.l.Background
		b.mu.Lock() // arrives with no kick of its own, as one whose kick found the run live
		b.notices = append(b.notices, Notice{TaskID: "late", Session: "late", CallID: "bgn_late", Content: "late result"})
		b.mu.Unlock()
	}
	if _, err := r.l.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the late notice", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 1 })
	messagesEqualFork(t, r)
}

// Fork rebuilds exactly the live conversation after boundary, idle and wake
// notices, delivered under load.
func TestForkEqualsMessagesWithIdleAndWakeNotices(t *testing.T) {
	r := newBGRig(t, WakeAuto, "a", "b", "c")
	r.l.Background.policy.MaxWakesPerHour = 1
	h := hostFor(r, true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.l.Run(context.Background(), "go")
		r.m.release("a") // idle: first wake
	}()
	<-done
	waitFor(t, "the wake", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.runs) == 1 })
	h.wg.Wait()
	r.m.release("b") // idle: over the hourly limit, so notify
	waitFor(t, "b's notice", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 2 })
	// c lands in the middle of a slow turn: its idle delivery must wait for
	// the run, which takes it at its boundary instead.
	go func() { time.Sleep(30 * time.Millisecond); r.m.release("c") }()
	if _, err := r.l.Run(context.Background(), "slow"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "c's notice", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 3 })
	messagesEqualFork(t, r)
}

// Close cancels the children as the session's end says, and records the
// closing end the last run owed.
func TestCloseCancelsAndSettles(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a")
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.l.Background.Close(TermSessionClosed)
	if r.l.Background.Live() != 0 {
		t.Fatal("a child outlived Close")
	}
	ret := payloads[map[string]any](r.events(t), EvSubagentReturn)
	if len(ret) != 1 || ret[0]["reason"] != string(TermSessionClosed) {
		t.Fatalf("returned: %v", ret)
	}
	if e, _ := LastEnd(r.events(t)); !e.Settled {
		t.Fatalf("no closing end: %+v", e)
	}
	if _, err := r.f.SpawnBackground(r.l.asParent(context.Background()), SubagentRequest{Prompt: "x", Description: "x"}); err == nil {
		t.Fatal("a child started after Close")
	}
}
