package app

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

func init() {
	registerSlash(slashCmd{Name: "/clear", Args: "[name]", Help: "end this session and start a new one; nothing is deleted", Group: "session", Order: 60, Run: legacy("/clear", slashClear)})
	registerSlash(slashCmd{Name: "/rename", Args: "<name>", Help: "name this session, for /resume and -r", Group: "session", Order: 115, Run: legacy("/rename", slashRename)})
	registerSlash(slashCmd{Name: "/branch", Args: "[name]", Help: "go on in a copy of this session; the original stays as it is", Group: "session", Order: 125, Run: legacy("/branch", slashBranch)})
	registerSlash(slashCmd{Name: "/sessions", Help: "list this workspace's recorded sessions", Group: "session", Order: 110, ReadOnly: true, Run: legacy("/sessions", slashSessions)})
	registerSlash(slashCmd{Name: "/resume", Args: "[id|name]", Help: "replay a past session and continue its conversation; alone, pick one", Group: "session", Order: 120, Run: legacy("/resume", slashResume)})
	registerSlash(slashCmd{Name: "/switch", Args: "[id|name]", Help: "switch to another session: /resume by another name", Group: "session", Order: 121, Run: legacy("/switch", slashResume)})
	registerSlash(slashCmd{Name: "/export", Args: "[path]", Help: "write the transcript to ~/.abhed/exports (.html; .jsonl verifiable, .json, .txt)", Group: "session", Order: 150, Run: legacy("/export", slashExport)})
}

// slashSessions is /sessions.
func slashSessions(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if rec, ok := st.store.(*local.Store); ok {
		entries, err := rec.Index().List(local.Filter{Cwd: st.workspace, Limit: 20})
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		if len(entries) == 0 {
			fmt.Println(s.Dim("  no sessions recorded in this workspace; abhed record list -all shows every one"))
			return false
		}
		for _, e := range entries {
			mark := "  "
			if e.ID == st.sessionID {
				mark = "* "
			}
			fmt.Printf("%s%s\n", mark, entryLine(e, false))
		}
		fmt.Println(s.Dim("  /resume <id or name> continues one"))
		return false
	}
	lister, ok := st.store.(interface {
		ListSessions(context.Context, int) ([]store.SessionRecord, error)
	})
	if !ok {
		fmt.Println(s.Dim("  session history needs storage.driver = postgres"))
		return false
	}
	records, err := lister.ListSessions(ctx, 20)
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	if len(records) == 0 {
		fmt.Println(s.Dim("  no sessions recorded"))
		return false
	}
	sortByActivity(records)
	for _, rec := range records {
		if rec.ParentID != "" {
			continue // a subagent's own row
		}
		mark := "  "
		if rec.ID == st.sessionID {
			mark = "* "
		}
		fmt.Printf("%s%-27s %-10s %-9s %s  %s\n", mark, rec.ID, recordState(rec), age(activityOf(rec)),
			ui.VisibleLine(orDefault(recordLabel(rec), "(no prompt)")), s.Dim(rec.User))
	}
	fmt.Println(s.Dim("  /resume <id> continues one"))
	return false
}

// recordState is a session row's state as the lists show it.
func recordState(rec store.SessionRecord) string {
	if rec.EndedAt == nil {
		return "open"
	}
	return orDefault(rec.TerminalReason, "ended")
}

// recordLabel is a session row's title, else its opening request.
func recordLabel(rec store.SessionRecord) string {
	return strings.TrimSpace(strings.SplitN(orDefault(rec.Title, rec.Prompt), "\n", 2)[0])
}

// activityOf is when a session row was last active, its start where the
// store cannot say.
func activityOf(rec store.SessionRecord) time.Time {
	if rec.UpdatedAt.IsZero() {
		return rec.StartedAt
	}
	return rec.UpdatedAt
}

func sortByActivity(records []store.SessionRecord) {
	sort.SliceStable(records, func(i, j int) bool { return activityOf(records[i]).After(activityOf(records[j])) })
}

// sessionLabel is what the footer calls the session: the name /rename gave
// it, else its first prompt cut to 30 characters, else its id.
func (st *cliState) sessionLabel() string {
	if st.sessionID == "" {
		return ""
	}
	if rec, ok := st.store.(*local.Store); ok {
		if e, err := rec.Index().Get(st.sessionID); err == nil {
			return clipLabel(orDefault(e.Name, e.Title), st.sessionID)
		}
		return st.sessionID
	}
	if st.labelID != st.sessionID || st.label == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if rec, ok, _ := storedSession(ctx, st, st.sessionID); ok {
			st.labelID, st.label = st.sessionID, recordLabel(rec)
		}
	}
	if st.labelID == st.sessionID {
		return clipLabel(st.label, st.sessionID)
	}
	return st.sessionID
}

func clipLabel(label, id string) string {
	label = strings.TrimSpace(strings.SplitN(label, "\n", 2)[0])
	if label == "" {
		return id
	}
	if r := []rune(label); len(r) > 30 {
		return string(r[:29]) + "…"
	}
	return label
}

// slashResume is /resume.
func slashResume(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	f := sessionFlags{Pick: len(fields) < 2}
	if len(fields) >= 2 {
		f.Resume = fields[1]
	}
	say := func(format string, args ...any) { fmt.Printf("  %s\n", s.Dim(fmt.Sprintf(format, args...))) }
	id, events, copied, err := chooseSession(ctx, st, f, say)
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	if id == "" {
		return false
	}
	if copied {
		if err := adoptBranch(st, id); err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
		}
		return false
	}
	// Another user's session is not shown here, let alone continued.
	if err := ownedHere(ctx, st, id); err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("%s\n", s.Dim(fmt.Sprintf("  replaying %d events from %s", len(events), id)))
	continueSession(ctx, st, r, id, events)
	return false
}

// slashRename is /rename: the name is recorded in the session, and the
// index lists it, so /resume and -r find the session by it.
func slashRename(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	name := strings.TrimSpace(strings.Join(fields[1:], " "))
	if name == "" {
		fmt.Println(s.Dim("  usage: /rename <name>"))
		return false
	}
	// The rules a rename in the console or over the API has, and its words.
	name, why := store.CleanTitle(name)
	if why != "" {
		fmt.Printf("  %s not renamed: %s\n", s.Red("✕"), why)
		return false
	}
	st.pendingName = name
	if st.loop == nil {
		fmt.Println(s.Dim("  the session is named " + name + " when it starts"))
		return false
	}
	release, err := claimForWrite(ctx, st)
	if err != nil {
		fmt.Printf("  %s not renamed: %v\n", s.Red("✕"), err)
		return false
	}
	afterOpen(st)
	release()
	if st.pendingName != "" {
		fmt.Printf("  %s not renamed\n", s.Red("✕"))
		return false
	}
	st.labelID, st.label = st.sessionID, name
	fmt.Println(s.Dim("  named " + name))
	return false
}

// slashBranch is /branch: the conversation goes on in a new session that
// starts as a copy of this one; this one is left as it is.
func slashBranch(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if st.loop == nil || st.sessionID == "" {
		fmt.Println(s.Dim("  nothing to branch yet"))
		return false
	}
	from := st.sessionID
	events, err := st.store.Events(from)
	if err != nil || len(events) == 0 {
		fmt.Println(s.Dim("  nothing to branch yet"))
		return false
	}
	id, err := branchInto(ctx, st, from, events, 0)
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	if len(fields) > 1 {
		st.pendingName = strings.Join(fields[1:], " ")
		afterOpen(st)
	}
	fmt.Println(s.Dim(fmt.Sprintf("  branched: now in %s; %s is left as it was (/resume %s goes back)", id, from, from)))
	return false
}

// slashClear is /clear: this session ends, recorded, and the next task
// starts a new one. Nothing is deleted; /resume goes back.
func slashClear(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	st.endBackground()
	if st.loop != nil && st.claim == "" {
		endIfOpen(st, agent.TermCompleted)
	}
	releaseConversation(st)
	st.loop, st.sessionID = nil, ""
	st.fresh()
	st.pendingName = strings.TrimSpace(strings.Join(fields[1:], " "))
	fmt.Println(s.Dim("  context cleared; the workspace is untouched"))
	return false
}

// slashExport is /export. It writes to ~/.abhed/exports unless given a
// path, never into the workspace, where it would join the repository. The
// record is redacted before it is written, so the export is too.
func slashExport(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	events, err := st.store.Events(st.sessionID)
	if st.sessionID == "" || err != nil || len(events) == 0 {
		fmt.Println(s.Dim("  no transcript to export yet"))
		return false
	}
	path := ""
	if len(fields) > 1 {
		path = fields[1]
		if !filepath.IsAbs(path) {
			path = filepath.Join(st.workspace, path) // named from where the session works
		}
		if !insideDir(path, st.workspace) && !inExports(path) {
			answer, err := st.ui().Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm,
				Title: fmt.Sprintf("Write the transcript to %s, outside the workspace?", path),
				Why:   "an export goes to ~/.abhed/exports or the workspace unless you say otherwise"})
			if err != nil || answer != ui.ChoiceYes {
				fmt.Println(s.Dim("  not exported"))
				return false
			}
		}
	}
	format := "html"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		format = "json"
	case ".jsonl":
		format = "jsonl"
	case ".txt":
		format = "txt"
	}
	if path == "" {
		if path, err = exportPath("", st.sessionID, format); err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
	}
	rec, onLocal := st.store.(*local.Store)
	switch {
	case onLocal:
		var e local.Entry
		if e, err = rec.Index().Get(st.sessionID); err == nil {
			_, err = exportSession(rec, e, format, path, os.Stdout, false, st.workspace)
		}
	case format == "json" || format == "html":
		data := []byte(agent.ExportHTML(st.sessionID, events) + "\n")
		if format == "json" {
			data, err = json.MarshalIndent(events, "", "  ")
			data = append(data, '\n')
		}
		var f *os.File
		if err == nil {
			if f, err = openExport(path, st.workspace); err == nil {
				_, err = f.Write(data)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
			}
		}
	default:
		err = fmt.Errorf("%s export needs the local record; use .html or .json", format)
	}
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("  wrote %d events to %s\n", len(events), path)
	return false
}

// newConversationID is a session id no other process picks: a clock in
// seconds gave two CLIs started together the same one.
func newConversationID() string {
	b := make([]byte, 12)
	if _, err := crand.Read(b); err != nil {
		panic("abhed: system random source unavailable: " + err.Error())
	}
	return "s-" + hex.EncodeToString(b)
}

// recordSession creates the durable session row that events reference.
// A no-op on the memory store, which has no session table.
func recordSession(ctx context.Context, st server.EventStore, id string, cfg config.Config) error {
	rec, ok := st.(interface {
		CreateSession(context.Context, store.SessionRecord) error
	})
	if !ok {
		return nil
	}
	user := cliUser()
	provider, _ := cfg.Provider()
	tenant := cliTenant(cfg)
	if err := rec.CreateSession(ctx, store.SessionRecord{
		ID: id, Tenant: tenant, User: user,
		Workspace: orDefault(sessionWorkspace, mustCwd()), Model: provider.Model,
		Mode:      orDefault(cfg.Permissions.Mode, "default"),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: could not persist session: %v\n", err)
		return err
	}
	return nil
}

// sessionWorkspace is the workspace this process's sessions are recorded
// in, which -C makes other than the directory it started in.
var sessionWorkspace string

// cliTenant is the tenant the CLI records a session in.
func cliTenant(cfg config.Config) string {
	if cfg.Storage.Tenant != "" {
		return cfg.Storage.Tenant
	}
	return "default"
}

// cliUser is who the CLI records a session as. A $USER shaped like an owner
// the store gives a meaning (unclaimed:, nobody:, github:…, agent, anonymous)
// is not taken as a person's name, or it could record or resume as that owner.
func cliUser() string {
	user := os.Getenv("USER")
	switch u := strings.ToLower(strings.TrimSpace(user)); {
	case u == "", strings.Contains(u, ":"), u == auth.Subagent, u == auth.Anonymous:
		return "local"
	}
	return user
}

// resumeConversation makes recorded session id the conversation the next task
// continues, rebuilt from its record with its turn count, as in the server.
func resumeConversation(ctx context.Context, st *cliState, id string, events []agent.Event) error {
	if err := ownedHere(ctx, st, id); err != nil {
		return err
	}
	// A subagent's record goes on only through the session that started it.
	if parent, child := agent.SubagentRecord(events); child {
		if parent == "" {
			return fmt.Errorf("session %s is a subagent's; resume the session that started it", id)
		}
		return fmt.Errorf("session %s is a subagent's; resume %s, the session that started it", id, parent)
	}
	live := id == st.sessionID && st.loop != nil
	if rec, ok, err := storedSession(ctx, st, id); err != nil {
		return err
	} else if ok && rec.EndedAt == nil && !live {
		return fmt.Errorf("session %s is running elsewhere", id)
	}
	if !live { // the same conversation keeps its cost and undo log
		// The last conversation's background tasks end before its logins are
		// reset, so none of them can log in into the one resumed.
		st.endBackground()
		releaseConversation(st)
		st.fresh()
	}
	if err := rebuildFrom(st, id, events); err != nil {
		return err
	}
	// Claimed when the first task runs, so leaving it unused claims nothing.
	if _, ok := st.store.(server.SessionResumer); ok && !live {
		st.claim, st.claimSeq = id, events[len(events)-1].Seq
	}
	return nil
}

// rebuildFrom makes the conversation session id as its record stands: the
// messages, the turn count, and a sequence that goes on from the last event.
func rebuildFrom(st *cliState, id string, events []agent.Event) error {
	msgs, err := agent.Fork(events, 0)
	// A conversation rewound to before its first prompt has none to rebuild.
	if err != nil && messaged(agent.Live(events)) {
		return err
	}
	end, _ := agent.LastEnd(events)
	loop := st.open(id, events[len(events)-1].Seq)
	loop.Recorder.Advance(events[len(events)-1].Seq)
	loop.SetHistory(msgs, end.Turns)
	loop.CarryUsage(end)
	// The allowance goes on from what the session spent, and results the
	// record owes the conversation arrive at the next task's first boundary.
	loop.Budget.Carry(agent.CarriedSpend(events))
	loop.QueueNotices(agent.PendingNotices(events, st.store.Events))
	// Undo and rewind go on from the checkpoints the record holds.
	rebuildUndo(st, events)
	return nil
}

// recordMove records that a resumed session continues on another model, once
// its first task has claimed it, so the record and a later resume agree.
func recordMove(st *cliState) error {
	if st.moved == nil || st.loop == nil {
		return nil
	}
	// Kept until written: a refused write refuses the task, and so the next one too.
	if _, err := st.loop.Recorder.Record(agent.EvModelSwitched, agent.ActorUser, agent.Trusted, *st.moved); err != nil {
		return err
	}
	st.moved = nil
	return nil
}

// storedSession reads session id's row, where the store keeps rows.
func storedSession(ctx context.Context, st *cliState, id string) (store.SessionRecord, bool, error) {
	getter, ok := st.store.(interface {
		GetSession(context.Context, string) (store.SessionRecord, error)
	})
	if !ok {
		return store.SessionRecord{}, false, nil
	}
	rec, err := getter.GetSession(ctx, id)
	return rec, err == nil, err
}

// ownedHere refuses a session recorded for another user or tenant. A CLI
// subagent's row is recorded as store.SubagentUser, and is owned by whoever
// owns the session that started it; a parent that cannot be found owns nothing.
func ownedHere(ctx context.Context, st *cliState, id string) error {
	rec, ok, err := storedSession(ctx, st, id)
	if err != nil || !ok {
		return err
	}
	tenant := cliTenant(st.appCfg)
	owner := rec
	for hops := 0; owner.User == store.SubagentUser && owner.ParentID != "" && owner.Tenant == tenant && hops < 16; hops++ {
		parent, found, err := storedSession(ctx, st, owner.ParentID)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return fmt.Errorf("could not check who owns session %s: %w", id, err)
		}
		if !found {
			break
		}
		owner = parent
	}
	// A subagent's row whose chain ends without a person owns nothing, even
	// for a user who happens to be named like the subagent rows are.
	// A session the owner migration moved to the same-named account is still this user's.
	mine := owner.User == cliUser() || owner.User == auth.LocalOwner(cliUser())
	// An unclaimed or nobody row is no one's, whatever $USER says.
	if ownsNoOne(owner.User) || ownsNoOne(rec.User) {
		mine = false
	}
	if owner.User == store.SubagentUser || !mine || rec.Tenant != tenant || owner.Tenant != tenant {
		return fmt.Errorf("session %s belongs to another user", id)
	}
	return nil
}

// ownsNoOne reports whether owner is an unclaimed or nobody key, in any case.
func ownsNoOne(owner string) bool {
	o := strings.ToLower(strings.TrimSpace(owner))
	return strings.HasPrefix(o, auth.UnclaimedPrefix) || strings.HasPrefix(o, auth.NobodyPrefix)
}

// claimResumed claims a resumed session as its first task starts: only one
// process continues a session. On refusal the conversation is dropped.
func claimResumed(ctx context.Context, st *cliState) error {
	id := st.claim
	if id == "" {
		return nil
	}
	st.claim = ""
	// A session whose end waits on this process's own background tasks was
	// never let go: it is still this process's, and is not claimed again.
	if st.loop != nil && st.sessionID == id && st.liveTasks() > 0 {
		return nil
	}
	claimed, err := st.store.(server.SessionResumer).ClaimResume(ctx, id)
	if err == nil && !claimed {
		err = fmt.Errorf("session %s is running elsewhere", id)
	}
	if err != nil {
		st.loop, st.sessionID = nil, ""
		return err
	}
	// Another process may have continued it since /resume read the record.
	events, err := st.store.Events(id)
	if err == nil && len(events) > 0 && events[len(events)-1].Seq != st.claimSeq {
		err = rebuildFrom(st, id, events)
	}
	if err != nil {
		releaseClaim(st.store, id, events)
		st.loop, st.sessionID = nil, ""
	}
	return err
}

// claimForWrite claims the session for a command that writes to its record
// outside a task; release leaves the row as it was found, ended as before.
func claimForWrite(ctx context.Context, st *cliState) (release func(), err error) {
	claimed := st.claim != ""
	if err := claimResumed(ctx, st); err != nil {
		return nil, err
	}
	return func() {
		if claimed && st.loop != nil {
			// The local record's claim is this process's lock, not a row to
			// release with another end; the claim is only handed back.
			if rec, ok := st.store.(*local.Store); ok {
				rec.Unclaim(st.sessionID)
			} else {
				endAsBefore(st)
			}
			holdUntilNextWrite(st)
		}
	}, nil
}

// endAsBefore records the session's last end again, reason and totals, so a
// row claimed only to write a fork or a compaction is released unchanged.
func endAsBefore(st *cliState) {
	end := json.RawMessage(nil)
	if events, err := st.store.Events(st.sessionID); err == nil {
		for _, ev := range events {
			if ev.Type == agent.EvSessionEnded {
				end = ev.Payload
			}
		}
	}
	if end == nil {
		end, _ = json.Marshal(agent.SessionEnded{Reason: agent.TermCompleted, Turns: st.loop.Usage().Turns})
	}
	_, _ = st.loop.Recorder.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, end)
}

// settleTurn closes a task: a failed one is ended if its end is missing, and
// the conversation is claimed again before its next write.
func settleTurn(st *cliState, runErr error) {
	if runErr != nil {
		endIfOpen(st, agent.TermError)
	}
	holdUntilNextWrite(st)
}

// releaseRefused hands back the claim a task took when a hook refused its
// message: the run never started, so nothing ended it, and the next task
// could not claim the session again.
func releaseRefused(st *cliState, reason agent.TerminalReason) {
	if reason != agent.TermPromptRefused || st.loop == nil {
		return
	}
	if _, durable := st.store.(server.SessionResumer); !durable {
		return
	}
	if rec, ok := st.store.(*local.Store); ok {
		rec.Unclaim(st.sessionID)
		return
	}
	endAsBefore(st)
}

// holdUntilNextWrite marks the conversation to be claimed again before its
// next write: the task's own end released it, and another process may claim it.
func holdUntilNextWrite(st *cliState) {
	if _, durable := st.store.(server.SessionResumer); durable && st.loop != nil {
		st.claim, st.claimSeq = st.sessionID, st.loop.Recorder.LastAppended()
	}
}

// releaseClaim ends a claimed session in its record, releasing its row; with the
// store unreadable as well its write at seq 1 is refused, as in an outage.
func releaseClaim(es server.EventStore, id string, events []agent.Event) {
	rec := agent.NewRecorder(es, id, "")
	if len(events) > 0 {
		rec.Advance(events[len(events)-1].Seq)
	} else if all, err := es.Events(id); err == nil && len(all) > 0 {
		rec.Advance(all[len(all)-1].Seq)
	}
	_, _ = rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermError})
}

// endIfOpen records a session.ended for reason when a turn stopped without
// one, so a claimed session is released rather than left running.
func endIfOpen(st *cliState, reason agent.TerminalReason) {
	if st.loop == nil {
		return
	}
	// Only a record whose last event is this process's own, and not already an
	// end: after a failed write it may be another process's run.
	events, err := st.store.Events(st.sessionID)
	if err != nil || len(events) == 0 {
		return
	}
	if last := events[len(events)-1]; last.Type == agent.EvSessionEnded || last.Seq != st.loop.Recorder.LastAppended() {
		return
	}
	_, _ = st.loop.Recorder.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: reason})
}

// messaged reports whether the record holds a message from anyone.
func messaged(events []agent.Event) bool {
	for _, ev := range events {
		if ev.Type == agent.EvUserMessage {
			return true
		}
	}
	return false
}

func mustCwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// openStore selects the event store: Postgres when the configuration names
// it, and otherwise the local record, so sessions survive the process and
// can be listed, resumed, branched, rewound and verified without a database.
func openStore(ctx context.Context, cfg config.Config) (server.EventStore, func(), error) {
	if cfg.Storage.Driver != "postgres" {
		rec, err := openRecord(cfg)
		if err != nil {
			return nil, nil, err
		}
		return rec, func() { _ = rec.Close() }, nil
	}
	pg, err := store.Open(ctx, storeConfig(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("open event store: %w", err)
	}
	return pg, pg.Close, nil
}

// openServeStore is serve's event store: Postgres when configured, and
// otherwise memory. The local record belongs to the person at the command line.
func openServeStore(ctx context.Context, cfg config.Config) (server.EventStore, func(), error) {
	if cfg.Storage.Driver != "postgres" {
		return agent.NewMemStore(), func() {}, nil
	}
	return openStore(ctx, cfg)
}

// serveStorageLabel names serve's store, as openServeStore chooses it.
func serveStorageLabel(cfg config.Config) string {
	if cfg.Storage.Driver != "postgres" {
		return "memory (sessions do not survive restart)"
	}
	return storageLabel(cfg)
}

// openRecord opens the local record the configuration names: the managed
// record.dir, or ~/.abhed/records, for this tenant and user, redacting with
// the secrets store, read again as it changes, before anything is written. A managed
// record.retention_days prunes what is older, leaving tombstones.
func openRecord(cfg config.Config) (*local.Store, error) {
	rec, err := local.Open(local.Options{Dir: cfg.Record.Dir, Tenant: cliTenant(cfg), User: cliUser(), Redact: openVault().Session()})
	if err != nil {
		return nil, fmt.Errorf("open the local record: %w", err)
	}
	if days := cfg.Record.RetentionDays; days > 0 && cfg.ManagedSets("record.retention_days") {
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		pruned, _, err := rec.PruneOlder(cutoff, "managed", fmt.Sprintf("record.retention_days is %d", days))
		// Each has a tombstone in the index; the person is told as well.
		for _, p := range pruned {
			fmt.Fprintf(os.Stderr, "abhed: pruned session %s under record.retention_days (%d days); a tombstone keeps its head\n", p.ID, days)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: the record's retention could not be applied: %v\n", err)
		}
	}
	return rec, nil
}

func storageLabel(cfg config.Config) string {
	if cfg.Storage.Driver == "postgres" {
		// Only what configuration says. Whether the record is protected is a
		// fact about the connection, and doctor reports it after asking.
		label := "postgres (durable, tenant=" + orDefault(cfg.Storage.Tenant, "default")
		if cfg.Storage.SingleRole {
			label += "; single_role: the server's role can alter the record"
		}
		return label + ")"
	}
	dir := cfg.Record.Dir
	if dir == "" {
		dir = "~/.abhed/records"
	}
	return "local record, chained (" + dir + "; abhed record verify)"
}

// insideDir reports whether path's folder is dir or under it, by real
// paths; where the name itself leads is openExport's to judge.
func insideDir(path, dir string) bool {
	where := filepath.Join(tools.RealPath(filepath.Dir(path)), filepath.Base(path))
	rel, err := filepath.Rel(tools.RealPath(dir), where)
	return err == nil && filepath.IsLocal(rel)
}

// releaseConversation lets go of the conversation this process was
// writing, so another Abhed process may continue it; its record stays.
func releaseConversation(st *cliState) {
	if rec, ok := st.store.(*local.Store); ok && st.sessionID != "" {
		_ = rec.Release(st.sessionID)
	}
}

func storeConfig(cfg config.Config) store.Config {
	sc := store.DefaultConfig(cfg.Storage.DSN)
	sc.SingleRole = cfg.Storage.SingleRole
	sc.Owners = ownerPolicy(cfg)
	if cfg.Storage.SingleRole {
		// A single-role start migrates owners itself, so it reads the
		// users_file too; a configured one it cannot read stops the start.
		sc.OwnerAccounts = func() ([]*auth.User, error) {
			users, _, err := fileOwnerAccounts(cfg, cfg.Workspace.Workspace, false)
			return users, err
		}
	}
	if cfg.Storage.Tenant != "" {
		sc.Tenant = cfg.Storage.Tenant
	}
	if cfg.Storage.MaxConns > 0 {
		sc.MaxConns = int32(cfg.Storage.MaxConns)
	}
	return sc
}
