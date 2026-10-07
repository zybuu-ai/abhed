package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// loosening is a workspace file that widens policy: trusted, echo runs unasked.
const loosening = `{"permissions":{"allow":["bash(echo *)"]}}`

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
