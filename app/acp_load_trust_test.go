package app

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// loosening is a workspace file that widens policy: trusted, echo runs unasked.
// Its web key is one the record names when refused (config.refused).
const loosening = `{"permissions":{"allow":["bash(echo *)"]},"web_fetch":{"enabled":true}}`

// closedSession is a recorded, closed session in a workspace holding the
// loosening file, granted when grant is set, as Studio finds one after a reload.
func closedSession(t *testing.T, grant bool) (*studioRig, string) {
	t.Helper()
	r := newStudioRig(t, "", say("first"))
	r.write(".abhed/config.json", loosening)
	if grant {
		st, err := config.InspectWorkspace(r.ws)
		if err != nil || st.SHA256 == "" {
			t.Fatalf("inspect: %v %+v", err, st)
		}
		if err := config.GrantTrust(r.ws, st.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	id := r.open()
	r.prompt(id, "hello")
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	return r, id
}

// workspaceTrustOf is the trust report in a session result.
func workspaceTrustOf(t *testing.T, res map[string]any) (bool, string) {
	t.Helper()
	st, _ := meta(res)["workspaceTrust"].(map[string]any)
	if st == nil {
		t.Fatalf("no workspaceTrust in %v", res["_meta"])
	}
	trusted, _ := st["trusted"].(bool)
	reason, _ := st["reason"].(string)
	return trusted, reason
}

// §5.6: session/load and session/resume take the editor's _meta trust as
// session/new does: "untrusted" narrows, nothing widens, and no field keeps
// the stored decision.
func TestStudioLoadAndResumeHonourRequestedTrust(t *testing.T) {
	untrusted := map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}
	cases := []struct {
		name        string
		grant       bool
		meta        map[string]any
		wantCode    int
		wantTrusted bool
		wantReason  string
	}{
		{name: "no field keeps the stored grant", grant: true, wantTrusted: true, wantReason: "stored"},
		{name: "no field keeps an undecided file untrusted", wantReason: "new"},
		{name: "untrusted overrides the stored grant", grant: true, meta: untrusted, wantReason: "refused"},
		{name: "untrusted under the legacy key", grant: true, meta: map[string]any{"abhed": map[string]any{"trust": "untrusted"}}, wantReason: "refused"},
		{name: "trusted cannot widen an undecided file", meta: map[string]any{acpMetaKey: map[string]any{"trust": "trusted"}}, wantCode: errParams},
		{name: "trusted is refused even over a grant", grant: true, meta: map[string]any{acpMetaKey: map[string]any{"trust": "trusted"}}, wantCode: errParams},
		{name: "an unknown field is refused", grant: true, meta: map[string]any{acpMetaKey: map[string]any{"trust": "untrusted", "allow": []string{"bash(*)"}}}, wantCode: errParams},
	}
	for _, method := range []string{"session/load", "session/resume"} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				r, id := closedSession(t, tc.grant)
				params := map[string]any{"sessionId": id, "cwd": r.ws, "mcpServers": []any{}}
				if tc.meta != nil {
					params["_meta"] = tc.meta
				}
				if tc.wantCode != 0 {
					r.cl.refused(tc.wantCode, method, params)
					if r.cl.conn.session(id) != nil {
						t.Fatal("a refused request left the session open")
					}
					return
				}
				var res map[string]any
				r.cl.ok(method, params, &res)
				if trusted, reason := workspaceTrustOf(t, res); trusted != tc.wantTrusted || reason != tc.wantReason {
					t.Fatalf("workspaceTrust: trusted=%v reason=%q", trusted, reason)
				}
				// The session keeps what it was opened with, for a later restart.
				s := r.cl.conn.session(id)
				if wantRefused := tc.wantReason == "refused"; (s.trust == config.TrustRefused) != wantRefused {
					t.Fatalf("session trust %q", s.trust)
				}
			})
		}
	}
}

// A session open with the stored trust is not narrowed in place: resuming it
// untrusted is refused with the way out, and works once it is closed.
func TestStudioResumeUntrustedOfAnOpenTrustedSession(t *testing.T) {
	r, id := closedSession(t, true)
	r.cl.ok("session/resume", map[string]any{"sessionId": id, "cwd": r.ws}, nil)
	msg := r.cl.refused(errRefused, "session/resume", map[string]any{"sessionId": id, "cwd": r.ws,
		"_meta": map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}})
	if !strings.Contains(msg, "close it") {
		t.Fatalf("refusal: %s", msg)
	}
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	var res map[string]any
	r.cl.ok("session/resume", map[string]any{"sessionId": id, "cwd": r.ws,
		"_meta": map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}}, &res)
	if trusted, reason := workspaceTrustOf(t, res); trusted || reason != "refused" {
		t.Fatalf("workspaceTrust: trusted=%v reason=%q", trusted, reason)
	}
}

// A restart for a changed workspace file and a fork keep the editor's
// "untrusted" rather than the connection's trust.
func TestStudioUntrustedSurvivesRestartAndFork(t *testing.T) {
	r, id := closedSession(t, true)
	untrusted := map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}
	r.cl.ok("session/resume", map[string]any{"sessionId": id, "cwd": r.ws, "_meta": untrusted}, nil)
	s := r.cl.conn.session(id)
	r.cl.conn.restartForTrust(s)
	if st := s.agent.(trustReporter).WorkspaceTrust(); st.Trusted || st.Reason != "refused" {
		t.Fatalf("after restart: %+v", st)
	}
	starts, _ := startsOf(r, id)
	if last := starts[len(starts)-1]; last["via"] != "restart" || last["workspace_trust"].(map[string]any)["requested"] != "untrusted" {
		t.Fatalf("the restart's record: %v", last)
	}
	var forked map[string]any
	r.cl.ok("_abhed/session/fork", map[string]any{"sessionId": id, "_meta": untrusted}, &forked)
	if trusted, reason := workspaceTrustOf(t, forked); trusted || reason != "refused" {
		t.Fatalf("fork workspaceTrust: trusted=%v reason=%q", trusted, reason)
	}
	r.cl.refused(errParams, "_abhed/session/fork", map[string]any{"sessionId": id,
		"_meta": map[string]any{acpMetaKey: map[string]any{"trust": "trusted"}}})
}

// End to end: a granted workspace file that allows echo unasked is ignored
// when Studio's Restricted Mode loads the session, so the call is asked.
func TestStudioUntrustedLoadIgnoresLooseningWorkspaceConfig(t *testing.T) {
	r, id := closedSession(t, true)
	asks := func(from int) int {
		n := 0
		for _, m := range r.cl.since(from) {
			if m.Method == "session/request_permission" {
				n++
			}
		}
		return n
	}
	run := func(meta map[string]any) int {
		t.Helper()
		params := map[string]any{"sessionId": id, "cwd": r.ws, "mcpServers": []any{}}
		if meta != nil {
			params["_meta"] = meta
		}
		r.cl.ok("session/load", params, nil)
		r.model.script(callTool("c1", "bash", map[string]any{"command": "echo hi"}), say("done"))
		from := r.cl.mark()
		r.prompt(id, "say hi")
		n := asks(from)
		r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
		return n
	}
	if n := run(map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}); n != 1 {
		t.Fatalf("untrusted load: %d asks, want the echo asked", n)
	}
	// The same file, loaded with its stored grant, lets echo run unasked.
	if n := run(nil); n != 0 {
		t.Fatalf("trusted load: %d asks", n)
	}
	raw, _ := json.Marshal(r.events(id))
	if !strings.Contains(string(raw), "echo hi") {
		t.Fatal("the call is not in the record")
	}
}

// startsOf are the session.started and session.resumed payloads in the
// session's record, in order, and the decisions of the config.refused events
// after the last one.
func startsOf(r *studioRig, id string) (starts []map[string]any, refused []string) {
	for _, ev := range r.events(id) {
		var p map[string]any
		_ = json.Unmarshal(ev.Payload, &p)
		switch ev.Type {
		case agent.EvSessionStarted, agent.EvSessionResumed:
			p["type"] = string(ev.Type)
			starts, refused = append(starts, p), nil
		case agent.EvConfigRefused:
			d, _ := p["decision"].(string)
			refused = append(refused, d)
		}
	}
	return starts, refused
}

// The record shows the trust a continued session ran under: load, resume and
// fork each record a session.resumed with the trust asked for and what
// applied, followed by the settings it refused, as session/new does.
func TestStudioContinuedSessionRecordsItsTrust(t *testing.T) {
	untrusted := map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}
	check := func(t *testing.T, r *studioRig, id, via, requested string, trusted bool) {
		t.Helper()
		starts, refused := startsOf(r, id)
		last := starts[len(starts)-1]
		wt, _ := last["workspace_trust"].(map[string]any)
		if last["type"] != "session.resumed" || last["via"] != via || last["surface"] != "acp" || last["through_seq"] == nil ||
			wt["requested"] != requested || wt["trusted"] != trusted {
			t.Fatalf("last session.started: %v", last)
		}
		want := "set_aside"
		if !trusted {
			want = "ignored_untrusted"
		}
		if !slices.Contains(refused, want) {
			t.Fatalf("config.refused after the start: %v, want %s", refused, want)
		}
	}
	for method, via := range map[string]string{"session/load": "load", "session/resume": "resume"} {
		t.Run(method, func(t *testing.T) {
			r, id := closedSession(t, true)
			starts, _ := startsOf(r, id)
			if wt, _ := starts[0]["workspace_trust"].(map[string]any); len(starts) != 1 || starts[0]["type"] != "session.started" ||
				wt["requested"] != "stored" || wt["trusted"] != true {
				t.Fatalf("session/new's start: %v", starts)
			}
			r.cl.ok(method, map[string]any{"sessionId": id, "cwd": r.ws, "_meta": untrusted}, nil)
			check(t, r, id, via, "untrusted", false)
			r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
			r.cl.ok(method, map[string]any{"sessionId": id, "cwd": r.ws}, nil)
			check(t, r, id, via, "stored", true)
			if again, _ := r.recorded(id, agent.EvSessionStarted); len(again) != 1 {
				t.Fatalf("a continuation recorded another start: %v", again)
			}
		})
	}
	t.Run("fork", func(t *testing.T) {
		r, id := closedSession(t, true)
		var forked struct {
			SessionID string `json:"sessionId"`
		}
		r.cl.ok("_abhed/session/fork", map[string]any{"sessionId": id, "_meta": untrusted}, &forked)
		check(t, r, forked.SessionID, "fork", "untrusted", false)
	})
}

// A fork with no _meta takes the stored trust, even from a session open
// untrusted: trust is the request's, not the source's.
func TestStudioForkWithoutMetaTakesStoredTrust(t *testing.T) {
	r, id := closedSession(t, true)
	r.cl.ok("session/resume", map[string]any{"sessionId": id, "cwd": r.ws,
		"_meta": map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}}, nil)
	var forked map[string]any
	r.cl.ok("_abhed/session/fork", map[string]any{"sessionId": id}, &forked)
	if trusted, reason := workspaceTrustOf(t, forked); !trusted || reason != "stored" {
		t.Fatalf("fork workspaceTrust: trusted=%v reason=%q", trusted, reason)
	}
}

// Both _meta keys at once are refused, since only one would be read; the
// doctor's checks take the requested trust too.
func TestStudioMetaBothKeysAndDoctorTrust(t *testing.T) {
	r, id := closedSession(t, true)
	both := map[string]any{acpMetaKey: map[string]any{}, "abhed": map[string]any{"trust": "untrusted"}}
	for _, method := range []string{"session/new", "session/load", "session/resume", "_abhed/session/fork", "_abhed/doctor"} {
		r.cl.refused(errParams, method, map[string]any{"sessionId": id, "cwd": r.ws, "_meta": both})
	}
	if r.cl.conn.session(id) != nil {
		t.Fatal("a refused request opened the session")
	}
	trustCheck := func(meta map[string]any) map[string]any {
		var res struct {
			Checks []map[string]any `json:"checks"`
		}
		params := map[string]any{"cwd": r.ws}
		if meta != nil {
			params["_meta"] = meta
		}
		r.cl.ok("_abhed/doctor", params, &res)
		for _, c := range res.Checks {
			if c["id"] == "trust" {
				return c
			}
		}
		t.Fatalf("no trust check: %v", res.Checks)
		return nil
	}
	if c := trustCheck(nil); c["status"] != "ok" {
		t.Fatalf("stored grant: %v", c)
	}
	if c := trustCheck(map[string]any{acpMetaKey: map[string]any{"trust": "untrusted"}}); c["status"] != "warn" || !strings.Contains(c["detail"].(string), "refused") {
		t.Fatalf("untrusted doctor: %v", c)
	}
	r.cl.refused(errParams, "_abhed/doctor", map[string]any{"cwd": r.ws, "_meta": map[string]any{acpMetaKey: map[string]any{"trust": "trusted"}}})
}

// When the trust a continued session opened under cannot be recorded, it does
// not run unrecorded: load and fork refuse and drop it, and a restart leaves
// it read-only and says so.
func TestStudioContinuedSessionRefusesWhenItsTrustCannotBeRecorded(t *testing.T) {
	failing := func(t *testing.T) {
		t.Helper()
		old := recordResumed
		recordResumed = func(*agent.Recorder, map[string]any) error { return errors.New("disk full") }
		t.Cleanup(func() { recordResumed = old })
	}
	for _, method := range []string{"session/load", "session/resume"} {
		t.Run(method, func(t *testing.T) {
			r, id := closedSession(t, true)
			failing(t)
			msg := r.cl.refused(errRecord, method, map[string]any{"sessionId": id, "cwd": r.ws})
			if !strings.Contains(msg, "disk full") || r.cl.conn.session(id) != nil {
				t.Fatalf("refusal %q, session left open: %v", msg, r.cl.conn.session(id) != nil)
			}
		})
	}
	t.Run("fork", func(t *testing.T) {
		r, id := closedSession(t, true)
		failing(t)
		msg := r.cl.refused(errRecord, "_abhed/session/fork", map[string]any{"sessionId": id})
		if !strings.Contains(msg, "disk full") || !strings.Contains(msg, "kept in the session list") {
			t.Fatalf("refusal: %q", msg)
		}
		r.cl.conn.sessMu.Lock()
		open := len(r.cl.conn.sessions)
		r.cl.conn.sessMu.Unlock()
		if open != 0 {
			t.Fatalf("%d sessions left open after a failed fork", open)
		}
	})
	t.Run("restart", func(t *testing.T) {
		r, id := closedSession(t, true)
		r.cl.ok("session/resume", map[string]any{"sessionId": id, "cwd": r.ws}, nil)
		s := r.cl.conn.session(id)
		failing(t)
		from := r.cl.mark()
		r.cl.conn.restartForTrust(s)
		if !strings.Contains(s.readOnly, "could not record its restart") {
			t.Fatalf("readOnly %q", s.readOnly)
		}
		r.cl.waitFor(from, "the restart's failure", func(m rpcMessage) bool {
			return m.Method == "session/update" && strings.Contains(string(m.Params), "could not record its restart")
		})
		for _, u := range updates(r.cl.since(from)) {
			content, _ := u["content"].(map[string]any)
			if text, _ := content["text"].(string); strings.Contains(text, "restarted under") {
				t.Fatalf("a failed restart said it restarted: %q", text)
			}
		}
		r.cl.refused(errRecord, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "x"}}})
	})
}
