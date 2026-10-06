package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// answeringModel replies "done" to every request.
func answeringModel(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// managedAt points the managed path at dir/config.json, holding body when
// it is not empty, for one test.
func managedAt(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	return path
}

// webHomeRun runs one -p session with the user's file holding user, and
// returns the events of the session it recorded.
func webHomeRun(t *testing.T, user string, args ...string) []agent.Event {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	model := `"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + answeringModel(t) + `","model":"m","context_window":8192}}}`
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "{" + model + "}"
	if user != "" {
		body = "{" + model + "," + user + "}"
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := stderrOf(t, append([]string{"-C", ws}, append(args, "-p", "hi")...)); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	rec, err := openRecord(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	list, err := rec.ListSessions(context.Background(), 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions: %v %v", list, err)
	}
	evs, err := rec.Events(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func configEvents(evs []agent.Event, t agent.EventType) []agent.ConfigAttempt {
	var out []agent.ConfigAttempt
	for _, ev := range evs {
		if ev.Type == t {
			var a agent.ConfigAttempt
			_ = json.Unmarshal(ev.Payload, &a)
			out = append(out, a)
		}
	}
	return out
}

// A user's attempt to turn web search on and point it elsewhere is recorded
// in the session, with who made it and no key; the start says search is off.
func TestRefusedWebAttemptsAreRecorded(t *testing.T) {
	managedAt(t, t.TempDir(), "")
	evs := webHomeRun(t, `"web_search":{"enabled":true,"base_url":"http://127.0.0.1:1/collect","api_key":"sk-do-not-record"}`)
	refused := configEvents(evs, agent.EvConfigRefused)
	keys := map[string]agent.ConfigAttempt{}
	for _, a := range refused {
		keys[a.Key] = a
	}
	on, ok := keys["web_search.enabled"]
	if !ok || on.Decision != "set_aside" || on.Layer != config.LayerUser || on.Value != "true" {
		t.Fatalf("the attempt is not recorded: %+v", refused)
	}
	if on.Principal.Kind != "os-user" || on.Principal.UID == "" || (runtime.GOOS != "windows" && on.Principal.OSUser == "") {
		t.Fatalf("no principal: %+v", on.Principal)
	}
	if _, ok := keys["web_search.base_url"]; !ok {
		t.Fatalf("the base_url attempt is not recorded: %+v", refused)
	}
	for _, ev := range evs {
		if bytes.Contains(ev.Payload, []byte("sk-do-not-record")) {
			t.Fatalf("the key reached the record: %s", ev.Payload)
		}
	}
	var start struct {
		Web map[string]any `json:"web"`
	}
	for _, ev := range evs {
		if ev.Type == agent.EvSessionStarted {
			_ = json.Unmarshal(ev.Payload, &start)
		}
	}
	if start.Web["search"] != false || !strings.HasPrefix(fmt.Sprint(start.Web["state"]), "off; only the managed configuration") {
		t.Fatalf("session.started: %+v", start.Web)
	}
}

// Inside an agent's command the attempt is credited to that command.
func TestRefusedWebAttemptInAgentCommandNamesIt(t *testing.T) {
	managedAt(t, t.TempDir(), "")
	asAgentCommand(t)
	t.Setenv("ABHED_COMMAND_ID", "c0ffee")
	evs := webHomeRun(t, `"web_search":{"enabled":true}`)
	refused := configEvents(evs, agent.EvConfigRefused)
	if len(refused) == 0 || refused[0].Principal.Kind != "agent-command" || refused[0].Principal.CommandID != "c0ffee" {
		t.Fatalf("not credited to the agent's command: %+v", refused)
	}
}

// The managed file still turns search on, and the start says so.
func TestManagedFileEnablesWebSearch(t *testing.T) {
	managedAt(t, t.TempDir(), `{"web_search":{"enabled":true,"provider":"searxng","base_url":"http://127.0.0.1:1"}}`)
	evs := webHomeRun(t, `"web_search":{"base_url":"http://127.0.0.1:2/collect"}`)
	var start struct {
		Web map[string]any `json:"web"`
	}
	for _, ev := range evs {
		if ev.Type == agent.EvSessionStarted {
			_ = json.Unmarshal(ev.Payload, &start)
		}
	}
	if start.Web["search"] != true || start.Web["search_source"] != "managed" || start.Web["provider"] != "searxng" {
		t.Fatalf("session.started: %+v", start.Web)
	}
	if refused := configEvents(evs, agent.EvConfigRefused); len(refused) != 1 || refused[0].Key != "web_search.base_url" {
		t.Fatalf("the redirect was not recorded: %+v", refused)
	}
}

// /config set refuses the web sections and records who asked.
func TestConfigSetRefusesWebAndRecordsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := &cliState{}
	e := &cmdEnv{st: st}
	err := configSet(context.Background(), e, "web_search.enabled", "true")
	if err == nil || !strings.Contains(err.Error(), "managed only") {
		t.Fatalf("not refused: %v", err)
	}
	if err := configSet(context.Background(), e, "no.such.key", "1"); err == nil {
		t.Fatal("an unknown key was taken")
	}
	if len(st.pending) != 2 {
		t.Fatalf("held events: %+v", st.pending)
	}
	a, ok := st.pending[0].payload.(agent.ConfigAttempt)
	if st.pending[0].typ != agent.EvConfigRefused || !ok || a.Key != "web_search.enabled" || a.Decision != "refused" ||
		a.Layer != "command" || a.Principal.Kind == "" || a.Principal.UID == "" {
		t.Fatalf("the refusal: %+v", st.pending[0])
	}
}

func runAdmin(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := adminCmd(args, &out, &errb)
	return code, out.String(), errb.String()
}

func adminLog(t *testing.T, path string) []adminEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []adminEntry
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e adminEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

// The admin command edits the managed file, keeps what else it says, and
// logs who changed what beside it; it never prints or logs a key.
func TestAdminWebSearchEditsTheManagedFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	file := managedAt(t, dir, `{"permissions":{"mode":"plan"},"web_search":{"api_key":"sk-admin-secret"}}`)
	code, out, errs := runAdmin(t, "web-search", "on", "--provider", "searxng", "--base-url", "https://search.internal")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if strings.Contains(out+errs, "sk-admin-secret") {
		t.Fatalf("a key was printed: %s %s", out, errs)
	}
	cfg, err := config.LoadWith(t.TempDir(), config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WebSearch.Enabled || cfg.WebSearch.Provider != "searxng" || cfg.Permissions.Mode != "plan" || cfg.WebSearch.APIKey != "sk-admin-secret" {
		t.Fatalf("the managed file: %+v %s", cfg.WebSearch, cfg.Permissions.Mode)
	}
	if code, _, errs := runAdmin(t, "web-search", "off"); code != 0 {
		t.Fatalf("off: %d %s", code, errs)
	}
	log := adminLog(t, filepath.Join(dir, adminLogName))
	if len(log) != 2 || log[0].Result != "changed" || log[0].UID == "" || log[0].To["enabled"] != true || log[1].To["enabled"] != false {
		t.Fatalf("log: %+v", log)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, adminLogName)); bytes.Contains(data, []byte("sk-admin-secret")) {
		t.Fatalf("the log holds the key: %s", data)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// Without the right to write the managed file the command refuses, changes
// nothing, and the attempt is logged where it can be.
func TestAdminWebSearchRefusesWithoutWriteAccess(t *testing.T) {
	if os.Getuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs a directory this user cannot write")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(t.TempDir(), "etc-abhed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := managedAt(t, dir, `{"web_search":{"enabled":false}}`)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	code, _, errs := runAdmin(t, "web-search", "on")
	if code != 1 || !strings.Contains(errs, "needs root or the administrator's rights") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), `"enabled":false`) {
		t.Fatalf("the managed file changed: %s", data)
	}
	log := adminLog(t, filepath.Join(home, ".abhed", adminLogName))
	if len(log) != 1 || log[0].Result != "refused" || log[0].UID == "" || log[0].File != file {
		t.Fatalf("log: %+v", log)
	}
}

// Inside an agent's command the command is refused, through Main and on its
// own, and the refusal is logged.
func TestAdminWebSearchRefusedInAgentCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	file := managedAt(t, dir, "")
	asAgentCommand(t)
	if out, code := stderrOf(t, []string{"admin", "web-search", "on"}); code != 1 || !strings.Contains(out, "refused inside an agent's command") {
		t.Fatalf("Main: exit %d %s", code, out)
	}
	code, _, errs := runAdmin(t, "web-search", "on")
	if code != 1 || !strings.Contains(errs, "inside an agent's command") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the managed file was written: %v", err)
	}
	// Once refused by Main, once by the command itself.
	log := adminLog(t, filepath.Join(dir, adminLogName))
	if len(log) != 2 || log[0].Result != "refused" || log[0].Agent == "" || log[1].Result != "refused" || log[1].Agent == "" {
		t.Fatalf("log: %+v", log)
	}
}

// The log's command is built from the flags that passed their checks: a
// password in --base-url or a key given as --api-key-env never reaches it.
func TestAdminLogCommandHoldsOnlyCheckedFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	managedAt(t, dir, "")
	secrets := []string{"hunter2pass", "sk-live-abcdef0123456789", "tvlyABCDEF0123456789xyz", "search_key"}
	runs := [][]string{
		{"web-search", "on", "--base-url", "https://admin:hunter2pass@search.internal/q"},
		{"web-search", "on", "--provider", "brave", "--api-key-env", "sk-live-abcdef0123456789"},
		{"web-search", "on", "--provider", "brave", "--api-key-env", "tvlyABCDEF0123456789xyz"},
		{"web-search", "on", "--provider", "brave", "--api-key-env", "search_key"},
	}
	for _, args := range runs {
		if code, _, errs := runAdmin(t, args...); code != 1 || !strings.Contains(errs, "refused") {
			t.Fatalf("%v: exit %d %s", args, code, errs)
		}
	}
	if code, _, errs := runAdmin(t, "web-search", "on", "--provider", "searxng", "--base-url", "https://search.internal/q?x=1",
		"--api-key-env", "SEARCH_API_KEY", "--max-results", "5"); code != 0 {
		t.Fatalf("valid flags: exit %d %s", code, errs)
	}
	data, err := os.ReadFile(filepath.Join(dir, adminLogName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if bytes.Contains(data, []byte(s)) {
			t.Fatalf("the log holds %q: %s", s, data)
		}
	}
	log := adminLog(t, filepath.Join(dir, adminLogName))
	if len(log) != 5 || log[0].Command != "web-search on --base-url (refused)" ||
		log[1].Command != "web-search on --provider brave --api-key-env (refused)" {
		t.Fatalf("log: %+v", log)
	}
	if got, want := log[4].Command, "web-search on --provider searxng --base-url "+config.PrintableURL("https://search.internal/q?x=1")+
		" --api-key-env SEARCH_API_KEY --max-results 5"; got != want {
		t.Fatalf("command %q, want %q", got, want)
	}
	// Refused before it ran, inside an agent's command, the same.
	logAdminInAgent([]string{"web-search", "on", "--api-key-env", "sk-live-abcdef0123456789"}, "test", io.Discard)
	if data, _ := os.ReadFile(filepath.Join(dir, adminLogName)); bytes.Contains(data, []byte("sk-live")) {
		t.Fatalf("the in-agent entry holds the key: %s", data)
	}
}

// --api-key-env takes a variable's name, never something shaped like a key.
func TestAdminKeyEnvMustBeAName(t *testing.T) {
	for _, v := range []string{"SEARCH_API_KEY", "_KEY", "BRAVE_KEY_2"} {
		if err := checkSearchFlags("", "", v, 0); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"search_key", "2KEY", "KEY-1", "sk-abc", "AKIAABCDEFGHIJKLMNOP", "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345678901"} {
		if err := checkSearchFlags("", "", v, 0); err == nil {
			t.Errorf("%q taken", v)
		} else if strings.Contains(err.Error(), v) {
			t.Errorf("the refusal repeats %q: %v", v, err)
		}
	}
}

// A managed file that is a link is refused, and neither it nor what it
// points at is changed.
func TestAdminWebSearchRefusesALinkedManagedFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"web_search":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	file := managedAt(t, dir, "")
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runAdmin(t, "web-search", "on")
	if code != 1 || !strings.Contains(errs, "not a regular file") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", fi, err)
	}
	if data, _ := os.ReadFile(target); string(data) != `{"web_search":{"enabled":false}}` {
		t.Fatalf("the link's target changed: %s", data)
	}
}

// The managed file is replaced whole: written and flushed beside it, then
// renamed over it, then the directory flushed; the old file is never
// rewritten in place and nothing is left behind.
func TestWriteManagedDocReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	old := `{"web_search":{"enabled":false}}`
	if err := os.WriteFile(file, []byte(old), 0o640); err != nil {
		t.Fatal(err)
	}
	// A second name for the old file: an in-place write would change it too.
	keep := filepath.Join(t.TempDir(), "old.json")
	if err := os.Link(file, keep); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	var steps []string
	oldSync, oldDir := syncManaged, syncManagedIn
	t.Cleanup(func() { syncManaged, syncManagedIn = oldSync, oldDir })
	syncManaged = func(f *os.File) error {
		data, _ := os.ReadFile(file)
		steps = append(steps, "sync file, target "+string(data))
		return f.Sync()
	}
	syncManagedIn = func(d string) error {
		data, _ := os.ReadFile(file)
		steps = append(steps, "sync dir "+filepath.Base(d)+", target changed "+fmt.Sprint(string(data) != old))
		return syncDir(d)
	}
	if err := writeManagedDoc(file, map[string]any{"web_search": map[string]any{"enabled": true}}, 0o640); err != nil {
		t.Fatal(err)
	}
	want := []string{"sync file, target " + old, "sync dir " + filepath.Base(dir) + ", target changed true"}
	if fmt.Sprint(steps) != fmt.Sprint(want) {
		t.Fatalf("steps %q, want %q", steps, want)
	}
	if data, _ := os.ReadFile(keep); string(data) != old {
		t.Fatalf("the old file was rewritten in place: %s", data)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), `"enabled": true`) {
		t.Fatalf("the new file: %s", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	if fi, _ := os.Stat(file); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// Run as root, the new file keeps the old one's owner and group, so a
// root:abhed 0640 file stays readable by the server.
func TestWriteManagedDocKeepsTheOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owners are ACLs on Windows")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := fileOwner(fi)
	if !ok {
		t.Fatal("no owner")
	}
	oldRoot, oldChown := runningAsRoot, chownManaged
	t.Cleanup(func() { runningAsRoot, chownManaged = oldRoot, oldChown })
	var got []int
	chownManaged = func(f *os.File, u, g int) error {
		got = append(got, u, g)
		return nil
	}
	runningAsRoot = func() bool { return false }
	if err := writeManagedDoc(file, map[string]any{}, 0o640); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("not root, yet chowned: %v", got)
	}
	runningAsRoot = func() bool { return true }
	if err := writeManagedDoc(file, map[string]any{}, 0o640); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint([]int{uid, gid}) {
		t.Fatalf("chown %v, want %d %d", got, uid, gid)
	}
	// A refused chown leaves the old file as it was.
	chownManaged = func(*os.File, int, int) error { return os.ErrPermission }
	if err := writeManagedDoc(file, map[string]any{"x": 1}, 0o640); err == nil {
		t.Fatal("a failed chown was ignored")
	}
	if data, _ := os.ReadFile(file); strings.Contains(string(data), `"x"`) {
		t.Fatalf("written despite the failed chown: %s", data)
	}
}
