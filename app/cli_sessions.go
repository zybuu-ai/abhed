package app

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
)

// slashSessions is /sessions.
func slashSessions(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
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
	for _, rec := range records {
		state := "running"
		if rec.EndedAt != nil {
			state = rec.TerminalReason
		}
		fmt.Printf("  %-22s %-10s %s  %s\n", rec.ID, state,
			rec.StartedAt.Format("2006-01-02 15:04"), s.Dim(rec.User))
	}
	return false
}

// slashResume is /resume.
func slashResume(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if len(fields) < 2 {
		fmt.Println(s.Dim("  usage: /resume <session-id>   (see /sessions)"))
		return false
	}
	events, err := st.store.Events(fields[1])
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	if len(events) == 0 {
		fmt.Printf("  %s no events for session %s\n", s.Red("✕"), fields[1])
		return false
	}
	// Another user's session is not shown here, let alone continued.
	if err := ownedHere(ctx, st, fields[1]); err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("%s\n", s.Dim(fmt.Sprintf("  replaying %d events from %s", len(events), fields[1])))
	for _, ev := range events {
		r.Event(ev)
	}
	if err := resumeConversation(ctx, st, fields[1], events); err != nil {
		fmt.Printf("  %s replayed, not continued: %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("  %s\n", s.Dim("resumed — the next thing you type continues this conversation"))
	// It continues on the model selected here, which may not be the one it last ran on.
	last, model := agent.ProviderOf(events), agent.LastModel(events)
	if p, ok := st.appCfg.Model.Providers[last]; ok && p.Model != "" {
		model = p.Model // the provider the record names, over a call it may predate
	}
	if last != st.appCfg.Model.Default && (last != "" || model != "" && model != st.adapter.Profile().Name) {
		fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("it last ran on %s and continues on %s; /model %s goes back",
			orDefault(model, last), st.adapter.Profile().Name, orDefault(last, "<provider>"))))
		st.moved = &agent.ModelSwitched{Provider: st.appCfg.Model.Default, Model: st.adapter.Profile().Name, From: model}
	}
	return false
}

// slashClear is /clear.
func slashClear(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// The next task starts a new conversation, and with it a new session.
	st.endBackground()
	st.loop, st.sessionID = nil, ""
	st.fresh()
	fmt.Println(s.Dim("  context cleared; the workspace is untouched"))
	return false
}

// slashExport is /export.
func slashExport(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// HTML by default, because a transcript that needs a parser before a
	// colleague can read it usually does not get read. `/export x.json`
	// still writes the raw events for a program.
	path := filepath.Join(sess.Root, fmt.Sprintf("abhed-session-%s.html", st.sessionID))
	if len(fields) > 1 {
		path = fields[1]
	}
	events, err := st.store.Events(st.sessionID)
	if err != nil || len(events) == 0 {
		fmt.Println(s.Dim("  no transcript to export yet"))
		return false
	}
	var data []byte
	if strings.HasSuffix(path, ".json") {
		data, err = json.MarshalIndent(events, "", "  ")
	} else {
		data = []byte(agent.ExportHTML(st.sessionID, events))
	}
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
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
		Workspace: mustCwd(), Model: provider.Model,
		Mode:      orDefault(cfg.Permissions.Mode, "default"),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: could not persist session: %v\n", err)
		return err
	}
	return nil
}

// cliTenant is the tenant the CLI records a session in.
func cliTenant(cfg config.Config) string {
	if cfg.Storage.Tenant != "" {
		return cfg.Storage.Tenant
	}
	return "default"
}

// cliUser is who the CLI records a session as.
func cliUser() string {
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "local"
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
	if err != nil && messaged(events) {
		return err
	}
	end, _ := agent.LastEnd(events)
	loop := st.open(id)
	loop.Recorder.Advance(events[len(events)-1].Seq)
	loop.SetHistory(msgs, end.Turns)
	loop.CarryUsage(end)
	// The allowance goes on from what the session spent, and results the
	// record owes the conversation arrive at the next task's first boundary.
	loop.Budget.Carry(agent.CarriedSpend(events))
	loop.QueueNotices(agent.PendingNotices(events, st.store.Events))
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

// ownedHere refuses a session recorded for another user or tenant.
func ownedHere(ctx context.Context, st *cliState, id string) error {
	rec, ok, err := storedSession(ctx, st, id)
	if err != nil || !ok {
		return err
	}
	if rec.User != cliUser() || rec.Tenant != cliTenant(st.appCfg) {
		return fmt.Errorf("session %s belongs to another user", id)
	}
	return nil
}

// claimResumed claims a resumed session as its first task starts: only one
// process continues a session. On refusal the conversation is dropped.
func claimResumed(ctx context.Context, st *cliState) error {
	id := st.claim
	if id == "" {
		return nil
	}
	st.claim = ""
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
			endAsBefore(st)
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

// openStore selects the event store. Memory is fine for a CLI session; audit
// and replay across restarts need Postgres.
func openStore(ctx context.Context, cfg config.Config) (server.EventStore, func(), error) {
	if cfg.Storage.Driver != "postgres" {
		return agent.NewMemStore(), func() {}, nil
	}
	pg, err := store.Open(ctx, storeConfig(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("open event store: %w", err)
	}
	return pg, pg.Close, nil
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
	return "memory (sessions do not survive restart)"
}

func storeConfig(cfg config.Config) store.Config {
	sc := store.DefaultConfig(cfg.Storage.DSN)
	sc.SingleRole = cfg.Storage.SingleRole
	if cfg.Storage.Tenant != "" {
		sc.Tenant = cfg.Storage.Tenant
	}
	if cfg.Storage.MaxConns > 0 {
		sc.MaxConns = int32(cfg.Storage.MaxConns)
	}
	return sc
}
