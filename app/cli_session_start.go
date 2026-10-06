package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// sessionFlags are -c, -r, -n and --fork-session.
type sessionFlags struct {
	Continue bool
	Resume   string
	// Pick is -r with no session named: choose one from a list.
	Pick bool
	Name string
	Fork bool
}

// startFlags are this run's session flags, set by Main.
var startFlags sessionFlags

func (f sessionFlags) check() error {
	n := 0
	for _, set := range []bool{f.Continue, f.Resume != "", f.Pick} {
		if set {
			n++
		}
	}
	switch {
	case n > 1:
		return errors.New("use one of -c and -r")
	case f.Fork && n == 0:
		return errors.New("--fork-session goes with -c or -r")
	}
	return nil
}

// resuming reports whether the run goes on from a recorded session.
func (f sessionFlags) resuming() bool { return f.Continue || f.Resume != "" || f.Pick }

// valueFlags are the top-level flags that take a value, so the scan for a
// bare -r does not read one as an argument.
var valueFlags = map[string]bool{"p": true, "mode": true, "model": true, "C": true, "add-dir": true,
	"max-turns": true, "output-format": true, "allow": true, "deny": true, "addr": true,
	"r": true, "resume": true, "n": true, "name": true}

// bareResume takes out a -r or --resume that names no session, which the
// flag package would refuse, and reports that the person wants to pick one.
func bareResume(args []string) ([]string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			return args, false
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if name == "r" || name == "resume" {
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				return append(args[:i:i], args[i+1:]...), true
			}
		}
		if valueFlags[name] {
			i++
		}
	}
	return args, false
}

// startSession applies -c, -r and --fork-session as the interactive session
// opens: the conversation to go on with, or a new one when there is none.
func startSession(ctx context.Context, st *cliState, r *ui.Renderer) {
	f := startFlags
	st.pendingName = f.Name
	sessionWorkspace = st.workspace
	if !f.resuming() {
		return
	}
	defer nameNow(ctx, st)
	s := r.Style()
	say := func(format string, args ...any) { fmt.Printf("  %s\n", s.Dim(fmt.Sprintf(format, args...))) }
	id, events, copied, err := chooseSession(ctx, st, f, say)
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		say("starting a new session")
		return
	}
	if id == "" {
		return
	}
	if copied {
		if err := adoptBranch(st, id); err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return
		}
		replayTail(r, events)
		return
	}
	if f.Fork {
		newID, err := branchInto(ctx, st, id, events, 0)
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return
		}
		replayTail(r, events)
		say("branched %s into %s; the original is left as it was", id, newID)
		return
	}
	continueSession(ctx, st, r, id, events)
}

// continueSession makes recorded session id the conversation, offering a
// fork when another Abhed process has it open.
func continueSession(ctx context.Context, st *cliState, r *ui.Renderer, id string, events []agent.Event) {
	s := r.Style()
	if rec, ok, _ := storedSession(ctx, st, id); ok && rec.EndedAt == nil && id != st.sessionID {
		answer, err := st.ui().Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm,
			Title: fmt.Sprintf("Session %s is open in another Abhed process. Fork it into a new session?", id),
			Why:   "one process writes a session at a time; a fork leaves it untouched"})
		if err != nil || answer != ui.ChoiceYes {
			fmt.Printf("  %s\n", s.Dim("not resumed"))
			return
		}
		newID, err := branchInto(ctx, st, id, events, 0)
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return
		}
		replayTail(r, events)
		fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("forked into %s", newID)))
		return
	}
	replayTail(r, events)
	if err := resumeConversation(ctx, st, id, events); err != nil {
		fmt.Printf("  %s replayed, not continued: %v\n", s.Red("✕"), err)
		return
	}
	noteMove(st, s, events)
	fmt.Printf("  %s\n", s.Dim("resumed "+id+" — the next thing you type continues it"))
}

// chooseSession finds the session -c or -r names, checks its record, and
// returns its id and events; "" when there is none to go on with. copied is
// set when a file from outside the record, or an unverified record, was
// copied into a new session.
func chooseSession(ctx context.Context, st *cliState, f sessionFlags, say func(string, ...any)) (id string, events []agent.Event, copied bool, err error) {
	st.copiedID, st.copiedEvents = "", nil
	id, events, err = findSession(ctx, st, f, say)
	if err != nil || id != "" {
		return id, events, false, err
	}
	return st.copiedID, st.copiedEvents, st.copiedID != "", nil
}

func findSession(ctx context.Context, st *cliState, f sessionFlags, say func(string, ...any)) (string, []agent.Event, error) {
	rec, _ := st.store.(*local.Store)
	var id string
	switch {
	case f.Continue:
		if rec == nil {
			return "", nil, errors.New("-c needs the local record; this run uses storage.driver = postgres, so name the session with -r")
		}
		entries, err := rec.Index().List(local.Filter{Cwd: st.workspace, Limit: 1})
		if err != nil {
			return "", nil, err
		}
		if len(entries) == 0 {
			say("no session to continue in this workspace; starting a new one")
			return "", nil, nil
		}
		id = entries[0].ID
	case f.Pick:
		picked, err := pickSession(ctx, st, say)
		if err != nil || picked == "" {
			return "", nil, err
		}
		id = picked
	default:
		if info, err := os.Stat(f.Resume); err == nil && !info.IsDir() && !inRecordDir(rec, f.Resume) {
			return fromFile(ctx, st, f.Resume, say)
		}
		if rec == nil {
			id = f.Resume
			break
		}
		e, err := rec.Index().Resolve(f.Resume)
		if err != nil {
			return "", nil, err
		}
		id = e.ID
	}
	unverified, err := checkRecorded(ctx, st, id)
	if err != nil {
		return "", nil, err
	}
	events, err := st.store.Events(id)
	if err != nil {
		return "", nil, err
	}
	if len(events) == 0 {
		return "", nil, fmt.Errorf("session %s has no events", id)
	}
	if unverified != "" {
		// Nothing more is written into a record that fails: going on from it
		// is a fork into a new session, which names it and why.
		newID, err := copyBranch(ctx, st.store, st.appCfg, id, events, 0, unverified, true)
		if err != nil {
			return "", nil, err
		}
		say("going on in a new session %s; %s is left exactly as it is", newID, id)
		st.copiedID, st.copiedEvents = newID, events
		return "", nil, nil
	}
	return id, events, nil
}

// inRecordDir reports whether path is a session file in the record.
func inRecordDir(rec *local.Store, path string) bool {
	if rec == nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	_, err = rec.Index().Resolve(abs)
	return err == nil
}

// checkRecorded verifies a session's chain before it goes on. A record that
// fails is shown as unverified and, only when the person confirms, goes on
// in a fork; the reason is returned for the fork to record.
func checkRecorded(ctx context.Context, st *cliState, id string) (string, error) {
	rec, ok := st.store.(*local.Store)
	if !ok {
		return "", nil
	}
	rep, err := rec.Verify(id)
	if err != nil {
		return "", err
	}
	if rep.OK {
		return "", nil
	}
	return unverifiedReason(rep), confirmUnverified(ctx, st, id, rep)
}

func unverifiedReason(rep local.Report) string {
	return fmt.Sprintf("at seq %d, %s", rep.FirstBad, rep.Reason)
}

func confirmUnverified(ctx context.Context, st *cliState, name string, rep local.Report) error {
	answer, err := st.ui().Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm,
		Title: fmt.Sprintf("The record of %s is unverified: at seq %d, %s. Go on from it in a new session?", name, rep.FirstBad, rep.Reason),
		Why:   "the chain shows an edit, a reorder or lines missing; the record is left as it is, and abhed record verify has the details"})
	if err != nil || answer != ui.ChoiceYes {
		return fmt.Errorf("not continued: the record of %s is unverified", name)
	}
	return nil
}

// fromFile resumes from a record file outside this record, such as an
// export: it is verified, and on a person's confirmation when it fails, and
// branched into a new session here, since the file is not this record's.
func fromFile(ctx context.Context, st *cliState, path string, say func(string, ...any)) (string, []agent.Event, error) {
	rep, err := local.VerifyFile(path)
	if err != nil {
		return "", nil, err
	}
	unverified := ""
	if !rep.OK {
		if err := confirmUnverified(ctx, st, path, rep); err != nil {
			return "", nil, err
		}
		unverified = unverifiedReason(rep)
	}
	events, err := readRecordFile(path)
	if err != nil {
		return "", nil, err
	}
	if len(events) == 0 {
		return "", nil, fmt.Errorf("%s holds no events", path)
	}
	newID, err := copyBranch(ctx, st.store, st.appCfg, events[0].SessionID, events, 0, unverified, true)
	if err != nil {
		return "", nil, err
	}
	say("%s copied into a new session %s; the file is left as it is", path, newID)
	st.copiedID, st.copiedEvents = newID, events
	return "", nil, nil
}

// readRecordFile reads the events of a record file, an export's trailer
// passed over.
func readRecordFile(path string) ([]agent.Event, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the person names the file
	if err != nil {
		return nil, err
	}
	var out []agent.Event
	for i, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(raw, `"abhed_record_head"`) {
			continue
		}
		var ev agent.Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return nil, fmt.Errorf("%s line %d is not a record line: %w", path, i+1, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// pickSession asks which of this workspace's sessions to resume; a last
// entry widens the list to every workspace. Typing filters the list, and the
// session already open is marked and is not resumed again.
func pickSession(ctx context.Context, st *cliState, say func(string, ...any)) (string, error) {
	rec, ok := st.store.(*local.Store)
	if !ok {
		return pickStoredSession(ctx, st, say)
	}
	for _, all := range []bool{false, true} {
		entries, err := rec.Index().List(local.Filter{Cwd: st.workspace, All: all, Limit: 30})
		if err != nil {
			return "", err
		}
		if len(entries) == 0 && all {
			return "", errors.New("no sessions recorded")
		}
		var items []ui.PickItem
		for _, e := range entries {
			items = append(items, pickItem(e, all, st.sessionID))
		}
		title := "Resume which session? (this workspace)"
		if all {
			title = "Resume which session? (every workspace)"
		} else {
			items = append(items, ui.PickItem{ID: "*all", Label: "…sessions in every workspace", Always: true})
		}
		id, err := st.ui().Pick(ctx, ui.PickSpec{Title: title, Items: items, Default: firstOther(items, st.sessionID), Filter: true})
		if err != nil {
			return "", nil //nolint:nilerr // no answer is no session, not a failure
		}
		if id != "*all" {
			return notCurrent(st, id, say), nil
		}
	}
	return "", nil
}

// pickStoredSession is the picker where the record is a database: this
// user's sessions, latest active first, in every workspace.
func pickStoredSession(ctx context.Context, st *cliState, say func(string, ...any)) (string, error) {
	lister, ok := st.store.(interface {
		ListSessions(context.Context, int) ([]store.SessionRecord, error)
	})
	if !ok {
		return "", errors.New("the session picker needs a session record; name the session with -r <id>")
	}
	records, err := lister.ListSessions(ctx, 200)
	if err != nil {
		return "", err
	}
	sortByActivity(records)
	me, tenant := cliUser(), cliTenant(st.appCfg)
	var items []ui.PickItem
	for _, rec := range records {
		if rec.ParentID != "" || rec.Tenant != tenant || rec.User != me && rec.User != auth.LocalOwner(me) {
			continue
		}
		detail := fmt.Sprintf("%s · %s", age(activityOf(rec)), recordState(rec))
		if rec.Workspace != "" && rec.Workspace != st.workspace {
			detail += " · " + rec.Workspace
		}
		if rec.ID == st.sessionID {
			detail = "● current · " + detail
		}
		items = append(items, ui.PickItem{ID: rec.ID, Label: orDefault(recordLabel(rec), "(no prompt)"), Detail: detail})
		if len(items) == 30 {
			break
		}
	}
	if len(items) == 0 {
		return "", errors.New("no sessions recorded for you")
	}
	id, err := st.ui().Pick(ctx, ui.PickSpec{Title: "Resume which session?", Items: items, Default: firstOther(items, st.sessionID), Filter: true})
	if err != nil {
		return "", nil //nolint:nilerr // no answer is no session, not a failure
	}
	return notCurrent(st, id, say), nil
}

// firstOther is the first item that is not the session already open, so
// Enter never offers to resume the session in use.
func firstOther(items []ui.PickItem, current string) string {
	for _, it := range items {
		if it.ID != current && !strings.HasPrefix(it.ID, "*") {
			return it.ID
		}
	}
	return ""
}

// notCurrent passes on a picked session unless it is the one already open,
// which is said and left as it is.
func notCurrent(st *cliState, id string, say func(string, ...any)) string {
	if id != "" && id == st.sessionID {
		say("%s is the session you are in", id)
		return ""
	}
	return id
}

func pickItem(e local.Entry, withCwd bool, current string) ui.PickItem {
	label := orDefault(e.Title, "(no prompt)")
	if e.Name != "" {
		label = e.Name + " · " + label
	}
	state := orDefault(e.Ended, "open")
	detail := fmt.Sprintf("%s · %s · %s", age(e.Active), orDefault(e.GitBranch, "-"), state)
	if withCwd {
		detail += " · " + e.Cwd
	}
	if e.ID == current {
		detail = "● current · " + detail
	}
	return ui.PickItem{ID: e.ID, Label: label, Detail: detail}
}

// resumeTurnsShown is how many of a resumed session's prompts are replayed
// on the screen; the rest are summarised.
const resumeTurnsShown = 10

// replayTail draws the last resumeTurnsShown prompts of a session and what followed them,
// with a line for what came before. Nothing is run again.
func replayTail(r *ui.Renderer, events []agent.Event) {
	n := resumeTurnsShown
	live := agent.Live(events)
	start, seen := 0, 0
	for i := len(live) - 1; i >= 0; i-- {
		if live[i].Type == agent.EvUserMessage {
			if seen++; seen == n {
				start = i
				break
			}
		}
	}
	if start > 0 {
		fmt.Printf("  %s\n", r.Style().Dim(fmt.Sprintf("… %d earlier events (abhed record show has them all)", start)))
	}
	for _, ev := range live[start:] {
		r.Event(ev)
	}
}

// noteMove says, and later records, that a resumed session goes on on
// another model than it last ran on.
func noteMove(st *cliState, s ui.Style, events []agent.Event) {
	last, model := agent.ProviderOf(events), agent.LastModel(events)
	if p, ok := st.appCfg.Model.Providers[last]; ok && p.Model != "" {
		model = p.Model // the provider the record names, over a call it may predate
	}
	if last != st.appCfg.Model.Default && (last != "" || model != "" && model != st.adapter.Profile().Name) {
		fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("it last ran on %s and continues on %s; /model %s goes back",
			orDefault(model, last), st.adapter.Profile().Name, orDefault(last, "<provider>"))))
		st.moved = &agent.ModelSwitched{Provider: st.appCfg.Model.Default, Model: st.adapter.Profile().Name, From: model}
	}
}

// redactPayload replaces stored secrets in a payload, and withholds one
// whose redaction left invalid JSON.
func redactPayload(red agent.Redactor, payload json.RawMessage) json.RawMessage {
	if v := reflect.ValueOf(red); red == nil || v.Kind() == reflect.Pointer && v.IsNil() || len(payload) == 0 {
		return payload
	}
	out := red.Redact(payload)
	if !json.Valid(out) {
		return json.RawMessage(`{"withheld":"` + agent.Withheld + `"}`)
	}
	return out
}

// branchInto copies session from's conversation, as it stands through seq
// (0 for all of it), into a new session and makes that the conversation.
func branchInto(ctx context.Context, st *cliState, from string, events []agent.Event, through int64) (string, error) {
	id, err := copyBranch(ctx, st.store, st.appCfg, from, events, through, "", false)
	if err != nil {
		return "", err
	}
	return id, adoptBranch(st, id)
}

// adoptBranch makes a branch just made the conversation.
func adoptBranch(st *cliState, id string) error {
	st.endBackground()
	releaseConversation(st)
	st.fresh()
	all, err := st.store.Events(id)
	if err != nil {
		return err
	}
	return rebuildFrom(st, id, all)
}

// copyBranch writes a new session holding from's conversation through seq.
// Its record opens with session.branched naming the source and the last seq
// taken, then the copied events, renumbered; the source is not touched.
// foreign marks a source whose record cannot be vouched for (a file from
// elsewhere, or one that failed verification): its copies are untrusted.
func copyBranch(ctx context.Context, es server.EventStore, cfg config.Config, from string, events []agent.Event, through int64, unverified string, foreign bool) (string, error) {
	id := newConversationID()
	copied := agent.BranchCopy(events, through, id, 2)
	if len(copied) == 0 {
		return "", fmt.Errorf("nothing to branch from %s", from)
	}
	last := through
	if last == 0 {
		for _, ev := range events {
			last = max(last, ev.Seq)
		}
	}
	if err := recordSession(ctx, es, id, cfg); err != nil {
		return "", err
	}
	rec := agent.NewRecorder(es, id, "")
	rec.Redact = openVault().Redactor()
	if _, err := rec.Record(agent.EvSessionBranched, agent.ActorUser, agent.Trusted,
		agent.SessionBranched{From: from, ThroughSeq: last, Unverified: unverified}); err != nil {
		return "", err
	}
	red := openVault().Redactor()
	for _, ev := range copied {
		// Redacted here, before any store: Postgres does not redact on append.
		ev.Payload = redactPayload(red, ev.Payload)
		if foreign {
			ev.Trust = agent.Untrusted
		}
		if err := es.Append(ev); err != nil {
			return "", fmt.Errorf("copy into %s: %w", id, err)
		}
	}
	return id, nil
}

// afterOpen records what was waiting for the conversation to exist: the
// name -n or /rename gave before the first prompt.
func afterOpen(st *cliState) {
	if st.pendingName == "" || st.loop == nil {
		return
	}
	if _, err := st.loop.Recorder.Record(agent.EvSessionNamed, agent.ActorUser, agent.Trusted, agent.SessionNamed{Name: st.pendingName}); err == nil {
		st.pendingName = ""
	}
}

// nameNow records a pending name on a conversation already open, such as
// one -r resumed.
func nameNow(ctx context.Context, st *cliState) {
	if st.pendingName == "" || st.loop == nil {
		return
	}
	release, err := claimForWrite(ctx, st)
	if err != nil {
		return
	}
	afterOpen(st)
	release()
}

// ui is the session's Surface: the terminal UI's, or a line surface.
func (c *cliState) ui() ui.Surface {
	if c.surface != nil {
		return c.surface
	}
	return ui.NewLineSurface(ui.LazyStdout{}, ui.Style{}, nil)
}

// headlessSession is the session a -p run writes: a new one, or with -c or
// -r the one it names, claimed and rebuilt; with --fork-session, or from a
// file outside the record, a branch of it. seed puts the rebuilt
// conversation on the run's loop.
func headlessSession(ctx context.Context, st *cliState) (string, func(*agent.Loop), error) {
	f := startFlags
	sessionWorkspace = st.workspace
	none := func(*agent.Loop) {}
	if !f.resuming() {
		id := newConversationID()
		return id, none, recordSession(ctx, st.store, id, st.appCfg)
	}
	if f.Pick {
		return "", nil, errors.New("-p cannot pick a session; name it with -r <id or name>, or use -c")
	}
	quiet := func(string, ...any) {}
	from, events, copied, err := chooseSession(ctx, st, f, quiet)
	switch {
	case err != nil:
		return "", nil, err
	case from == "":
		id := newConversationID()
		return id, none, recordSession(ctx, st.store, id, st.appCfg)
	case copied:
		return seeded(st.store, from)
	case f.Fork:
		id, err := copyBranch(ctx, st.store, st.appCfg, from, events, 0, "", false)
		if err != nil {
			return "", nil, err
		}
		return seeded(st.store, id)
	}
	if err := ownedHere(ctx, st, from); err != nil {
		return "", nil, err
	}
	if rec, ok := st.store.(server.SessionResumer); ok {
		claimed, err := rec.ClaimResume(ctx, from)
		if err != nil {
			return "", nil, err
		}
		if !claimed {
			return "", nil, fmt.Errorf("session %s is open in another Abhed process; --fork-session goes on in a copy", from)
		}
	}
	return seeded(st.store, from)
}

// seeded reads session id and rebuilds it on the run's loop, as a resume does.
func seeded(es server.EventStore, id string) (string, func(*agent.Loop), error) {
	events, err := es.Events(id)
	if err != nil || len(events) == 0 {
		return "", nil, fmt.Errorf("session %s: nothing recorded to go on from", id)
	}
	return id, func(loop *agent.Loop) {
		msgs, _ := agent.Fork(events, 0)
		end, _ := agent.LastEnd(events)
		loop.Recorder.Advance(events[len(events)-1].Seq)
		loop.SetHistory(msgs, end.Turns)
		loop.CarryUsage(end)
		loop.Budget.Carry(agent.CarriedSpend(events))
	}, nil
}
