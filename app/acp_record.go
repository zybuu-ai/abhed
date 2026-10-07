package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// Sessions and the record (docs/architecture/studio-acp-contract.md §4): the
// local, hash-chained record the CLI writes, read and continued from Studio.

func init() {
	recordFeatures = append(recordFeatures, "sessions", "fork", "events", "record.verify", "hawkeye", "export")
	handle(map[string]func(*acpConn, rpcMessage){
		"session/list":           (*acpConn).listSessions,
		"session/load":           func(c *acpConn, m rpcMessage) { c.loadSession(m, true) },
		"session/resume":         func(c *acpConn, m rpcMessage) { c.loadSession(m, false) },
		"session/close":          (*acpConn).closeSession,
		"_abhed/session/rename":  (*acpConn).renameSession,
		"_abhed/session/fork":    (*acpConn).forkSession,
		"_abhed/session/compact": (*acpConn).compactSession,
		"_abhed/events/subscribe": func(c *acpConn, m rpcMessage) {
			c.subscribe(m)
		},
		"_abhed/events/unsubscribe": (*acpConn).unsubscribe,
		"_abhed/events/page":        (*acpConn).eventsPage,
		"_abhed/record/verify":      (*acpConn).verifyRecord,
		"_abhed/export":             (*acpConn).exportRecord,
		"_abhed/hawkeye":            (*acpConn).hawkeyeReport,
	})
}

// listPage is how many sessions one session/list reply holds.
const listPage = 50

// recorded finds a session in the record that belongs to this engine's user:
// one of theirs, or a subagent's session one of theirs started. Any other id,
// another user's included, is an unknown session (§2.8).
func (c *acpConn) recorded(id string) (*local.Store, local.Entry, *rpcError) {
	rec, err := c.record()
	if err != nil {
		return nil, local.Entry{}, refusal(errRecord, "the record is unavailable: %v", err)
	}
	if id == "" {
		return nil, local.Entry{}, refusal(errParams, "unknown session")
	}
	e, err := rec.Index().Get(id)
	if err != nil || e.Pruned {
		return nil, local.Entry{}, refusal(errParams, "unknown session")
	}
	owner := e
	for depth := 0; owner.Subagent && depth < 16; depth++ {
		if owner, err = rec.Index().Get(owner.Parent); err != nil {
			return nil, local.Entry{}, refusal(errParams, "unknown session")
		}
	}
	if owner.Subagent || owner.User != c.recUser {
		return nil, local.Entry{}, refusal(errParams, "unknown session")
	}
	return rec, e, nil
}

func (c *acpConn) listSessions(msg rpcMessage) {
	var p struct {
		Cwd    string `json:"cwd"`
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	rec, err := c.record()
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRecord, "the record is unavailable: %v", err))
		return
	}
	f := local.Filter{All: true}
	if p.Cwd != "" {
		f = local.Filter{Cwd: p.Cwd}
	}
	entries, err := rec.Index().List(f)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRecord, "the record's index: %v", err))
		return
	}
	var mine []local.Entry
	for _, e := range entries {
		if e.User == c.recUser && !e.Subagent && !e.Pruned {
			mine = append(mine, e)
		}
	}
	start := 0
	if p.Cursor != "" {
		if start, err = strconv.Atoi(p.Cursor); err != nil || start < 0 || start > len(mine) {
			c.reply(msg.ID, nil, refusal(errParams, "unknown cursor"))
			return
		}
	}
	end := min(start+listPage, len(mine))
	infos := make([]any, 0, end-start)
	for _, e := range mine[start:end] {
		infos = append(infos, c.sessionInfo(rec, e))
	}
	res := map[string]any{"sessions": infos}
	if end < len(mine) {
		res["nextCursor"] = strconv.Itoa(end)
	}
	c.reply(msg.ID, res, nil)
}

// sessionInfo is a SessionInfo: the spec's fields and Abhed's in _meta.
func (c *acpConn) sessionInfo(rec *local.Store, e local.Entry) map[string]any {
	info := map[string]any{"sessionId": e.ID, "cwd": e.Cwd}
	if e.Title != "" {
		info["title"] = ui.VisibleLine(e.Title)
	}
	if !e.Updated.IsZero() {
		info["updatedAt"] = e.Updated.UTC().Format(time.RFC3339)
	}
	meta := map[string]any{
		"created": e.Created.UTC().Format(time.RFC3339), "events": e.Head.Lines,
		"head":       map[string]any{"seq": e.Head.Seq, "hash": e.Head.Hash},
		"background": 0, "waitingAsks": 0, "status": "idle",
	}
	if e.Name != "" {
		meta["name"] = ui.VisibleLine(e.Name)
	}
	if e.Parent != "" {
		meta["parent"], meta["forkSeq"] = e.Parent, e.ForkSeq
	}
	if e.GitBranch != "" {
		meta["gitBranch"] = ui.VisibleLine(e.GitBranch)
	}
	if e.Ended != "" {
		meta["status"], meta["endReason"] = "ended", e.Ended
	}
	if s := c.session(e.ID); s != nil {
		meta["status"] = "idle"
		if idle(s) != nil {
			meta["status"] = "live"
		}
		meta["background"], meta["waitingAsks"] = s.liveTasks(), s.waitingAll()
		if s.inner {
			meta["mode"] = string(s.parts.Loop.Policy.Mode)
		}
	} else if row, err := rec.GetSession(context.Background(), e.ID); err == nil && row.EndedAt == nil && !rec.Held(e.ID) {
		meta["status"], meta["openElsewhere"] = "live", true
	}
	info["_meta"] = map[string]any{acpMetaKey: meta}
	return info
}

// loadSession is session/load (replay first) and session/resume (no replay):
// the chain is verified, the session continued in this process, and its
// model, mode and "always" scopes restored as today's policy allows.
func (c *acpConn) loadSession(msg rpcMessage, replay bool) {
	var p struct {
		SessionID string                     `json:"sessionId"`
		Cwd       string                     `json:"cwd"`
		Meta      map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		c.reply(msg.ID, nil, refusal(errParams, "%v", err))
		return
	}
	trust, terr := c.requestedTrust(p.Meta)
	if terr != nil {
		c.reply(msg.ID, nil, terr)
		return
	}
	if open := c.session(p.SessionID); open != nil {
		// Its agent already holds the wider configuration; narrowing it in place is not possible.
		if trust == config.TrustRefused && open.trust != config.TrustRefused {
			c.reply(msg.ID, nil, refusal(errRefused, "this session is open on this connection with the workspace's stored trust; "+
				"close it, then load it again untrusted"))
			return
		}
		c.reply(msg.ID, nil, refusal(errRefused, "this session is already open on this connection"))
		return
	}
	rec, e, rerr := c.recorded(p.SessionID)
	if rerr != nil {
		c.reply(msg.ID, nil, rerr)
		return
	}
	if e.Subagent {
		c.reply(msg.ID, nil, refusal(errRefused, "a subagent's session goes on only through the session that started it"))
		return
	}
	rep, err := rec.Verify(e.ID)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRecord, "the record could not be verified: %v", err))
		return
	}
	events, err := rec.Events(e.ID)
	if err != nil || len(events) == 0 {
		c.reply(msg.ID, nil, refusal(errRecord, "the record could not be read: %v", err))
		return
	}
	cwd := e.Cwd
	if cwd == "" {
		cwd = p.Cwd
	}
	var s *acpSession
	if rep.OK {
		var e2 *rpcError
		if s, e2 = c.openSession(openOptions{cwd: cwd, trust: trust, id: e.ID, resume: true}); e2 != nil {
			c.reply(msg.ID, nil, e2)
			return
		}
		if err := c.restore(s, rec, events); err != nil {
			c.dropSession(s)
			c.reply(msg.ID, nil, refusal(errRecord, "the conversation could not be rebuilt: %v", err))
			return
		}
	} else {
		// Read, never written to: going on from it is a fork.
		s = &acpSession{id: e.ID, cwd: cwd, trust: trust, always: map[string]bool{}, agent: recordOnly{},
			readOnly: "the record failed verification at seq " + strconv.FormatInt(rep.FirstBad, 10)}
		c.sessMu.Lock()
		c.sessions[s.id] = s
		c.sessMu.Unlock()
	}
	if replay {
		c.replay(s, events)
	}
	res := c.sessionResult(s)
	delete(res, "sessionId")
	verified := map[string]any{"verified": rep.OK, "head": map[string]any{"seq": rep.Head.Seq, "hash": rep.Head.Hash}}
	if !rep.OK {
		verified["firstBad"] = map[string]any{"seq": rep.FirstBad, "reason": rep.Reason}
	}
	metaOf(res)["record"] = verified
	cmds := commandsUpdate(s)
	c.reply(msg.ID, res, nil)
	c.sessionUpdate(s.id, cmds)
}

// recordOnly stands in for the agent of a session whose record failed
// verification: it is shown, never continued.
type recordOnly struct{}

func (recordOnly) Run(context.Context, string) (string, error) {
	return "", errors.New("the record failed verification; fork it to go on")
}
func (recordOnly) Steer(string)                {}
func (recordOnly) Flush(context.Context) error { return nil }
func (recordOnly) CancelTasks() int            { return 0 }
func (recordOnly) Close()                      {}

// restore rebuilds a continued session from its record, as the CLI's resume does.
func (c *acpConn) restore(s *acpSession, rec *local.Store, events []agent.Event) error {
	if !s.inner {
		return nil
	}
	loop := s.parts.Loop
	msgs, err := agent.Fork(events, 0)
	// A conversation rewound to before its first prompt has none to rebuild.
	if err != nil && messaged(agent.Live(events)) {
		return err
	}
	var last int64
	for _, ev := range events {
		last = max(last, ev.Seq)
	}
	end, _ := agent.LastEnd(events)
	loop.Recorder.Advance(last)
	loop.SetHistory(msgs, end.Turns)
	loop.CarryUsage(end)
	if loop.Budget != nil {
		loop.Budget.Carry(agent.CarriedSpend(events))
	}
	loop.QueueNotices(agent.PendingNotices(events, rec.Events))
	s.undo.Rebuild(events, rec.Blobs().Get)
	var model, mode string
	for _, ev := range agent.Live(events) {
		switch ev.Type {
		case agent.EvModelSwitched:
			var m agent.ModelSwitched
			if json.Unmarshal(ev.Payload, &m) == nil && m.Provider != "" {
				model = m.Provider
			}
		case agent.EvModeChanged:
			var m agent.ModeChanged
			if json.Unmarshal(ev.Payload, &m) == nil {
				mode = m.To
			}
		case agent.EvApprovalScopeGranted:
			// Only asks consult these, after deny rules and the destructive
			// check, so a scope cannot outlive a rule that now refuses it.
			var g agent.ScopeGranted
			if json.Unmarshal(ev.Payload, &g) == nil && g.Scope != "" {
				if _, err := policy.ParseRule(g.Scope); err == nil {
					s.always[g.Scope] = true
				}
			}
		}
	}
	if m, ok := s.agent.(modelSwitcher); ok && model != "" {
		_ = m.SwitchModelNamed(model) // a model no longer configured stays the default
	}
	// The mode goes back within today's ceiling; a mode now refused stays as configured.
	if mode != "" && mode != string(loop.Policy.Mode) && mode != string(policy.ModeBypass) {
		if _, err := s.parts.Config.Apply(config.Overrides{Mode: mode}); err == nil {
			loop.Policy.Mode = policy.Mode(mode)
		}
	}
	return nil
}

// dropSession forgets a session this connection opened and lets it go.
func (c *acpConn) dropSession(s *acpSession) {
	c.sessMu.Lock()
	delete(c.sessions, s.id)
	c.sessMu.Unlock()
	c.shut(s)
}

func (c *acpConn) closeSession(msg rpcMessage) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.dropSession(s)
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) renameSession(msg rpcMessage) {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 200 || strings.ContainsAny(name, "\n\r") {
		c.reply(msg.ID, nil, refusal(errParams, "a name is one line of at most 200 characters"))
		return
	}
	if _, e := innerOf(s); e != nil || s.readOnly != "" {
		c.reply(msg.ID, nil, refusal(errRefused, "this session cannot be renamed"))
		return
	}
	s.record(agent.EvSessionNamed, agent.SessionNamed{Name: name})
	c.reply(msg.ID, map[string]any{}, nil)
}

// forkSession copies a session's conversation through a seq into a new
// session, recorded session.branched in the new one, and opens it here.
func (c *acpConn) forkSession(msg rpcMessage) {
	var p struct {
		SessionID  string                     `json:"sessionId"`
		ThroughSeq int64                      `json:"throughSeq"`
		Meta       map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		c.reply(msg.ID, nil, refusal(errParams, "%v", err))
		return
	}
	trust, terr := c.requestedTrust(p.Meta)
	if terr != nil {
		c.reply(msg.ID, nil, terr)
		return
	}
	rec, e, rerr := c.recorded(p.SessionID)
	if rerr != nil {
		c.reply(msg.ID, nil, rerr)
		return
	}
	if e.Subagent {
		c.reply(msg.ID, nil, refusal(errRefused, "a subagent's session cannot be forked on its own"))
		return
	}
	// A fork rewrites the conversation and ends its logins: never under a run.
	if s := c.session(e.ID); s != nil {
		if err := idle(s); err != nil || s.liveTasks() > 0 {
			c.reply(msg.ID, nil, refusal(errBusy, "a prompt or a background task is running in this session; fork once it ends"))
			return
		}
	}
	rep, err := rec.Verify(e.ID)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRecord, "the record could not be verified: %v", err))
		return
	}
	events, err := rec.Events(e.ID)
	if err != nil || len(events) == 0 {
		c.reply(msg.ID, nil, refusal(errRecord, "nothing to fork"))
		return
	}
	if p.ThroughSeq < 0 || (p.ThroughSeq > 0 && p.ThroughSeq > events[len(events)-1].Seq) {
		c.reply(msg.ID, nil, refusal(errParams, "throughSeq is past the end of the record"))
		return
	}
	unverified := ""
	if !rep.OK {
		unverified = unverifiedReason(rep)
	}
	id, through, err := c.copyInto(rec, e, events, p.ThroughSeq, unverified)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRecord, "the fork could not be written: %v", err))
		return
	}
	s, oerr := c.openSession(openOptions{cwd: e.Cwd, trust: trust, id: id, resume: true})
	if oerr != nil {
		c.reply(msg.ID, nil, oerr)
		return
	}
	all, _ := rec.Events(id)
	if err := c.restore(s, rec, all); err != nil {
		c.dropSession(s)
		c.reply(msg.ID, nil, refusal(errRecord, "the fork could not be rebuilt: %v", err))
		return
	}
	res := c.sessionResult(s)
	res["parent"], res["forkSeq"] = e.ID, through
	cmds := commandsUpdate(s)
	c.reply(msg.ID, res, nil)
	c.sessionUpdate(s.id, cmds)
}

// copyInto writes a new session holding the source's conversation through
// seq, as the CLI's branch does; a source that failed verification is copied
// as untrusted and says so.
func (c *acpConn) copyInto(rec *local.Store, src local.Entry, events []agent.Event, through int64, unverified string) (string, int64, error) {
	id := newConversationID()
	copied := agent.BranchCopy(events, through, id, 2)
	if len(copied) == 0 {
		return "", 0, errors.New("nothing to fork")
	}
	last := through
	if last == 0 {
		for _, ev := range events {
			last = max(last, ev.Seq)
		}
	}
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, User: c.recUser,
		Workspace: src.Cwd, StartedAt: time.Now().UTC()}); err != nil {
		return "", 0, err
	}
	r := agent.NewRecorder(rec, id, "")
	r.Redact = openVault().Redactor()
	if _, err := r.Record(agent.EvSessionBranched, agent.ActorUser, agent.Trusted,
		agent.SessionBranched{From: src.ID, ThroughSeq: last, Unverified: unverified}); err != nil {
		return "", 0, err
	}
	for _, ev := range copied {
		if unverified != "" {
			ev.Trust = agent.Untrusted
		}
		if err := rec.Append(ev); err != nil {
			return "", 0, err
		}
	}
	// Handed back at rest, so this process's resume can claim it.
	rec.Unclaim(id)
	return id, last, nil
}

func (c *acpConn) compactSession(msg rpcMessage) {
	var p struct {
		Focus string `json:"focus"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e == nil {
		e = idle(s)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if strings.TrimSpace(p.Focus) != "" {
		c.reply(msg.ID, nil, refusal(errRefused, "a compaction focus is not supported by this engine yet"))
		return
	}
	info, err := compactRecorded(c.root(), s, parts.Loop)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the conversation could not be compacted: %v", err))
		return
	}
	c.reply(msg.ID, map[string]any{"before": info.BeforeTokens, "after": info.AfterTokens}, nil)
}

// recordEvent is a RecordEvent: an event as its line in the record holds it.
func recordEvent(ev local.Sealed) map[string]any {
	trust := "trusted"
	if ev.Trust == agent.Untrusted {
		trust = "untrusted"
	}
	return map[string]any{"seq": ev.Seq, "id": ev.ID, "ts": ev.CreatedAt.UTC().Format(time.RFC3339Nano),
		"type": string(ev.Type), "actor": string(ev.Actor), "trust": trust, "session": ev.SessionID,
		"payload": ev.Payload, "prev": ev.Prev, "hash": ev.Hash}
}

// maxSubscriptions bounds the event streams on one session.
const maxSubscriptions = 8

// eventSub is one event stream: a session's record as it grows.
type eventSub struct {
	id      string
	session string
	stop    chan struct{}
}

func (c *acpConn) subscribe(msg rpcMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		AfterSeq  int64  `json:"afterSeq"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	rec, e, rerr := c.recorded(p.SessionID)
	if rerr != nil {
		c.reply(msg.ID, nil, rerr)
		return
	}
	c.subMu.Lock()
	if c.subs == nil {
		c.subs = map[string]*eventSub{}
	}
	n := 0
	for _, s := range c.subs {
		if s.session == e.ID {
			n++
		}
	}
	if n >= maxSubscriptions {
		c.subMu.Unlock()
		c.reply(msg.ID, nil, refusal(errRefused, "this session already has %d event streams", maxSubscriptions))
		return
	}
	sub := &eventSub{id: "sub-" + acpID(), session: e.ID, stop: make(chan struct{})}
	c.subs[sub.id] = sub
	c.subMu.Unlock()
	// Taken before the backlog is read, so nothing written between is missed.
	ch := rec.Subscribe(e.ID)
	seq, hash, _ := rec.Index().Head(e.ID)
	c.reply(msg.ID, map[string]any{"head": map[string]any{"seq": seq, "hash": hash}, "subscription": sub.id}, nil)
	go c.stream(rec, sub, ch, p.AfterSeq)
}

// stream sends the backlog after seq, then each event as it is recorded, in
// seq order. A wake from the store is a cue to read what is new from the file.
func (c *acpConn) stream(rec *local.Store, sub *eventSub, ch <-chan agent.Event, after int64) {
	defer rec.Unsubscribe(sub.session, ch)
	send := func() bool {
		lines, err := rec.SealedSince(sub.session, after)
		if err != nil {
			return false
		}
		for _, l := range lines {
			select {
			case <-sub.stop:
				return false
			default:
			}
			c.notification("_abhed/event", map[string]any{"subscription": sub.id, "sessionId": sub.session, "event": recordEvent(l)})
			after = l.Seq
		}
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-sub.stop:
			return
		case _, ok := <-ch:
			if !ok || !send() {
				return
			}
		}
	}
}

func (c *acpConn) unsubscribe(msg rpcMessage) {
	var p struct {
		Subscription string `json:"subscription"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	c.subMu.Lock()
	sub := c.subs[p.Subscription]
	delete(c.subs, p.Subscription)
	c.subMu.Unlock()
	if sub == nil {
		c.reply(msg.ID, nil, refusal(errParams, "unknown subscription"))
		return
	}
	close(sub.stop)
	c.reply(msg.ID, map[string]any{}, nil)
}

// unsubscribeSession ends the streams of a session that closed.
func (c *acpConn) unsubscribeSession(id string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for key, sub := range c.subs {
		if sub.session == id {
			close(sub.stop)
			delete(c.subs, key)
		}
	}
}

// pageLimit bounds one events page.
const pageLimit = 500

func (c *acpConn) eventsPage(msg rpcMessage) {
	var p struct {
		SessionID string   `json:"sessionId"`
		AfterSeq  int64    `json:"afterSeq"`
		Limit     int      `json:"limit"`
		Types     []string `json:"types"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	rec, e, rerr := c.recorded(p.SessionID)
	if rerr != nil {
		c.reply(msg.ID, nil, rerr)
		return
	}
	if p.Limit <= 0 || p.Limit > pageLimit {
		p.Limit = pageLimit
	}
	lines, err := rec.SealedSince(e.ID, p.AfterSeq)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRecord, "%v", err))
		return
	}
	want := map[string]bool{}
	for _, t := range p.Types {
		want[t] = true
	}
	out := []any{}
	more := false
	for _, l := range lines {
		if len(want) > 0 && !want[string(l.Type)] {
			continue
		}
		if len(out) == p.Limit {
			more = true
			break
		}
		out = append(out, recordEvent(l))
	}
	seq, hash, _ := rec.Index().Head(e.ID)
	c.reply(msg.ID, map[string]any{"events": out, "head": map[string]any{"seq": seq, "hash": hash}, "more": more}, nil)
}

func (c *acpConn) verifyRecord(msg rpcMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	rec, e, rerr := c.recorded(p.SessionID)
	if rerr != nil {
		c.reply(msg.ID, nil, rerr)
		return
	}
	rep, err := rec.Verify(e.ID)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRecord, "the record could not be verified: %v", err))
		return
	}
	res := map[string]any{"ok": rep.OK, "events": rep.Events, "head": map[string]any{"seq": rep.Head.Seq, "hash": rep.Head.Hash},
		"store": map[string]any{"store": "local", "dir": rec.Dir()}}
	if !rep.OK {
		res["firstBad"] = map[string]any{"seq": rep.FirstBad, "reason": rep.Reason}
	}
	c.reply(msg.ID, res, nil)
}

// exportRecord writes a session under ~/.abhed/exports, never the workspace:
// JSONL that keeps the chain, or the HawkEYE page. Studio copies it from there.
func (c *acpConn) exportRecord(msg rpcMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Format    string `json:"format"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if p.Format != "jsonl" && p.Format != "html" {
		c.reply(msg.ID, nil, refusal(errParams, `format is "jsonl" or "html"`))
		return
	}
	rec, e, rerr := c.recorded(p.SessionID)
	if rerr != nil {
		c.reply(msg.ID, nil, rerr)
		return
	}
	path, err := exportPath("", e.ID, p.Format)
	if err == nil {
		switch p.Format {
		case "jsonl":
			_, err = exportSession(rec, e, "jsonl", path, io.Discard, false)
		default:
			err = writeHawkeyeExport(rec, e, path)
		}
	}
	if err != nil {
		code := errRefused
		var unv *local.UnverifiedError
		if errors.As(err, &unv) {
			code = errRecord
		}
		c.reply(msg.ID, nil, refusal(code, "the export failed: %v", err))
		return
	}
	data, err := os.ReadFile(path) // #nosec G304 -- a file this call just wrote under ~/.abhed/exports
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the export could not be read back: %v", err))
		return
	}
	sum := sha256.Sum256(data)
	seq, hash, _ := rec.Index().Head(e.ID)
	c.reply(msg.ID, map[string]any{"path": path, "bytes": len(data), "sha256": hex.EncodeToString(sum[:]),
		"head": map[string]any{"seq": seq, "hash": hash}}, nil)
}

func writeHawkeyeExport(rec *local.Store, e local.Entry, path string) error {
	page, err := hawkeye.HTML(hawkeyeOf(rec, e))
	if err != nil {
		return err
	}
	f, err := openExport(path)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(f, page); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// hawkeyeOf is the HawkEYE report over a session's durable record.
func hawkeyeOf(rec *local.Store, e local.Entry) hawkeye.Report {
	events, _ := rec.Events(e.ID)
	return hawkeye.AnalyzeWith(e.ID, events, hawkeye.Options{Live: rec.Held(e.ID)})
}

func (c *acpConn) hawkeyeReport(msg rpcMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Format    string `json:"format"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	rec, e, rerr := c.recorded(p.SessionID)
	if rerr != nil {
		c.reply(msg.ID, nil, rerr)
		return
	}
	rep := hawkeyeOf(rec, e)
	switch p.Format {
	case "", "json":
		c.reply(msg.ID, rep, nil)
	case "html":
		page, err := hawkeye.HTML(rep)
		if err != nil {
			c.reply(msg.ID, nil, refusal(errRefused, "the report could not be rendered: %v", err))
			return
		}
		c.reply(msg.ID, map[string]any{"html": page}, nil)
	default:
		c.reply(msg.ID, nil, refusal(errParams, `format is "json" or "html"`))
	}
}

// restartForTrust rebuilds a session's agent from its record, so a changed
// workspace file is decided about again. A session kept only in memory says
// so instead: its conversation would not survive the restart.
func (c *acpConn) restartForTrust(s *acpSession) {
	say := func(t string) {
		c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(t)})
	}
	if !c.durable() || !s.inner {
		say("The workspace's configuration file changed during this session; start a new session for the change to be decided about.\n")
		return
	}
	rec, err := c.record()
	if err != nil {
		return
	}
	s.agent.Close()
	if err := c.buildAgent(s, openOptions{cwd: s.cwd, trust: s.trust, id: s.id, resume: true}); err != nil {
		s.readOnly = "the session could not restart after its workspace file changed: " + err.Message
		say(s.readOnly + "\n")
		return
	}
	events, _ := rec.Events(s.id)
	if err := c.restore(s, rec, events); err != nil {
		s.readOnly = "the session could not restart after its workspace file changed"
	}
	say("The workspace's configuration file changed; the session restarted under the decision about its new content.\n")
}
