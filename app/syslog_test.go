package app

import (
	"errors"
	"strings"
	"testing"
)

// captureSysLog replaces the system log for one test and returns what was sent.
func captureSysLog(t *testing.T, fail error) *[]string {
	t.Helper()
	var sent []string
	old := sysLogWrite
	sysLogWrite = func(msg string) error {
		sent = append(sent, msg)
		return fail
	}
	t.Cleanup(func() { sysLogWrite = old })
	return &sent
}

// Every attempt, changed, unchanged or refused, goes to the system log as one
// line with who, what, from and to, and the result, and never a key.
func TestAdminAttemptsGoToTheSystemLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	managedAt(t, t.TempDir(), `{"web_search":{"api_key":"sk-admin-secret"}}`)
	sent := captureSysLog(t, nil)
	if code, _, errs := runAdmin(t, "web-search", "on", "--provider", "brave", "--api-key-env", "BRAVE_KEY"); code != 0 {
		t.Fatalf("on: %d %s", code, errs)
	}
	if code, _, errs := runAdmin(t, "web-search", "on", "--provider", "brave"); code != 0 {
		t.Fatalf("again: %d %s", code, errs)
	}
	if code, _, _ := runAdmin(t, "web-search", "off", "--provider", "brave"); code != 1 {
		t.Fatalf("off with a provider was not refused: %d", code)
	}
	if len(*sent) != 3 {
		t.Fatalf("sent %d entries: %q", len(*sent), *sent)
	}
	for i, want := range []string{`result="changed"`, `result="unchanged"`, `result="refused"`} {
		m := (*sent)[i]
		if !strings.HasPrefix(m, sysLogTag+" time=") || !strings.Contains(m, want) || !strings.Contains(m, ` uid="`) ||
			!strings.Contains(m, ` user="`) || !strings.Contains(m, ` sudo_user="`) || !strings.Contains(m, ` from="`) || !strings.Contains(m, ` to="`) {
			t.Fatalf("entry %d: %s", i, m)
		}
		if strings.Contains(m, "sk-admin-secret") {
			t.Fatalf("entry %d holds the key: %s", i, m)
		}
	}
	if m := (*sent)[0]; !strings.Contains(m, `action="web-search on"`) || !strings.Contains(m, `api_key_set\":true`) ||
		!strings.Contains(m, `BRAVE_KEY`) {
		t.Fatalf("changed entry: %s", m)
	}
	if m := (*sent)[2]; !strings.Contains(m, `action="web-search off"`) || !strings.Contains(m, `reason="--provider`) {
		t.Fatalf("refused entry: %s", m)
	}
}

// A value carrying a newline or a quote stays inside its field: the entry is
// one line and cannot forge another.
func TestSystemLogEscapesNewlines(t *testing.T) {
	forged := "x\n" + sysLogTag + ` result="changed" user="root"`
	m := sysLogMessage(adminEntry{Time: "t", UID: "501", User: "u\r\nroot", Action: "web-search on",
		From: map[string]any{"provider": forged, "api_key": "sk-secret"}, Result: "refused", Reason: "bad\nline "})
	if strings.ContainsAny(m, "\n\r ") {
		t.Fatalf("a newline reached the entry: %q", m)
	}
	if strings.Count(m, sysLogTag) != 2 || strings.Count(m, ` result="`) != 1 {
		// The forged tag is inside the from field, escaped, and no field repeats.
		t.Fatalf("a value forged a field: %s", m)
	}
	if strings.Contains(m, "sk-secret") {
		t.Fatalf("a key reached the entry: %s", m)
	}
	if !strings.Contains(m, `user="u\r\nroot"`) {
		t.Fatalf("user not escaped: %s", m)
	}
}

// A system log that cannot be written is said on stderr; the command's
// result and the admin.jsonl entry stand.
func TestSystemLogFailureDoesNotChangeTheResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	managedAt(t, dir, "")
	captureSysLog(t, errors.New("no log here"))
	code, out, errs := runAdmin(t, "web-search", "on")
	if code != 0 || !strings.Contains(out, "web search is on") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if !strings.Contains(errs, "not in the system log: no log here") {
		t.Fatalf("stderr: %s", errs)
	}
	if log := adminLog(t, dir+"/"+adminLogName); len(log) != 1 || log[0].Result != "changed" {
		t.Fatalf("log: %+v", log)
	}
}

// Refused by Main inside an agent's command, before the command runs, the
// attempt still reaches the system log.
func TestAdminInAgentCommandGoesToTheSystemLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	managedAt(t, t.TempDir(), "")
	asAgentCommand(t)
	sent := captureSysLog(t, nil)
	if _, code := stderrOf(t, []string{"admin", "web-search", "on"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], `result="refused"`) || !strings.Contains((*sent)[0], `agent_command="ABHED_SANDBOX is set"`) {
		t.Fatalf("sent: %q", *sent)
	}
}
