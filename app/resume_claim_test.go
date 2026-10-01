package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/store"
)

// rowStore is the memory store with the session rows and the atomic claim a
// durable store has, counting the claims made.
type rowStore struct {
	*agent.MemStore
	mu     sync.Mutex
	rows   map[string]store.SessionRecord
	claims int
}

// Append refuses a seq already taken and ends the row on session.ended, as
// Postgres does.
func (r *rowStore) Append(ev agent.Event) error {
	evs, _ := r.Events(ev.SessionID)
	for _, e := range evs {
		if e.Seq == ev.Seq {
			return fmt.Errorf("seq %d of %s is taken", ev.Seq, ev.SessionID)
		}
	}
	if err := r.MemStore.Append(ev); err != nil {
		return err
	}
	if ev.Type == agent.EvSessionEnded {
		r.mu.Lock()
		if rec, ok := r.rows[ev.SessionID]; ok {
			now := time.Now()
			rec.EndedAt = &now
			r.rows[ev.SessionID] = rec
		}
		r.mu.Unlock()
	}
	return nil
}

func (r *rowStore) GetSession(_ context.Context, id string) (store.SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.rows[id]
	if !ok {
		return rec, store.ErrNotFound
	}
	return rec, nil
}

func (r *rowStore) ClaimResume(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims++
	rec, ok := r.rows[id]
	if !ok || rec.EndedAt == nil {
		return false, nil
	}
	rec.EndedAt = nil
	r.rows[id] = rec
	return true, nil
}

// resumeRig is a CLI session state over a rowStore holding one finished
// session, recorded as id for user and tenant.
func resumeRig(t *testing.T, user, tenant string) (*cliState, *rowStore, *ui.Renderer, *tools.Session) {
	t.Helper()
	t.Setenv("USER", "me")
	rs := &rowStore{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}}
	ended := time.Now()
	rs.rows["s-old"] = store.SessionRecord{ID: "s-old", User: user, Tenant: tenant, EndedAt: &ended}
	msg, _ := json.Marshal(agent.Message{Text: "remember ZEBRA-41"})
	end, _ := json.Marshal(agent.SessionEnded{Reason: agent.TermCompleted, Turns: 3, TokensIn: 42})
	_ = rs.Append(agent.Event{ID: "e1", SessionID: "s-old", Seq: 1, Type: agent.EvUserMessage, Payload: msg})
	reply, _ := json.Marshal(agent.Message{Text: "noted ZEBRA-41"})
	_ = rs.Append(agent.Event{ID: "e2", SessionID: "s-old", Seq: 2, Type: agent.EvAgentMessage, Payload: reply})
	_ = rs.Append(agent.Event{ID: "e3", SessionID: "s-old", Seq: 3, Type: agent.EvSessionEnded, Payload: end})
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return rigState(rs, sess), rs, ui.NewRenderer(io.Discard, true), sess
}

// rigState is a CLI session state over rs, as a new process would start one.
func rigState(rs *rowStore, sess *tools.Session) *cliState {
	st := &cliState{store: rs, appCfg: config.Default(), sess: sess}
	st.fresh()
	st.open = func(id string) *agent.Loop {
		l := agent.NewLoop(nil, nil, policy.New(policy.ModeDefault), agent.AutoApprove{}, sess, agent.NewRecorder(rs, id, ""), agent.DefaultConfig())
		st.loop, st.sessionID = l, id
		return l
	}
	return st
}

// A session recorded for another user, or in another tenant, is neither
// replayed nor continued.
func TestResumeRefusesAnotherOwnersSession(t *testing.T) {
	for _, owner := range [][2]string{{"mallory", "default"}, {"me", "other"}} {
		st, rs, _, sess := resumeRig(t, owner[0], owner[1])
		var shown bytes.Buffer
		handleCommand(context.Background(), "/resume s-old", ui.NewRenderer(&shown, false), policy.New(policy.ModeDefault), sess, st)
		if st.loop != nil || st.claim != "" || rs.claims != 0 {
			t.Errorf("%v: resumed another owner's session", owner)
		}
		if strings.Contains(shown.String(), "ZEBRA-41") {
			t.Errorf("%v: replayed another owner's transcript", owner)
		}
	}
}

// A session running elsewhere is not continued: at /resume when its row says
// so, and at the first task when another process claimed it first.
func TestResumeRefusesASessionRunningElsewhere(t *testing.T) {
	st, rs, r, sess := resumeRig(t, "me", "default")
	rec := rs.rows["s-old"]
	rec.EndedAt = nil
	rs.rows["s-old"] = rec
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	if st.loop != nil {
		t.Fatal("a running session was taken over at /resume")
	}

	st, rs, r, sess = resumeRig(t, "me", "default")
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	if st.loop == nil || st.claim != "s-old" {
		t.Fatal("a finished session of mine was not resumed")
	}
	rec = rs.rows["s-old"]
	rec.EndedAt = nil // another process claims it first
	rs.rows["s-old"] = rec
	if err := claimResumed(context.Background(), st); err == nil || st.loop != nil {
		t.Fatalf("the first task went ahead on a session claimed elsewhere: %v", err)
	}
}

// Nothing is claimed until a task runs, so leaving a resumed conversation
// unused leaves the session as it was; the first task claims it once.
func TestResumeClaimsOnlyWhenATaskRuns(t *testing.T) {
	for _, leave := range []string{"/clear", "/quit", "/resume s-old"} {
		st, rs, r, sess := resumeRig(t, "me", "default")
		handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
		handleCommand(context.Background(), leave, r, policy.New(policy.ModeDefault), sess, st)
		if rs.claims != 0 || rs.rows["s-old"].EndedAt == nil {
			t.Errorf("/resume then %s left a claim", leave)
		}
	}
	st, rs, r, sess := resumeRig(t, "me", "default")
	st.total.InputTokens, st.undo = 500, nil
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	if st.total.InputTokens != 0 || st.undo == nil {
		t.Fatal("/resume kept the last conversation's cost or undo log")
	}
	if err := claimResumed(context.Background(), st); err != nil || rs.claims != 1 || st.loop == nil {
		t.Fatalf("the first task's claim: %v, %d claims", err, rs.claims)
	}
	if err := claimResumed(context.Background(), st); err != nil || rs.claims != 1 {
		t.Fatal("a later task claimed again")
	}
	if got := st.loop.Usage().Turns; got != 3 {
		t.Fatalf("the resumed conversation starts at %d turns, want the recorded 3", got)
	}
}

// /fork writes to the record, so after /resume it claims the session first,
// and writes nothing when another process holds it.
func TestForkAfterResumeClaimsFirst(t *testing.T) {
	st, rs, r, sess := resumeRig(t, "me", "default")
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	handleCommand(context.Background(), "/fork 1", r, policy.New(policy.ModeDefault), sess, st)
	// One claim, released as it was found, and taken again before the next write.
	if rs.claims != 1 || st.claim != "s-old" {
		t.Fatalf("/fork after /resume made %d claims", rs.claims)
	}

	st, rs, r, sess = resumeRig(t, "me", "default")
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	rec := rs.rows["s-old"]
	rec.EndedAt = nil // another process claims it first
	rs.rows["s-old"] = rec
	handleCommand(context.Background(), "/fork 1", r, policy.New(policy.ModeDefault), sess, st)
	events, _ := rs.Events("s-old")
	for _, ev := range events {
		if ev.Type == agent.EvForked {
			t.Fatal("/fork wrote to a session another process holds")
		}
	}
}

// /resume of the conversation already open here keeps its cost and undo log.
func TestResumeOfTheLiveSessionKeepsItsCost(t *testing.T) {
	st, _, r, sess := resumeRig(t, "me", "default")
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	st.total.InputTokens = 500
	undo := st.undo
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	if st.total.InputTokens != 500 || st.undo != undo || st.claim != "s-old" {
		t.Fatalf("resuming the open conversation reset it: %d tokens, claim %q", st.total.InputTokens, st.claim)
	}
}

// /compact writes to the record too, so after /resume it claims first.
func TestCompactAfterResumeClaimsFirst(t *testing.T) {
	st, rs, r, sess := resumeRig(t, "me", "default")
	handleCommand(context.Background(), "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	handleCommand(context.Background(), "/compact", r, policy.New(policy.ModeDefault), sess, st)
	// One claim, released as it was found, and taken again before the next write.
	if rs.claims != 1 || st.claim != "s-old" {
		t.Fatalf("/compact after /resume made %d claims", rs.claims)
	}
}

// Another process continues the session between /resume and the first task
// here: the claim rebuilds from the record as it now stands, and goes on
// after the other process's events rather than over them.
func TestClaimAfterSomeoneElseContinuedRebuilds(t *testing.T) {
	st, rs, r, sess := resumeRig(t, "me", "default")
	ctx := context.Background()
	handleCommand(ctx, "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	if ok, _ := rs.ClaimResume(ctx, "s-old"); !ok {
		t.Fatal("the other process's claim failed")
	}
	msg, _ := json.Marshal(agent.Message{Text: "other task"})
	end, _ := json.Marshal(agent.SessionEnded{Reason: agent.TermCompleted, Turns: 4})
	_ = rs.Append(agent.Event{ID: "o4", SessionID: "s-old", Seq: 4, Type: agent.EvUserMessage, Payload: msg})
	_ = rs.Append(agent.Event{ID: "o5", SessionID: "s-old", Seq: 5, Type: agent.EvSessionEnded, Payload: end})
	rs.mu.Lock()
	rec := rs.rows["s-old"]
	ended := time.Now()
	rec.EndedAt = &ended
	rs.rows["s-old"] = rec
	rs.mu.Unlock()

	if err := claimResumed(ctx, st); err != nil {
		t.Fatal(err)
	}
	ev, err := st.loop.Recorder.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "mine"})
	if err != nil || ev.Seq != 6 {
		t.Fatalf("the continuation wrote seq %d (%v), want 6, after the other process's events", ev.Seq, err)
	}
	seen := false
	for _, m := range st.loop.Messages() {
		seen = seen || m.Content == "other task"
	}
	if !seen || st.loop.Usage().Turns != 4 {
		t.Fatalf("the continuation was not rebuilt from the record as it stands (turns %d)", st.loop.Usage().Turns)
	}
}

// A turn that fails after the claim, with no session.ended of its own, is
// ended in the record through the call every task ends with; one that already
// ended is not ended twice.
func TestFailedTurnAfterClaimEndsTheSession(t *testing.T) {
	st, rs, r, sess := resumeRig(t, "me", "default")
	ctx := context.Background()
	handleCommand(ctx, "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	if err := claimResumed(ctx, st); err != nil {
		t.Fatal(err)
	}
	_, _ = st.loop.Recorder.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "mine"})
	settleTurn(st, errors.New("the turn failed"))
	events, _ := rs.Events("s-old")
	if last := events[len(events)-1]; last.Type != agent.EvSessionEnded {
		t.Fatalf("the failed turn left the record open at %s", last.Type)
	}
	settleTurn(st, errors.New("and again"))
	if again, _ := rs.Events("s-old"); len(again) != len(events) {
		t.Fatal("a record that had ended was ended a second time")
	}
}

// Between tasks the session is released, so another process can claim it and
// run. The next task here is refused rather than written over that run, and
// a failed write never ends the other process's run.
func TestNextTaskDoesNotTouchAnotherProcesssRun(t *testing.T) {
	st, rs, r, sess := resumeRig(t, "me", "default")
	ctx := context.Background()
	handleCommand(ctx, "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	if err := claimResumed(ctx, st); err != nil {
		t.Fatal(err)
	}
	rec := st.loop.Recorder
	_, _ = rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "mine"})
	_, _ = rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted})
	settleTurn(st, nil)

	// Another process claims the released session and starts a run.
	if ok, _ := rs.ClaimResume(ctx, "s-old"); !ok {
		t.Fatal("the other process could not claim a released session")
	}
	other, _ := json.Marshal(agent.Message{Text: "theirs"})
	_ = rs.Append(agent.Event{ID: "o6", SessionID: "s-old", Seq: 6, Type: agent.EvUserMessage, Payload: other})

	if err := claimResumed(ctx, st); err == nil {
		t.Fatal("the next task went ahead on a session another process is running")
	}
	// Even a write that got through collides, and its failure ends nothing.
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "late"}); err == nil {
		t.Fatal("the store took a seq already used")
	}
	st.loop, st.sessionID = agent.NewLoop(nil, nil, nil, nil, sess, rec, agent.DefaultConfig()), "s-old"
	settleTurn(st, errors.New("seq taken"))
	events, _ := rs.Events("s-old")
	if last := events[len(events)-1]; last.Seq != 6 || last.Type != agent.EvUserMessage {
		t.Fatalf("the other process's run was ended: last event %d %s", last.Seq, last.Type)
	}
}

// A rebuild that fails after the claim releases the row it claimed.
func TestFailedRebuildAfterClaimReleasesTheRow(t *testing.T) {
	st, rs, r, sess := resumeRig(t, "me", "default")
	ctx := context.Background()
	handleCommand(ctx, "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
	// Elsewhere a message is recorded that no conversation can be rebuilt from.
	rs.mu.Lock()
	rs.MemStore = agent.NewMemStore()
	rs.mu.Unlock()
	bad, _ := json.Marshal(agent.Message{Text: "x"})
	_ = rs.MemStore.Append(agent.Event{ID: "b1", SessionID: "s-old", Seq: 1, Type: agent.EvUserMessage, Payload: []byte(`"not an object"`)})
	_ = rs.MemStore.Append(agent.Event{ID: "b2", SessionID: "s-old", Seq: 2, Type: agent.EvSessionEnded, Payload: bad})
	if err := claimResumed(ctx, st); err == nil {
		t.Fatal("the rebuild went ahead")
	}
	if rs.rows["s-old"].EndedAt == nil {
		t.Fatal("the claimed row was left running after the rebuild failed")
	}
}

// A /fork or /compact that claims the session only to write to it leaves the
// row ended as it was, with the same reason and totals, so quitting then
// leaves it resumable, here or by another process.
func TestForkOrCompactThenQuitLeavesTheSessionResumable(t *testing.T) {
	for _, cmd := range []string{"/fork 2", "/compact"} {
		st, rs, r, sess := resumeRig(t, "me", "default")
		ctx := context.Background()
		handleCommand(ctx, "/resume s-old", r, policy.New(policy.ModeDefault), sess, st)
		handleCommand(ctx, cmd, r, policy.New(policy.ModeDefault), sess, st)
		handleCommand(ctx, "/quit", r, policy.New(policy.ModeDefault), sess, st)
		if rs.claims != 1 || rs.rows["s-old"].EndedAt == nil {
			t.Fatalf("%s then /quit left the row running (%d claims)", cmd, rs.claims)
		}
		events, _ := rs.Events("s-old")
		var end agent.SessionEnded
		if last := events[len(events)-1]; last.Type != agent.EvSessionEnded || json.Unmarshal(last.Payload, &end) != nil ||
			end.Reason != agent.TermCompleted || end.Turns != 3 || end.TokensIn != 42 {
			t.Fatalf("%s: the row was not ended as before: %+v", cmd, end)
		}
		if rep := hawkeye.Analyze("s-old", events); rep.Outcome != string(agent.TermCompleted) || len(rep.Integrity.Gaps) != 0 {
			t.Fatalf("%s: HawkEYE reads the record as %s with gaps %v", cmd, rep.Outcome, rep.Integrity.Gaps)
		} else {
			for _, f := range rep.Findings {
				if f.Severity == hawkeye.Critical {
					t.Fatalf("%s: a critical finding: %+v", cmd, f)
				}
			}
		}
		// A later process resumes it, and its first task can claim it.
		later := rigState(rs, sess)
		handleCommand(ctx, "/resume s-old", r, policy.New(policy.ModeDefault), sess, later)
		if err := claimResumed(ctx, later); err != nil || later.loop == nil {
			t.Fatalf("%s: a later /resume could not continue: %v", cmd, err)
		}
	}
}

// Every task in interactive() ends through settleTurn, which ends a failed
// turn and claims the session again before the next write.
func TestInteractiveSettlesEveryTask(t *testing.T) {
	src, err := os.ReadFile("cli_interactive.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "\nfunc interactive(")
	end := strings.Index(body[start+1:], "\n}\n")
	if start < 0 || end < 0 || !strings.Contains(body[start:start+1+end], "settleTurn(sessionState, runErr)") {
		t.Fatal("interactive() no longer ends each task through settleTurn")
	}
}

// An "always allow" lasts one session: /clear and /resume start another
// without it.
func TestAlwaysAllowEndsWithTheSession(t *testing.T) {
	for _, cmd := range []string{"/clear", "/resume s-old"} {
		st, _, r, sess := resumeRig(t, "me", "default")
		ap := ui.NewApprover(io.Discard)
		st.scopes = ap.Session
		st.open("s-live")
		ap.Session.Add("bash(mkdir *)")
		handleCommand(context.Background(), cmd, r, policy.New(policy.ModeDefault), sess, st)
		if ap.Session.Has("bash(mkdir *)") {
			t.Errorf("%s: a scope from the last session still approves", cmd)
		}
	}
}

// A second Ctrl-C ends a turn still stopping as user_interrupt, and adds no
// end to a turn that already recorded its own.
func TestDoubleCtrlCEndsAsInterrupted(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		st, rs, _, _ := resumeRig(t, "me", "default")
		loop := st.open("s-new")
		if _, err := loop.Recorder.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "go"}); err != nil {
			t.Fatal(err)
		}
		if stopped {
			_, _ = loop.Recorder.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermUserInterrupt})
		}
		endOnExit(st, stopped)
		events, _ := rs.Events("s-new")
		ends := 0
		for _, ev := range events {
			if ev.Type == agent.EvSessionEnded {
				ends++
			}
		}
		end, _ := agent.LastEnd(events)
		if ends != 1 || end.Reason != agent.TermUserInterrupt {
			t.Errorf("stopped %v: %d ends, last %q; want one, user_interrupt", stopped, ends, end.Reason)
		}
	}
}

// A subagent's record is not resumed on its own: it goes on only through the
// session that started it.
func TestResumeRefusesASubagentsSession(t *testing.T) {
	ms := agent.NewMemStore()
	msg, _ := json.Marshal(agent.Message{Text: "child work ZEBRA-77"})
	_ = ms.Append(agent.Event{ID: "k1", SessionID: "s-kid", ParentID: "s-top", Seq: 1, Type: agent.EvUserMessage, Payload: msg})
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &cliState{store: ms, appCfg: config.Default(), sess: sess}
	st.fresh()
	st.open = func(id string) *agent.Loop {
		l := agent.NewLoop(nil, nil, policy.New(policy.ModeDefault), agent.AutoApprove{}, sess, agent.NewRecorder(ms, id, ""), agent.DefaultConfig())
		st.loop, st.sessionID = l, id
		return l
	}
	var shown bytes.Buffer
	handleCommand(context.Background(), "/resume s-kid", ui.NewRenderer(&shown, false), policy.New(policy.ModeDefault), sess, st)
	if st.loop != nil || st.sessionID == "s-kid" {
		t.Fatal("a subagent's session was resumed on its own")
	}
	if err := resumeConversation(context.Background(), st, "s-kid", mustEvents(t, ms, "s-kid")); err == nil || !strings.Contains(err.Error(), "s-top") {
		t.Fatalf("the refusal does not name the session to resume: %v", err)
	}
}

func mustEvents(t *testing.T, st agent.Store, id string) []agent.Event {
	t.Helper()
	evs, err := st.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// Ownership is checked first, so another user's subagent record is refused
// as theirs without naming the session that started it; a record from before
// events named a parent is known by its first event, the subagent's spawn.
func TestResumeChecksTheOwnerBeforeTheSubagent(t *testing.T) {
	child := []agent.Event{{ID: "k1", SessionID: "s-old", ParentID: "s-secret-parent", Seq: 1, Type: agent.EvUserMessage}}
	st, _, _, _ := resumeRig(t, "mallory", "default")
	if err := resumeConversation(context.Background(), st, "s-old", child); err == nil ||
		!strings.Contains(err.Error(), "another user") || strings.Contains(err.Error(), "s-secret-parent") {
		t.Fatalf("another user's subagent record: %v", err)
	}
	st, _, _, _ = resumeRig(t, "me", "default")
	legacy := []agent.Event{{ID: "k1", SessionID: "s-old", Seq: 1, Type: agent.EvSubagentSpawned}}
	if err := resumeConversation(context.Background(), st, "s-old", legacy); err == nil || !strings.Contains(err.Error(), "subagent") {
		t.Fatalf("a subagent record with no parent named was resumed: %v", err)
	}
}

// /resume goes on from what the session spent, and queues what its
// background children left undelivered.
func TestCLIResumeCarriesBudgetAndNotices(t *testing.T) {
	ms := agent.NewMemStore()
	p := agent.NewRecorder(ms, "s-bg", "")
	_, _ = p.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "go"})
	_, _ = p.Record(agent.EvSubagentSpawned, agent.ActorAgent, agent.Trusted, map[string]any{"session": "c1", "task_id": "c1", "background": true})
	_, _ = p.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted, TokensIn: 70, TokensOut: 5})
	_, _ = p.Record(agent.EvSubagentReturn, agent.ActorAgent, agent.Trusted, map[string]any{"session": "c1", "task_id": "c1", "background": true,
		"reason": "completed", "tokens_in": 20, "tokens_out": 5})
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &cliState{store: ms, appCfg: config.Default(), sess: sess}
	st.fresh()
	st.open = func(id string) *agent.Loop {
		l := agent.NewLoop(nil, nil, policy.New(policy.ModeDefault), agent.AutoApprove{}, sess, agent.NewRecorder(ms, id, ""), agent.DefaultConfig())
		l.Budget = agent.NewBudget(0, 10, false)
		st.loop, st.sessionID = l, id
		return l
	}
	evs, _ := ms.Events("s-bg")
	if err := rebuildFrom(st, "s-bg", evs); err != nil {
		t.Fatal(err)
	}
	if st.loop.Budget.Spent() != 100 || st.loop.Background.Pending() != 1 {
		t.Fatalf("spent %d, pending %d", st.loop.Budget.Spent(), st.loop.Background.Pending())
	}
}

// A CLI subagent's row is recorded as store.SubagentUser. /resume of the
// person's own subagent names the session that started it, through a nested
// subagent too; another user's subagent is refused as theirs, its parent unnamed.
func TestResumeOfOwnSubagentNamesItsParent(t *testing.T) {
	st, rs, _, sess := resumeRig(t, "me", "default")
	ended := time.Now()
	rs.rows["s-top"] = store.SessionRecord{ID: "s-top", User: "me", Tenant: "default", EndedAt: &ended}
	rs.rows["s-kid"] = store.SessionRecord{ID: "s-kid", User: store.SubagentUser, Tenant: "default", ParentID: "s-top", EndedAt: &ended}
	rs.rows["s-grandkid"] = store.SessionRecord{ID: "s-grandkid", User: store.SubagentUser, Tenant: "default", ParentID: "s-kid", EndedAt: &ended}
	rs.rows["s-their-top"] = store.SessionRecord{ID: "s-their-top", User: "mallory", Tenant: "default", EndedAt: &ended}
	rs.rows["s-their-kid"] = store.SessionRecord{ID: "s-their-kid", User: store.SubagentUser, Tenant: "default", ParentID: "s-their-top", EndedAt: &ended}
	rs.rows["s-orphan"] = store.SessionRecord{ID: "s-orphan", User: store.SubagentUser, Tenant: "default", ParentID: "s-gone", EndedAt: &ended}
	msg, _ := json.Marshal(agent.Message{Text: "child work OKAPI-3"})
	for id, parent := range map[string]string{"s-kid": "s-top", "s-grandkid": "s-kid", "s-their-kid": "s-their-top", "s-orphan": "s-gone"} {
		_ = rs.Append(agent.Event{ID: id + "-1", SessionID: id, ParentID: parent, Seq: 1, Type: agent.EvUserMessage, Payload: msg})
	}
	resume := func(id string) string {
		var shown bytes.Buffer
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdout
		os.Stdout = w
		handleCommand(context.Background(), "/resume "+id, ui.NewRenderer(&shown, false), policy.New(policy.ModeDefault), sess, st)
		os.Stdout = old
		_ = w.Close()
		said, _ := io.ReadAll(r)
		if st.loop != nil || st.claim != "" || rs.claims != 0 {
			t.Fatalf("%s was resumed on its own", id)
		}
		return shown.String() + string(said)
	}
	for id, parent := range map[string]string{"s-kid": "s-top", "s-grandkid": "s-kid"} {
		if got := resume(id); strings.Contains(got, "another user") || !strings.Contains(got, "is a subagent's; resume "+parent) {
			t.Errorf("/resume of my own subagent %s:\n%s", id, got)
		}
	}
	for _, id := range []string{"s-their-kid", "s-orphan"} {
		got := resume(id)
		if !strings.Contains(got, "belongs to another user") || strings.Contains(got, "s-their-top") ||
			strings.Contains(got, "s-gone") || strings.Contains(got, "OKAPI-3") {
			t.Errorf("/resume of %s, not mine, said more than whose it is:\n%s", id, got)
		}
	}
}

// brokenParent is a store whose lookup of one session fails.
type brokenParent struct {
	*rowStore
	broken string
}

func (b brokenParent) GetSession(ctx context.Context, id string) (store.SessionRecord, error) {
	if id == b.broken {
		return store.SessionRecord{}, fmt.Errorf("connection reset")
	}
	return b.rowStore.GetSession(ctx, id)
}

// A subagent's row is owned only through a parent in this tenant, never by a
// user named like the subagent rows are, and a lookup that fails says so
// rather than blaming another user.
func TestSubagentOwnershipWalkEdges(t *testing.T) {
	st, rs, _, _ := resumeRig(t, "me", "default")
	ended := time.Now()
	rs.rows["s-far-top"] = store.SessionRecord{ID: "s-far-top", User: "me", Tenant: "other", EndedAt: &ended}
	rs.rows["s-far-kid"] = store.SessionRecord{ID: "s-far-kid", User: store.SubagentUser, Tenant: "default", ParentID: "s-far-top", EndedAt: &ended}
	rs.rows["s-orphan"] = store.SessionRecord{ID: "s-orphan", User: store.SubagentUser, Tenant: "default", ParentID: "s-gone", EndedAt: &ended}
	rs.rows["s-top"] = store.SessionRecord{ID: "s-top", User: "me", Tenant: "default", EndedAt: &ended}
	rs.rows["s-kid"] = store.SessionRecord{ID: "s-kid", User: store.SubagentUser, Tenant: "default", ParentID: "s-top", EndedAt: &ended}
	if err := ownedHere(context.Background(), st, "s-far-kid"); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("a subagent whose parent is in another tenant was owned here: %v", err)
	}
	t.Setenv("USER", store.SubagentUser)
	if err := ownedHere(context.Background(), st, "s-orphan"); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("a user named %q owned an orphaned subagent row: %v", store.SubagentUser, err)
	}
	t.Setenv("USER", "me")
	st.store = brokenParent{rowStore: rs, broken: "s-top"}
	if err := ownedHere(context.Background(), st, "s-kid"); err == nil || strings.Contains(err.Error(), "another user") || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("a failed lookup was not reported as one: %v", err)
	}
}

// A session the owner migration moved to the account named like this OS
// user is still this user's; another account's is not.
func TestCLIOwnsItsMigratedSessions(t *testing.T) {
	st, rs, _, _ := resumeRig(t, "me", "default")
	t.Setenv("USER", "Me")
	ended := time.Now()
	rs.rows["s-moved"] = store.SessionRecord{ID: "s-moved", User: "local:me", Tenant: "default", EndedAt: &ended}
	rs.rows["s-theirs"] = store.SessionRecord{ID: "s-theirs", User: "local:you", Tenant: "default", EndedAt: &ended}
	if err := ownedHere(context.Background(), st, "s-moved"); err != nil {
		t.Errorf("the CLI lost its migrated session: %v", err)
	}
	if err := ownedHere(context.Background(), st, "s-theirs"); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("another account's session was owned here: %v", err)
	}
}

// An unclaimed or nobody row is refused even when $USER is spelled like it.
func TestCLIRefusesUnclaimedOwnerViaUser(t *testing.T) {
	st, rs, _, _ := resumeRig(t, "me", "default")
	ended := time.Now()
	for _, owner := range []string{"unclaimed:bob@example.test", "nobody:local", "Unclaimed:Bob"} {
		rs.rows["s-x"] = store.SessionRecord{ID: "s-x", User: owner, Tenant: "default", EndedAt: &ended}
		t.Setenv("USER", owner)
		if err := ownedHere(context.Background(), st, "s-x"); err == nil || !strings.Contains(err.Error(), "another user") {
			t.Errorf("$USER=%q resumed a row owned by %q: %v", owner, owner, err)
		}
	}
}
