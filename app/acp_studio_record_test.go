package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// §4.1: a session is listed, closed, and loaded again from the record: the
// conversation replays with its seqs and no tool runs again; the chain is
// verified first, and the model sees the earlier turns.
func TestStudioSessionLoadReplays(t *testing.T) {
	r := newStudioRig(t, "", say("first answer"))
	id := r.open()
	if stop, _ := r.prompt(id, "remember the word apple"); stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}
	var list struct {
		Sessions []map[string]any `json:"sessions"`
	}
	r.cl.ok("session/list", map[string]any{"cwd": r.ws}, &list)
	if len(list.Sessions) != 1 || list.Sessions[0]["sessionId"] != id || list.Sessions[0]["title"] != "remember the word apple" {
		t.Fatalf("session/list: %v", list.Sessions)
	}
	if st := meta(list.Sessions[0])["status"]; st != "idle" {
		t.Fatalf("status of an open idle session: %v", st)
	}
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	r.cl.refused(errParams, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{}})

	from := r.cl.mark()
	var loaded map[string]any
	r.cl.ok("session/load", map[string]any{"sessionId": id, "cwd": r.ws, "mcpServers": []any{}}, &loaded)
	rec := meta(loaded)["record"].(map[string]any)
	if rec["verified"] != true {
		t.Fatalf("record: %v", rec)
	}
	ups := updates(r.cl.since(from))
	var said, asked bool
	for _, u := range ups {
		if u["sessionUpdate"] == "user_message_chunk" && meta(u)["seq"] != nil {
			said = true
		}
		if u["sessionUpdate"] == "agent_message_chunk" && strings.Contains(u["content"].(map[string]any)["text"].(string), "first answer") {
			asked = true
		}
	}
	if !said || !asked {
		t.Fatalf("replay: %v", ups)
	}
	// The conversation goes on: the model is sent the earlier turn.
	r.model.script(say("second answer"))
	if stop, _ := r.prompt(id, "what was the word?"); stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}
	msgs, _ := r.recorded(id, agent.EvUserMessage)
	if len(msgs) != 2 {
		t.Fatalf("user messages after load: %v", msgs)
	}
	ok, _ := r.recordStore().Verify(id)
	if !ok.OK {
		t.Fatalf("the chain does not verify after going on: %+v", ok)
	}
}

// §4.1 one writer: a session another Abhed process holds is refused, and
// §2.8 another user's session is unknown here.
func TestStudioLoadRefusesHeldAndForeignSessions(t *testing.T) {
	r := newStudioRig(t, "", say("x"))
	rec := r.recordStore()
	// Another user's session in the same record directory.
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: "s-0123456789abcdef01234567", User: "mallory", Workspace: r.ws}); err != nil {
		t.Fatal(err)
	}
	r2 := agent.NewRecorder(rec, "s-0123456789abcdef01234567", "")
	if _, err := r2.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "secret plan"}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"session/load", "_abhed/record/verify", "_abhed/events/page", "_abhed/hawkeye", "_abhed/session/fork"} {
		r.cl.refused(errParams, method, map[string]any{"sessionId": "s-0123456789abcdef01234567", "cwd": r.ws, "format": "json"})
	}
	var list struct {
		Sessions []map[string]any `json:"sessions"`
	}
	r.cl.ok("session/list", map[string]any{}, &list)
	for _, s := range list.Sessions {
		if s["sessionId"] == "s-0123456789abcdef01234567" {
			t.Fatal("another user's session was listed")
		}
	}
	// A session held by another process of this user is refused, with a fork offered.
	id := r.open()
	r.prompt(id, "hello")
	other, err := local.Open(local.Options{Dir: rec.Dir(), Tenant: rec.Tenant(), User: cliUser()})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	if err := other.Acquire(id); err != nil {
		t.Fatal(err)
	}
	msg := r.cl.refused(errRefused, "session/load", map[string]any{"sessionId": id, "cwd": r.ws})
	if !strings.Contains(msg, "another Abhed process") {
		t.Fatalf("refusal: %s", msg)
	}
}

// §4.1 verification before trust: a record that fails verification loads
// marked unverified, takes no prompt, and can be forked.
func TestStudioUnverifiedRecordIsReadOnly(t *testing.T) {
	r := newStudioRig(t, "", say("a"))
	id := r.open()
	r.prompt(id, "the original words")
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	path := r.recordStore().Path(id)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "the original words", "the altered words!", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var loaded map[string]any
	r.cl.ok("session/load", map[string]any{"sessionId": id, "cwd": r.ws}, &loaded)
	rec := meta(loaded)["record"].(map[string]any)
	if rec["verified"] != false || rec["firstBad"] == nil {
		t.Fatalf("record: %v", rec)
	}
	r.cl.refused(errRecord, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "go on"}}})
	var v map[string]any
	r.cl.ok("_abhed/record/verify", map[string]any{"sessionId": id}, &v)
	if v["ok"] != false || v["firstBad"] == nil {
		t.Fatalf("verify: %v", v)
	}
	var fork map[string]any
	r.cl.ok("_abhed/session/fork", map[string]any{"sessionId": id}, &fork)
	if fork["sessionId"] == nil || fork["parent"] != id {
		t.Fatalf("fork: %v", fork)
	}
	branched, _ := r.recorded(fork["sessionId"].(string), agent.EvSessionBranched)
	if len(branched) != 1 || branched[0]["unverified"] == nil {
		t.Fatalf("session.branched: %v", branched)
	}
}

// §4.1 rename, §4.2 fork and §4.3 compact; each is the person's, recorded.
func TestStudioRenameForkCompact(t *testing.T) {
	r := newStudioRig(t, "", say("one"), say("summary of it"))
	id := r.open()
	r.prompt(id, "first")
	r.cl.ok("_abhed/session/rename", map[string]any{"sessionId": id, "name": "my work"}, nil)
	named, actors := r.recorded(id, agent.EvSessionNamed)
	if len(named) != 1 || named[0]["name"] != "my work" || actors[0] != agent.ActorUser {
		t.Fatalf("session.named: %v %v", named, actors)
	}
	r.cl.refused(errParams, "_abhed/session/rename", map[string]any{"sessionId": id, "name": "two\nlines"})
	var fork map[string]any
	r.cl.ok("_abhed/session/fork", map[string]any{"sessionId": id}, &fork)
	child := fork["sessionId"].(string)
	if child == id || fork["parent"] != id {
		t.Fatalf("fork: %v", fork)
	}
	// Returned open: its first prompt needs no session/load.
	r.model.script(say("in the fork"))
	if stop, _ := r.prompt(child, "go on"); stop != "end_turn" {
		t.Fatalf("fork prompt: %q", stop)
	}
	// More exchanges than compaction keeps verbatim, so there is older
	// history to summarise.
	for _, said := range []string{"two", "three", "four", "five"} {
		r.model.script(say(said))
		if stop, _ := r.prompt(id, "next"); stop != "end_turn" {
			t.Fatalf("prompt: %q", stop)
		}
	}
	var c map[string]any
	r.model.script(say("compacted summary"))
	r.cl.ok("_abhed/session/compact", map[string]any{"sessionId": id}, &c)
	started, actors := r.recorded(id, agent.EvCompactStarted)
	if len(started) != 1 || actors[0] != agent.ActorUser {
		t.Fatalf("compaction.started: %v", started)
	}
	if done, _ := r.recorded(id, agent.EvCompactDone); len(done) != 1 {
		t.Fatalf("compaction.completed: %v", done)
	}
}

// §4.4 the event stream: a page, and a subscription that delivers the
// backlog then each event as it is recorded, with the chain's links.
func TestStudioEventStream(t *testing.T) {
	r := newStudioRig(t, "", say("one"), say("two"))
	id := r.open()
	r.prompt(id, "first")
	var page struct {
		Events []map[string]any `json:"events"`
		More   bool             `json:"more"`
		Head   map[string]any   `json:"head"`
	}
	r.cl.ok("_abhed/events/page", map[string]any{"sessionId": id, "limit": 2}, &page)
	if len(page.Events) != 2 || !page.More || page.Events[0]["hash"] == "" || page.Events[1]["prev"] != page.Events[0]["hash"] {
		t.Fatalf("page: %+v", page)
	}
	r.cl.ok("_abhed/events/page", map[string]any{"sessionId": id, "types": []string{"user.message"}}, &page)
	if len(page.Events) != 1 || page.Events[0]["actor"] != "user" {
		t.Fatalf("typed page: %+v", page.Events)
	}
	from := r.cl.mark()
	var sub struct {
		Subscription string `json:"subscription"`
	}
	r.cl.ok("_abhed/events/subscribe", map[string]any{"sessionId": id}, &sub)
	r.cl.waitFor(from, "the backlog", func(m rpcMessage) bool {
		return m.Method == "_abhed/event" && strings.Contains(string(m.Params), `"user.message"`)
	})
	at := r.cl.mark()
	r.prompt(id, "second")
	r.cl.waitFor(at, "a live event", func(m rpcMessage) bool {
		return m.Method == "_abhed/event" && strings.Contains(string(m.Params), "second")
	})
	// Seqs arrive in order, each once.
	var last float64
	for _, m := range r.cl.since(from) {
		if m.Method != "_abhed/event" {
			continue
		}
		var p struct {
			Event map[string]any `json:"event"`
		}
		_ = json.Unmarshal(m.Params, &p)
		seq := p.Event["seq"].(float64)
		if seq <= last {
			t.Fatalf("seq %v after %v", seq, last)
		}
		last = seq
	}
	for i := 0; i < maxSubscriptions-1; i++ {
		r.cl.ok("_abhed/events/subscribe", map[string]any{"sessionId": id}, nil)
	}
	r.cl.refused(errRefused, "_abhed/events/subscribe", map[string]any{"sessionId": id})
	r.cl.ok("_abhed/events/unsubscribe", map[string]any{"subscription": sub.Subscription}, nil)
	r.cl.refused(errParams, "_abhed/events/unsubscribe", map[string]any{"subscription": sub.Subscription})
}

// §4.5 export goes to ~/.abhed/exports with its hash, and verifies offline;
// §4.6 HawkEYE over the durable record.
func TestStudioExportAndHawkeye(t *testing.T) {
	r := newStudioRig(t, "", say("one"))
	id := r.open()
	r.prompt(id, "first")
	var ex struct {
		Path   string `json:"path"`
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	r.cl.ok("_abhed/export", map[string]any{"sessionId": id, "format": "jsonl"}, &ex)
	if !strings.HasPrefix(ex.Path, r.home) || strings.HasPrefix(ex.Path, r.ws) || ex.Bytes == 0 || len(ex.SHA256) != 64 {
		t.Fatalf("export: %+v", ex)
	}
	if rep, err := local.VerifyFile(ex.Path); err != nil || !rep.OK {
		t.Fatalf("the export does not verify offline: %+v %v", rep, err)
	}
	r.cl.refused(errParams, "_abhed/export", map[string]any{"sessionId": id, "format": "pdf"})
	var rep map[string]any
	r.cl.ok("_abhed/hawkeye", map[string]any{"sessionId": id, "format": "json"}, &rep)
	if rep["session_id"] != id || rep["totals"] == nil {
		t.Fatalf("hawkeye: %v", rep)
	}
	var page struct {
		HTML string `json:"html"`
	}
	r.cl.ok("_abhed/hawkeye", map[string]any{"sessionId": id, "format": "html"}, &page)
	if !strings.Contains(page.HTML, "<html") {
		t.Fatal("no HawkEYE page")
	}
	r.cl.ok("_abhed/export", map[string]any{"sessionId": id, "format": "html"}, &ex)
	if !strings.HasSuffix(ex.Path, ".html") {
		t.Fatalf("html export: %+v", ex)
	}
}

// §2.5: an "always" is recorded approval.scope_granted by the person, and a
// session loaded from the record keeps it.
func TestRuleAlwaysIsRecordedAndRestored(t *testing.T) {
	r := newStudioRig(t, "")
	dir := r.ws
	r.model.script(callTool("c1", "bash", map[string]any{"command": "mkdir " + dir + "/one"}), say("done"))
	asks := 0
	r.cl.answering(func(method string, params json.RawMessage) any {
		asks++
		return chosen(params, "allow_always")
	})
	id := r.open()
	r.prompt(id, "make a folder")
	granted, actors := r.recorded(id, agent.EvApprovalScopeGranted)
	if asks != 1 || len(granted) != 1 || granted[0]["by"] != "user" || actors[0] != agent.ActorUser || granted[0]["scope"] == "" {
		t.Fatalf("asks %d, approval.scope_granted %v %v", asks, granted, actors)
	}
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	r.cl.ok("session/load", map[string]any{"sessionId": id, "cwd": r.ws}, nil)
	r.model.script(callTool("c2", "bash", map[string]any{"command": "mkdir " + dir + "/two"}), say("done"))
	r.prompt(id, "another")
	if asks != 1 {
		t.Fatalf("the restored scope was asked again: %d asks", asks)
	}
}
