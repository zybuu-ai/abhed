package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// A refused trust names the _meta key the client sent, current or legacy.
func TestRequestedTrustNamesTheKeySent(t *testing.T) {
	c := &acpConn{}
	for key, want := range map[string]string{acpMetaKey: acpMetaKey, acpLegacyMetaKey: acpLegacyMetaKey} {
		raw := map[string]json.RawMessage{key: json.RawMessage(`{"trust":"trusted"}`)}
		_, e := c.requestedTrust(raw)
		if e == nil || !strings.Contains(e.Message, `_meta["`+want+`"]`) {
			t.Errorf("%s: %+v", key, e)
		}
	}
}

// A malformed params refusal names the field and the JSON kind, never a Go type.
func TestStudioMalformedParamsNameTheField(t *testing.T) {
	r, id := closedSession(t, true)
	cases := []struct {
		method string
		params any
		want   string
	}{
		{"session/load", map[string]any{"sessionId": 7, "cwd": r.ws}, `"sessionId" must be a string, not number`},
		{"session/resume", map[string]any{"sessionId": id, "_meta": "x"}, `"_meta" must be an object, not string`},
		{"_abhed/session/fork", map[string]any{"sessionId": id, "throughSeq": "3"}, `"throughSeq" must be a whole number, not string`},
		{"_abhed/doctor", map[string]any{"cwd": []string{"a"}}, `"cwd" must be a string, not array`},
		{"session/new", []int{1}, `params must be an object, not array`},
		{"session/load", map[string]any{"sessionId": id, "_meta": map[string]any{acpMetaKey: map[string]any{"trust": 1}}}, `"trust" must be a string, not number`},
		{"session/load", map[string]any{"sessionId": id, "_meta": map[string]any{acpMetaKey: map[string]any{"trust": "untrusted", "x": 1}}}, `unknown field "x"`},
	}
	for _, c := range cases {
		msg := r.cl.refused(errParams, c.method, c.params)
		if !strings.Contains(msg, c.want) || strings.Contains(msg, "Go ") || strings.Contains(msg, "json.RawMessage") || strings.Contains(msg, "struct") {
			t.Errorf("%s %v: %q, want it to say %q", c.method, c.params, msg, c.want)
		}
	}
}
