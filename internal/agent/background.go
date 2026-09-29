package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

// Background subagents belong to a session, not to the run that started
// them. A child started in the background runs on the manager's context, so
// the tool call that started it returns at once and its end does not stop
// the child. When a child ends, its result becomes a notice: recorded first,
// then put in the conversation as a task_status call and its result, the
// channel a tool's output comes through and never the person's.
//
// Where nobody can come back to the conversation (-p, eval, unattended
// sessions), the wake mode is off: children are joined, and the run that
// started them waits for them before it ends.

// WakeMode says what a child's result does when it arrives while the
// session is idle.
type WakeMode string

const (
	// WakeOff joins children: the run that started one waits for it.
	WakeOff WakeMode = "off"
	// WakeNotify records the result and shows it; the model acts on it when
	// the person next sends a message.
	WakeNotify WakeMode = "notify"
	// WakeAuto records the result and starts a wake run, within limits.
	WakeAuto WakeMode = "auto"
)

var wakeRank = map[WakeMode]int{WakeOff: 0, WakeNotify: 1, WakeAuto: 2}

// ParseWakeMode reads a configured wake mode; empty is notify.
func ParseWakeMode(s string) (WakeMode, error) {
	switch m := WakeMode(s); m {
	case "":
		return WakeNotify, nil
	case WakeOff, WakeNotify, WakeAuto:
		return m, nil
	}
	return "", fmt.Errorf("wake mode %q is not off, notify or auto", s)
}

// Tighter returns the narrower of two wake modes.
func (m WakeMode) Tighter(o WakeMode) WakeMode {
	if wakeRank[o] < wakeRank[m] {
		return o
	}
	return m
}

// Terminal reasons that end a child from outside it. They are recorded on
// the child's own session.ended and on its parent's subagent.returned.
const (
	TermCancelledByParent TerminalReason = "cancelled_by_parent"
	TermSessionDeleted    TerminalReason = "session_deleted"
	TermSessionClosed     TerminalReason = "session_closed"
	TermOwnerInactive     TerminalReason = "owner_inactive"
	TermLost              TerminalReason = "lost"
)

// StopCause is the cause a context is cancelled with to end what runs on it
// with a stated reason.
type StopCause struct{ Reason TerminalReason }

func (c StopCause) Error() string { return "stopped: " + string(c.Reason) }

// ErrBackgroundLifetime is the cause a background child's deadline carries.
var ErrBackgroundLifetime = errors.New("the background task's lifetime ran out")

// BackgroundPolicy is a session's limits on background children.
type BackgroundPolicy struct {
	// Wake is the session's effective wake mode.
	Wake WakeMode
	// MaxLive bounds the children alive at once, across runs. Zero is 4.
	MaxLive int
	// Lifetime bounds each child's wall-clock life. Zero is 60 minutes.
	Lifetime time.Duration
	// MaxWakesPerHour bounds automatic wake runs in a rolling hour.
	MaxWakesPerHour int
	// WakeMaxTurns caps one wake run. Zero is 8.
	WakeMaxTurns int
	// Settle is how long an idle session waits for more results before it
	// delivers them, so results arriving together are delivered together.
	// Zero is two seconds.
	Settle time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (p BackgroundPolicy) maxLive() int {
	if p.MaxLive <= 0 {
		return 4
	}
	return p.MaxLive
}

func (p BackgroundPolicy) lifetime() time.Duration {
	if p.Lifetime <= 0 {
		return 60 * time.Minute
	}
	return p.Lifetime
}

func (p BackgroundPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// TurnEndWait bounds how long cancelling children waits for each to record
// its end.
var TurnEndWait = 5 * time.Second

// Background is one session's background children and the notices their
// ends leave. It is created once per session and outlives every run.
type Background struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	loop   *Loop
	policy BackgroundPolicy

	mu       sync.Mutex
	tasks    map[string]*bgTask
	order    []string
	reserved int
	notices  []Notice
	closed   bool
	signal   chan struct{}
	hooks    BackgroundHooks

	// unacted counts notices delivered while idle that no run has seen.
	unacted int
	// wakes are when automatic wake runs started, for the hourly limit.
	wakes []time.Time
	// waking is set while a wake run is being started for pending notices.
	waking bool
	// armed is set while an idle delivery is scheduled.
	armed bool
	// lastReason is how the last run ended; lastEnd is what it recorded, and
	// owed says a closing end is due once background work is over.
	lastReason TerminalReason
	lastEnd    SessionEnded
	owed       bool
}

// BackgroundHooks are how a surface takes part in idle delivery.
type BackgroundHooks struct {
	// CanWake says whether this surface can host a wake run now, and why not.
	CanWake func() (bool, string)
	// Wake starts a wake run for the notices of taskIDs, on its own
	// goroutine; it reports whether one was started.
	Wake func(taskIDs []string) bool
	// Idle is told of notices delivered while no run was live.
	Idle func(IdleEvent)
}

// IdleEvent is what an idle delivery did.
type IdleEvent struct {
	Notices []Notice
	// Settled is set when background work finished and the closing end was recorded.
	Settled bool
}

// ErrNothingToWake refuses a wake with no result waiting to be acted on.
var ErrNothingToWake = errors.New("nothing to wake for: no background result is waiting")

// SetHooks connects a surface.
func (b *Background) SetHooks(h BackgroundHooks) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.hooks = h
	b.mu.Unlock()
}

// SetMode changes the wake mode for what comes next. Children already
// running keep the mode they started under: joining them now would block a
// run that is not live.
func (b *Background) SetMode(m WakeMode) {
	b.mu.Lock()
	b.policy.Wake = m
	b.mu.Unlock()
}

// Unacted is how many results were delivered while idle and not yet seen by a run.
func (b *Background) Unacted() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.unacted
}

// bgTask is one background child.
type bgTask struct {
	ID          string
	Description string
	AgentType   string
	Provider    string
	Model       string
	Started     time.Time
	// joined children are waited for by the run that started them.
	joined  bool
	cancel  context.CancelCauseFunc
	done    chan struct{}
	ended   bool
	reason  TerminalReason
	summary string
	turns   int
}

// Notice is a background child's result as the conversation receives it.
type Notice struct {
	TaskID      string `json:"task_id"`
	Session     string `json:"session"`
	Description string `json:"description,omitempty"`
	// Status is completed, failed or cancelled, or the reason a cap ended it.
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Turns     int    `json:"turns"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	// CallID is the synthetic task_status call the result answers.
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	// Delivery is boundary, idle or wake: when the conversation took it.
	Delivery string `json:"delivery,omitempty"`
	// Wake is what an idle delivery did: notify, auto or skipped:<reason>.
	Wake string `json:"wake,omitempty"`
}

// NewBackground makes loop's manager, on a context of its own that only
// Close ends.
func NewBackground(loop *Loop, policy BackgroundPolicy) *Background {
	ctx, cancel := context.WithCancelCause(context.Background())
	b := &Background{ctx: ctx, cancel: cancel, loop: loop, policy: policy,
		tasks: map[string]*bgTask{}, signal: make(chan struct{}, 1)}
	if loop != nil {
		loop.Background = b
	}
	return b
}

// Mode is the wake mode new children are started under.
func (b *Background) Mode() WakeMode {
	if b == nil {
		return WakeOff
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.policy.Wake
}

// poke wakes a run waiting for its joined children.
func (b *Background) poke() {
	if b == nil {
		return
	}
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

// Live counts the children still running.
func (b *Background) Live() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.liveLocked()
}

func (b *Background) liveLocked() int {
	n := 0
	for _, t := range b.tasks {
		if !t.ended {
			n++
		}
	}
	return n
}

func (b *Background) joinedLive() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, t := range b.tasks {
		if !t.ended && t.joined {
			n++
		}
	}
	return n
}

// Free is how many more children may start now.
func (b *Background) Free() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.policy.maxLive() - b.liveLocked() - b.reserved
}

// reserve holds one of the live slots for a spawn being prepared.
func (b *Background) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("the session is closing; no background task can start")
	}
	if n, most := b.liveLocked()+b.reserved, b.policy.maxLive(); n >= most {
		return fmt.Errorf("background task limit reached (%d of %d running). Wait for one to finish, or run this one in the foreground", n, most)
	}
	b.reserved++
	return nil
}

func (b *Background) unreserve() {
	b.mu.Lock()
	b.reserved--
	b.mu.Unlock()
}

// Tasks lists this session's background children, oldest first.
func (b *Background) Tasks() []TaskInfo {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]TaskInfo, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, b.tasks[id].info())
	}
	return out
}

// TaskInfo is what task_status and the surfaces say about one child.
type TaskInfo struct {
	ID          string    `json:"task_id"`
	Description string    `json:"description"`
	AgentType   string    `json:"agent_type,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	Turns       int       `json:"turns,omitempty"`
	Started     time.Time `json:"started"`
	Summary     string    `json:"summary,omitempty"`
}

func (t *bgTask) info() TaskInfo {
	st := "running"
	if t.ended {
		st = noticeStatus(t.reason)
	}
	return TaskInfo{ID: t.ID, Description: t.Description, AgentType: t.AgentType,
		Provider: t.Provider, Model: t.Model, Status: st, Reason: string(t.reason),
		Turns: t.turns, Started: t.Started, Summary: t.summary}
}

// Task reports one child, and whether it is one of this session's.
func (b *Background) Task(id string) (TaskInfo, bool) {
	if b == nil {
		return TaskInfo{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	if !ok {
		return TaskInfo{}, false
	}
	return t.info(), true
}

// noticeStatus is the one-word outcome a notice names.
func noticeStatus(r TerminalReason) string {
	switch r {
	case TermCompleted:
		return "completed"
	case TermError, TermRetryExhausted:
		return "failed"
	case TermUserInterrupt, TermCancelledByParent, TermSessionDeleted, TermSessionClosed,
		TermOwnerInactive, TermShutdown, TermLost:
		return "cancelled"
	}
	return string(r)
}

// Cancel stops one child with reason; false when there is no such running child.
func (b *Background) Cancel(id string, reason TerminalReason) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	t, ok := b.tasks[id]
	running := ok && !t.ended
	b.mu.Unlock()
	if !running {
		return false
	}
	t.cancel(StopCause{reason})
	waitDone([]*bgTask{t})
	return true
}

// CancelAll stops every running child with reason and waits a bounded time
// for each to record its end. Later spawns still work.
func (b *Background) CancelAll(reason TerminalReason) int {
	return b.cancelWhere(reason, func(*bgTask) bool { return true })
}

func (b *Background) cancelWhere(reason TerminalReason, which func(*bgTask) bool) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	var hit []*bgTask
	for _, t := range b.tasks {
		if !t.ended && which(t) {
			hit = append(hit, t)
		}
	}
	b.mu.Unlock()
	for _, t := range hit {
		t.cancel(StopCause{reason})
	}
	waitDone(hit)
	return len(hit)
}

func waitDone(ts []*bgTask) {
	deadline := time.NewTimer(TurnEndWait)
	defer deadline.Stop()
	for _, t := range ts {
		select {
		case <-t.done:
		case <-deadline.C:
			return
		}
	}
}

// Close ends the session's background work: no child starts after it, every
// running one is cancelled with reason, and it waits a bounded time for them.
// A closing end owed by the last run is recorded; the results not yet
// delivered stay in the record, where PendingNotices finds them.
func (b *Background) Close(reason TerminalReason) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	var hit []*bgTask
	for _, t := range b.tasks {
		if !t.ended {
			hit = append(hit, t)
		}
	}
	b.mu.Unlock()
	b.cancel(StopCause{reason})
	waitDone(hit)
	if b.loop == nil {
		return
	}
	b.loop.runMu.Lock()
	b.mu.Lock()
	b.notices, b.waking = nil, false
	b.mu.Unlock()
	b.settleIfDue()
	b.loop.runMu.Unlock()
}

// push takes a finished child's notice for the next delivery: at a run's
// boundary if one is live, or while idle after the settle window.
func (b *Background) push(n Notice) {
	b.mu.Lock()
	b.notices = append(b.notices, n)
	b.mu.Unlock()
	b.poke()
	b.kick()
}

func (b *Background) settle() time.Duration {
	if b.policy.Settle <= 0 {
		return 2 * time.Second
	}
	return b.policy.Settle
}

// kick schedules an idle delivery, once, after the settle window, so results
// arriving together are delivered together.
func (b *Background) kick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.armed || b.closed {
		return
	}
	b.armed = true
	time.AfterFunc(b.settle(), b.deliverIdle)
}

// deliverIdle delivers what is pending while no run is live. It waits for a
// run that is live to end: that run takes what arrives at its boundaries,
// and whatever arrived after its last look is delivered here.
func (b *Background) deliverIdle() {
	b.mu.Lock()
	b.armed = false
	closed := b.closed
	b.mu.Unlock()
	l := b.loop
	if closed || l == nil {
		return
	}
	l.runMu.Lock()
	b.mu.Lock()
	pending, waking := len(b.notices), b.waking
	var ids []string
	for _, n := range b.notices {
		ids = append(ids, n.TaskID)
	}
	b.mu.Unlock()
	if pending == 0 || waking {
		settled := b.settleIfDue()
		l.runMu.Unlock()
		if settled {
			b.idle(IdleEvent{Settled: true})
		}
		return
	}
	wake := "notify"
	if b.Mode() == WakeAuto {
		if ok, why := b.canWake(); !ok {
			wake = "skipped:" + why
		} else {
			b.mu.Lock()
			b.waking = true
			start := b.hooks.Wake
			b.mu.Unlock()
			l.runMu.Unlock()
			if start != nil && start(ids) {
				return
			}
			// The surface could not start it after all: deliver as notify.
			b.mu.Lock()
			b.waking = false
			b.mu.Unlock()
			l.runMu.Lock()
			wake = "skipped:host"
		}
	}
	before := b.Pending()
	delivered := b.peekNotices()
	if err := l.deliverNotices("idle", wake); err != nil {
		// The next run ends at once; the record still has each return.
		l.noteRecordErr(err)
		l.runMu.Unlock()
		return
	}
	b.mu.Lock()
	b.unacted += before
	b.mu.Unlock()
	settled := b.settleIfDue()
	l.runMu.Unlock()
	for i := range delivered {
		delivered[i].Delivery, delivered[i].Wake = "idle", wake
	}
	b.idle(IdleEvent{Notices: delivered, Settled: settled})
}

func (b *Background) peekNotices() []Notice {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Notice(nil), b.notices...)
}

func (b *Background) idle(ev IdleEvent) {
	b.mu.Lock()
	h := b.hooks.Idle
	b.mu.Unlock()
	if h != nil {
		h(ev)
	}
}

// canWake says whether a wake run may start now, and names why not. The
// caller holds runMu, so no run is live.
func (b *Background) canWake() (bool, string) {
	l := b.loop
	b.mu.Lock()
	last, can := b.lastReason, b.hooks.CanWake
	most := b.policy.MaxWakesPerHour
	recent := b.recentWakesLocked()
	b.mu.Unlock()
	switch {
	case last != TermCompleted && last != TermWakeLimit:
		return false, "last_run_" + orNone(string(last))
	case l.Budget.Exhausted():
		return false, "budget"
	case l.turns >= l.Config.MaxTurns:
		return false, "max_turns"
	case recent >= most:
		return false, "wake_limit"
	case can == nil:
		return false, "host"
	}
	return can()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (b *Background) recentWakesLocked() int {
	cut := b.policy.now().Add(-time.Hour)
	n := 0
	for _, w := range b.wakes {
		if w.After(cut) {
			n++
		}
	}
	return n
}

// noteEnd keeps how a run ended, for the wake rules and the closing end.
// The caller holds runMu.
func (b *Background) noteEnd(end SessionEnded) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.lastReason, b.lastEnd, b.owed = end.Reason, end, end.Background > 0
	b.mu.Unlock()
}

// settleIfDue records the closing end once the last run's background work
// is over: no child running, no notice waiting, no wake starting. The
// caller holds runMu.
func (b *Background) settleIfDue() bool {
	b.mu.Lock()
	due := b.owed && b.liveLocked() == 0 && len(b.notices) == 0 && !b.waking
	end := b.lastEnd
	if due {
		b.owed = false
	}
	b.mu.Unlock()
	if !due {
		return false
	}
	end.Background, end.Settled = 0, true
	b.loop.record(EvSessionEnded, ActorSystem, end)
	return true
}

// Wake is what started a wake run.
type Wake struct {
	// By is policy, for the session's own wake mode, or caller, for an
	// explicit wake by the surface.
	By      string
	TaskIDs []string
}

// WakeSet is the payload of session.wake_set.
type WakeSet struct {
	Wake    WakeMode `json:"wake"`
	By      string   `json:"by"`
	Ceiling WakeMode `json:"ceiling,omitempty"`
}

// SessionWoken is the payload of session.woken.
type SessionWoken struct {
	By            string   `json:"by"`
	WakeMode      WakeMode `json:"wake_mode"`
	TaskIDs       []string `json:"task_ids,omitempty"`
	WakesLastHour int      `json:"wakes_last_hour"`
}

// RunWoken runs a turn no person prompted, for background results: it
// records session.woken, delivers what is pending at its first boundary and
// stops at the wake's turn cap as wake_limit. It has no authority a prompted
// run lacks: the same policy, approver and scopes.
func (l *Loop) RunWoken(ctx context.Context, w Wake) (TerminalReason, error) {
	l.runMu.Lock()
	defer l.unlockRun()
	b := l.Background
	if b == nil {
		return "", ErrNothingToWake
	}
	b.mu.Lock()
	b.waking = false
	if len(b.notices) == 0 && b.unacted == 0 {
		b.mu.Unlock()
		return "", ErrNothingToWake
	}
	if w.By == "policy" {
		b.wakes = append(b.wakes, b.policy.now())
	}
	woken := SessionWoken{By: w.By, WakeMode: b.policy.Wake, TaskIDs: w.TaskIDs, WakesLastHour: b.recentWakesLocked()}
	most := b.policy.WakeMaxTurns
	b.mu.Unlock()
	if _, err := l.Recorder.Record(EvSessionWoken, ActorSystem, Trusted, woken); err != nil {
		return TermError, err
	}
	if most <= 0 {
		most = 8
	}
	l.wakeCap = min(l.Config.MaxTurns, l.turns+most)
	l.wakeDelivery = "auto"
	if w.By != "policy" {
		l.wakeDelivery = "caller"
	}
	defer func() { l.wakeCap, l.wakeDelivery = 0, "" }()
	return l.run(ctx)
}

// Pending is how many notices wait for delivery.
func (b *Background) Pending() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.notices)
}

func (b *Background) takeNotices() []Notice {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.notices
	b.notices = nil
	return out
}

// requeue puts notices that could not be delivered back at the front.
func (b *Background) requeue(ns []Notice) {
	b.mu.Lock()
	b.notices = append(append([]Notice(nil), ns...), b.notices...)
	b.mu.Unlock()
}

// QueueNotices adds notices rebuilt from the record, such as a result that
// arrived before a restart, for the next delivery.
func (l *Loop) QueueNotices(ns []Notice) {
	if len(ns) == 0 {
		return
	}
	if l.Background == nil {
		NewBackground(l, BackgroundPolicy{Wake: WakeOff})
	}
	for _, n := range ns {
		l.Background.push(n)
	}
}

// SpawnBackground starts a child on the session's background manager and
// returns its task id, which is the child's session id. The spawn is
// settled and recorded before it returns; the child runs on.
func (f *SubagentFactory) SpawnBackground(ctx context.Context, req SubagentRequest) (string, error) {
	parent, _ := ctx.Value(parentKey{}).(*parentLink)
	if parent == nil || parent.loop == nil || parent.loop.Background == nil {
		return "", errors.New("background tasks run only from a session's own task call; run it in the foreground")
	}
	if parent.depth > 0 {
		return "", errors.New("a subagent cannot start a background task; do the work directly")
	}
	mgr := parent.loop.Background
	id := newID()
	req.sessionID = id
	joined := mgr.Mode() == WakeOff
	extra := map[string]any{"background": true, "task_id": id}
	held := false
	c, err := f.prepare(ctx, req, extra, func() error {
		if err := mgr.reserve(); err != nil {
			return err
		}
		held = true
		return nil
	})
	if held {
		defer mgr.unreserve()
	}
	if err != nil {
		return "", err
	}

	// The child's context comes from the session, not the call: the call's
	// end, and its call and request ids, do not reach the child.
	cctx, cancel := context.WithCancelCause(mgr.ctx)
	cctx, stopDeadline := context.WithDeadlineCause(cctx, mgr.policy.now().Add(mgr.policy.lifetime()), ErrBackgroundLifetime)
	cctx = context.WithValue(cctx, parentKey{}, parent)
	t := &bgTask{ID: id, Description: req.Description, AgentType: req.AgentType, Provider: c.provider,
		Model: c.adapter.Profile().Name, Started: mgr.policy.now(), joined: joined,
		cancel: cancel, done: make(chan struct{})}
	mgr.mu.Lock()
	mgr.tasks[id] = t
	mgr.order = append(mgr.order, id)
	mgr.mu.Unlock()

	go func() {
		defer close(t.done)
		defer stopDeadline()
		summary, reason, err := c.execute(cctx)
		if err != nil {
			summary = err.Error()
		}
		if req.settle != nil {
			sctx, done := context.WithTimeout(context.WithoutCancel(cctx), TurnEndWait)
			summary = strings.TrimSpace(summary) + "\n\n" + req.settle(sctx)
			done()
		}
		usage := c.sub.Usage()
		n := Notice{TaskID: id, Session: id, Description: req.Description, Status: noticeStatus(reason),
			Reason: string(reason), Turns: usage.Turns, TokensIn: usage.InputTokens, TokensOut: usage.OutputTokens,
			Provider: c.provider, Model: c.adapter.Profile().Name, CallID: "bgn_" + newID(),
			Content: parentRedacted(parent, summary)}
		mgr.mu.Lock()
		t.ended, t.reason, t.summary, t.turns = true, reason, n.Content, usage.Turns
		mgr.mu.Unlock()
		cancel(nil)
		mgr.push(n)
	}()
	return id, nil
}

// parentRedacted redacts text as the parent's record would.
func parentRedacted(p *parentLink, text string) string {
	if p != nil && p.rec != nil {
		if red := p.rec.redactor(); red != nil {
			return redactedText(red.Redact, text)
		}
	}
	return text
}

// noticeArgs is the synthetic task_status call's arguments, the same bytes in
// the live conversation and in one rebuilt from the record.
func noticeArgs(taskID string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"task_id": taskID})
	return b
}

// noticeMessages is the assistant call and the tool result a notice becomes.
func noticeMessages(n Notice) []model.Message {
	return []model.Message{
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: n.CallID, Name: "task_status", Args: noticeArgs(n.TaskID)}}},
		{Role: model.RoleTool, ToolCallID: n.CallID, Content: n.Content},
	}
}

// deliverNotices records each pending notice and then puts it in the
// conversation. A notice the record refused is not applied, and it and the
// ones after it stay pending. The caller holds runMu.
func (l *Loop) deliverNotices(delivery, wake string) error {
	if l.Background == nil {
		return nil
	}
	ns := l.Background.takeNotices()
	for i, n := range ns {
		n.Delivery, n.Wake = delivery, wake
		// Untrusted: the content is a subagent's summary, shaped by the files it read.
		if _, err := l.Recorder.Record(EvSubagentNotice, ActorSystem, Untrusted, n); err != nil {
			l.Background.requeue(ns[i:])
			return err
		}
		l.messages = append(l.messages, noticeMessages(n)...)
	}
	return nil
}

// hasWork reports whether a completed run has more to do: a queued message,
// or a notice not yet delivered.
func (l *Loop) hasWork() bool {
	return len(l.Queued()) > 0 || l.Background.Pending() > 0
}

// waitBackground holds a completed run while joined children still run. It
// reports whether there is work for another turn; false when the context
// ended or no joined child is left and nothing arrived.
func (l *Loop) waitBackground(ctx context.Context) bool {
	for {
		if l.hasWork() {
			return true
		}
		if l.Background.joinedLive() == 0 {
			return false
		}
		select {
		case <-l.Background.signal:
		case <-ctx.Done():
			return false
		}
	}
}

// onRunEnd stops the children a run's end takes with it: every child when
// the conversation cannot go on, and the joined ones whenever the run did
// not complete, since nobody waits for them any more.
func (b *Background) onRunEnd(reason TerminalReason) {
	if b == nil {
		return
	}
	switch reason {
	case TermCompleted, TermWakeLimit:
		return
	case TermError, TermMaxTurns, TermMaxBudget, TermShutdown:
		b.CancelAll(reason)
		return
	}
	b.cancelWhere(reason, func(t *bgTask) bool { return t.joined })
}

// PendingNotices rebuilds the notices a record owes its conversation: each
// background child whose return is recorded and whose notice is not. child
// reads a child's own record, for its last answer.
func PendingNotices(events []Event, child func(id string) ([]Event, error)) []Notice {
	events = Live(events)
	delivered := map[string]bool{}
	for _, e := range events {
		if e.Type == EvSubagentNotice {
			var n Notice
			if json.Unmarshal(e.Payload, &n) == nil {
				delivered[n.TaskID] = true
			}
		}
	}
	var out []Notice
	for _, e := range events {
		if e.Type != EvSubagentReturn {
			continue
		}
		var r struct {
			Background  bool   `json:"background"`
			TaskID      string `json:"task_id"`
			Session     string `json:"session"`
			Description string `json:"description"`
			Reason      string `json:"reason"`
			Turns       int    `json:"turns"`
			TokensIn    int    `json:"tokens_in"`
			TokensOut   int    `json:"tokens_out"`
			Provider    string `json:"provider"`
			Model       string `json:"model"`
		}
		if json.Unmarshal(e.Payload, &r) != nil || !r.Background || r.TaskID == "" || delivered[r.TaskID] {
			continue
		}
		delivered[r.TaskID] = true
		content := ""
		if child != nil {
			if evs, err := child(r.Session); err == nil {
				content = lastAgentMessage(evs)
			}
		}
		if strings.TrimSpace(content) == "" {
			content = fmt.Sprintf("(subagent ended with %s and produced no summary)", r.Reason)
		}
		if len(content) > MaxSummaryChars {
			content = content[:MaxSummaryChars] + "\n\n[summary truncated]"
		}
		if r.Reason != string(TermCompleted) {
			content += fmt.Sprintf("\n\n[subagent ended early: %s]", r.Reason)
		}
		out = append(out, Notice{TaskID: r.TaskID, Session: r.Session, Description: r.Description,
			Status: noticeStatus(TerminalReason(r.Reason)), Reason: r.Reason, Turns: r.Turns,
			TokensIn: r.TokensIn, TokensOut: r.TokensOut, Provider: r.Provider, Model: r.Model,
			CallID: "bgn_" + newID(), Content: content})
	}
	return out
}

// lastAgentMessage is the last non-empty answer in a record.
func lastAgentMessage(evs []Event) string {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == EvAgentMessage {
			var m Message
			if json.Unmarshal(evs[i].Payload, &m) == nil && strings.TrimSpace(m.Text) != "" {
				return m.Text
			}
		}
	}
	return ""
}
