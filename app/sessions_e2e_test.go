package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store/local"
)

// TestSessionsHelper is the CLI the session tests run, in the workspace and
// with the arguments they pass.
func TestSessionsHelper(t *testing.T) {
	ws := os.Getenv("ABHED_SESS_WS")
	if ws == "" {
		t.Skip("run by the session tests")
	}
	var args []string
	if a := os.Getenv("ABHED_SESS_ARGS"); a != "" {
		args = strings.Split(a, "\x1f")
	}
	os.Exit(Main(append([]string{"-C", ws}, args...)))
}

// sessRig is a home, a workspace and a scripted model that answers "noted n"
// and keeps what it was sent, shared by every CLI a test runs.
type sessRig struct {
	t        *testing.T
	home, ws string
	url      string
	mu       sync.Mutex
	bodies   []string
}

func newSessRig(t *testing.T) *sessRig {
	t.Helper()
	g := &sessRig{t: t, home: t.TempDir(), ws: t.TempDir()}
	if r, err := filepath.EvalSymlinks(g.ws); err == nil {
		g.ws = r
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bodies = append(g.bodies, string(body))
		n := len(g.bodies)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		// "write path=content" as the latest prompt is a write call; anything
		// else, or a tool's result, is answered with text.
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if k := len(req.Messages); k > 0 && req.Messages[k-1].Role == "user" {
			text, _ := req.Messages[k-1].Content.(string)
			if spec, ok := strings.CutPrefix(text, "write "); ok {
				path, content, _ := strings.Cut(spec, "=")
				path = filepath.Join(g.ws, path) // the write tool takes absolute paths
				args, _ := json.Marshal(map[string]string{"path": path, "content": content})
				call, _ := json.Marshal(string(args))
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c%d\",\"type\":\"function\",\"function\":{\"name\":\"write\",\"arguments\":%s}}]}}]}\n\n", n, call)
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				return
			}
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"noted %d\"}}]}\n\n", n)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	g.url = srv.URL
	cfg := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	_ = os.MkdirAll(filepath.Join(g.ws, ".abhed"), 0o755)
	if err := os.WriteFile(filepath.Join(g.ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *sessRig) lastBody() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return conversationOf(g.t, g.bodies[len(g.bodies)-1])
}

func (g *sessRig) cmd(args ...string) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^TestSessionsHelper$") //nolint:gosec // the test binary itself
	c.Env = append(os.Environ(), "ABHED_SESS_WS="+g.ws, "ABHED_SESS_ARGS="+strings.Join(args, "\x1f"),
		"HOME="+g.home, "USERPROFILE="+g.home, "ABHED_TRUST_WORKSPACE=1")
	return c
}

// start runs an interactive CLI on pipes.
func (g *sessRig) start(args ...string) *cliSession {
	g.t.Helper()
	c := &cliSession{t: g.t, out: &syncBuffer{}, ws: g.ws}
	c.cmd = g.cmd(args...)
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		g.t.Fatal(err)
	}
	c.stdin = stdin
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	if err := c.cmd.Start(); err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = c.stdin.Close(); _ = c.cmd.Process.Kill(); _ = c.cmd.Wait() })
	return c
}

// ask sends a prompt and waits for its turn to end.
func (g *sessRig) ask(c *cliSession, prompt string) string {
	g.t.Helper()
	c.tasks++
	fmt.Fprintln(c.stdin, prompt)
	c.waitFor(func(out string) bool { return strings.Count(out, " in / ") >= c.tasks }, "the task to finish")
	return g.lastBody()
}

// exit closes the CLI's input and waits for it to end.
func exit(c *cliSession) {
	c.t.Helper()
	_ = c.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = c.cmd.Process.Kill()
		c.t.Fatalf("the CLI did not exit:\n%s", c.out.String())
	}
}

// record opens the rig's local record, as another process would.
func (g *sessRig) record() *local.Store {
	g.t.Helper()
	rec, err := local.Open(local.Options{Dir: filepath.Join(g.home, ".abhed", "records")})
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = rec.Close() })
	return rec
}

func (g *sessRig) sessions() []local.Entry {
	g.t.Helper()
	l, err := g.record().Index().List(local.Filter{All: true})
	if err != nil {
		g.t.Fatal(err)
	}
	return l
}

func verified(t *testing.T, rec *local.Store, id string) []agent.Event {
	t.Helper()
	rep, err := rec.Verify(id)
	if err != nil || !rep.OK {
		t.Fatalf("record of %s: %+v %v", id, rep, err)
	}
	evs, _ := rec.Events(id)
	return evs
}

func hasType(evs []agent.Event, t agent.EventType) bool {
	for _, e := range evs {
		if e.Type == t {
			return true
		}
	}
	return false
}

// A session survives the process: -c after an exit continues it, by name
// too, and every record verifies.
func TestContinueAfterExit(t *testing.T) {
	g := newSessRig(t)
	c := g.start("-n", "codewords")
	g.ask(c, "Remember the codeword ZEBRA-41.")
	exit(c)
	if !strings.Contains(c.out.String(), "local record, chained") {
		t.Fatalf("the banner does not name the record:\n%s", c.out.String())
	}

	c = g.start("-c")
	c.waitFor(func(out string) bool { return strings.Contains(out, "resumed") }, "the resume")
	if body := g.ask(c, "What is the codeword?"); !strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("-c did not carry the conversation:\n%s", body)
	}
	exit(c)
	s := g.sessions()
	if len(s) != 1 || s[0].Name != "codewords" || s[0].Title != "Remember the codeword ZEBRA-41." {
		t.Fatalf("sessions: %+v", s)
	}
	evs := verified(t, g.record(), s[0].ID)
	if !hasType(evs, agent.EvSessionNamed) {
		t.Fatal("-n was not recorded")
	}

	c = g.start("-r", "codewords")
	if body := g.ask(c, "And again?"); !strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("-r by name did not carry the conversation:\n%s", body)
	}
	exit(c)
	if s := g.sessions(); len(s) != 1 {
		t.Fatalf("-r started another session: %+v", s)
	}
	verified(t, g.record(), s[0].ID)
}

// --fork-session goes on in a copy: the original's record is unchanged, and
// the copy's conversation is the original's.
func TestForkSessionLeavesTheOriginal(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "Remember the codeword ZEBRA-41.")
	exit(c)
	orig := g.sessions()[0].ID
	before := verified(t, g.record(), orig)

	c = g.start("-c", "--fork-session")
	c.waitFor(func(out string) bool { return strings.Contains(out, "branched") }, "the branch")
	if body := g.ask(c, "What is the codeword?"); !strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("the branch lost the conversation:\n%s", body)
	}
	exit(c)
	rec := g.record()
	if after := verified(t, rec, orig); len(after) != len(before) {
		t.Fatalf("the original grew from %d to %d events", len(before), len(after))
	}
	var branch local.Entry
	for _, e := range g.sessions() {
		if e.ID != orig {
			branch = e
		}
	}
	if branch.Parent != orig {
		t.Fatalf("the branch does not name its source: %+v", branch)
	}
	evs := verified(t, rec, branch.ID)
	var b agent.SessionBranched
	if evs[0].Type != agent.EvSessionBranched || json.Unmarshal(evs[0].Payload, &b) != nil || b.From != orig {
		t.Fatalf("the branch's record opens with %s %s", evs[0].Type, evs[0].Payload)
	}
	// Fork of the copy through the copied part is the original's conversation.
	want, _ := agent.Fork(before, 0)
	got, _ := agent.Fork(evs, int64(len(branchEvents(before, 0))+1))
	if w, _ := json.Marshal(want); !bytes.Equal(w, mustJSON(got)) {
		t.Fatalf("the copy's conversation differs:\n got %s\nwant %s", mustJSON(got), w)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// /branch and /rename inside a session; /clear ends it and starts another;
// /resume goes back by name. Nothing is deleted.
func TestBranchRenameClearResume(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "Remember the codeword ZEBRA-41.")
	c.command("/rename first", "named first")
	c.command("/branch second", "branched")
	if body := g.ask(c, "Also remember OSPREY-58."); !strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("the branch lost the conversation:\n%s", body)
	}
	c.command("/clear", "context cleared")
	if body := g.ask(c, "What codewords do you know?"); strings.Contains(body, "ZEBRA") {
		t.Fatalf("/clear kept the conversation:\n%s", body)
	}
	c.command("/resume first", "resumed")
	if body := g.ask(c, "Which codewords?"); !strings.Contains(body, "ZEBRA-41") || strings.Contains(body, "OSPREY") {
		t.Fatalf("/resume first carried the wrong conversation:\n%s", body)
	}
	c.command("/sessions", "/resume <id or name>")
	exit(c)
	s := g.sessions()
	if len(s) != 3 {
		t.Fatalf("want three sessions, got %+v", s)
	}
	rec := g.record()
	for _, e := range s {
		verified(t, rec, e.ID)
	}
	if e, err := rec.Index().Resolve("second"); err != nil || e.Parent == "" {
		t.Fatalf("the branch by its name: %+v %v", e, err)
	}
}

// Resuming a record that fails verification says so and needs a yes; a
// piped run that does not answer yes starts a new session instead.
func TestResumeTamperedNeedsConfirm(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "Remember the codeword ZEBRA-41.")
	exit(c)
	id := g.sessions()[0].ID
	p := filepath.Join(g.home, ".abhed", "records", "default", id+".jsonl")
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, bytes.Replace(data, []byte("ZEBRA-41"), []byte("ZEBRA-99"), 1), 0o600); err != nil {
		t.Fatal(err)
	}

	c = g.start("-r", id)
	c.waitFor(func(out string) bool { return strings.Contains(out, "unverified") }, "the warning")
	fmt.Fprintln(c.stdin, "no")
	c.waitFor(func(out string) bool { return strings.Contains(out, "starting a new session") }, "the refusal")
	if body := g.ask(c, "What is the codeword?"); strings.Contains(body, "ZEBRA") {
		t.Fatalf("an unconfirmed tampered record was continued:\n%s", body)
	}
	exit(c)

	before, _ := os.ReadFile(p)
	c = g.start("-r", id)
	c.waitFor(func(out string) bool { return strings.Contains(out, "unverified") }, "the warning")
	fmt.Fprintln(c.stdin, "yes")
	c.waitFor(func(out string) bool { return strings.Contains(out, "going on in a new session") }, "the fork")
	if body := g.ask(c, "What is the codeword?"); !strings.Contains(body, "ZEBRA-99") {
		t.Fatalf("a confirmed resume did not go on:\n%s", body)
	}
	exit(c)
	// Nothing was written into the failing record, and its edit is still
	// there to find; the fork names it and why.
	if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
		t.Fatal("the unverified record was written to")
	}
	rec := g.record()
	if rep, _ := rec.Verify(id); rep.OK || rep.FirstBad != 1 {
		t.Fatalf("the tampered line is no longer reported: %+v", rep)
	}
	var fork local.Entry
	for _, e := range g.sessions() {
		if e.Parent == id {
			fork = e
		}
	}
	evs := verified(t, rec, fork.ID)
	var b agent.SessionBranched
	if evs[0].Type != agent.EvSessionBranched || json.Unmarshal(evs[0].Payload, &b) != nil || b.From != id || !strings.Contains(b.Unverified, "seq 1") {
		t.Fatalf("the fork does not record its unverified source: %s %s", evs[0].Type, evs[0].Payload)
	}
}

// -r alone picks from this workspace's sessions.
func TestResumePicker(t *testing.T) {
	g := newSessRig(t)
	for _, p := range []string{"Remember ALPHA-1.", "Remember BRAVO-2."} {
		c := g.start()
		g.ask(c, p)
		exit(c)
	}
	c := g.start("-r")
	c.waitFor(func(out string) bool { return strings.Contains(out, "choose 1-3") }, "the picker")
	fmt.Fprintln(c.stdin, "2") // newest first: ALPHA is second
	c.waitFor(func(out string) bool { return strings.Contains(out, "resumed") }, "the resume")
	if body := g.ask(c, "Which codeword?"); !strings.Contains(body, "ALPHA-1") || strings.Contains(body, "BRAVO") {
		t.Fatalf("the picker resumed the wrong session:\n%s", body)
	}
	exit(c)
}

// -c with -p continues headless; /export writes outside the workspace and
// its jsonl verifies.
func TestHeadlessContinueAndExport(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "Remember the codeword ZEBRA-41.")
	c.command("/export", "wrote ")
	c.command("/export s.jsonl", "wrote ")
	exit(c)
	if _, err := os.Stat(filepath.Join(g.ws, "s.jsonl")); err != nil {
		t.Fatal("an export to a path given was not written there")
	}
	entries, _ := os.ReadDir(filepath.Join(g.home, ".abhed", "exports"))
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".html") {
		t.Fatalf("default export: %v", entries)
	}
	if rep, err := local.VerifyFile(filepath.Join(g.ws, "s.jsonl")); err != nil || !rep.OK {
		t.Fatalf("the jsonl export: %+v %v", rep, err)
	}

	out, err := g.cmd("-c", "-p", "What is the codeword?").CombinedOutput()
	if err != nil {
		t.Fatalf("-c -p: %v\n%s", err, out)
	}
	if body := g.lastBody(); !strings.Contains(body, "ZEBRA-41") {
		t.Fatalf("-c -p did not carry the conversation:\n%s", body)
	}
	if s := g.sessions(); len(s) != 1 {
		t.Fatalf("-c -p started another session: %+v", s)
	}
	verified(t, g.record(), g.sessions()[0].ID)
	if out, err := g.cmd("-c", "-r", "x").CombinedOutput(); err == nil || !strings.Contains(string(out), "one of -c and -r") {
		t.Fatalf("-c with -r: %v %s", err, out)
	}
}

func TestBareResumeFlag(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		out  []string
		pick bool
	}{
		{[]string{"-r"}, []string{}, true},
		{[]string{"-r", "-C", "x"}, []string{"-C", "x"}, true},
		{[]string{"-r", "name"}, []string{"-r", "name"}, false},
		{[]string{"-p", "-r"}, []string{"-p", "-r"}, false},
		{[]string{"--resume"}, []string{}, true},
		{[]string{"record", "-r"}, []string{"record", "-r"}, false},
	} {
		got, pick := bareResume(tc.in)
		if pick != tc.pick || strings.Join(got, " ") != strings.Join(tc.out, " ") {
			t.Errorf("bareResume(%v) = %v %v", tc.in, got, pick)
		}
	}
}

var _ = config.TrustEnv

// A stored secret typed into a prompt, named, and exported never reaches
// the record's files: it is redacted before the first write.
func TestSecretsNeverReachTheRecord(t *testing.T) {
	g := newSessRig(t)
	_ = os.MkdirAll(filepath.Join(g.home, ".abhed"), 0o700)
	if err := os.WriteFile(filepath.Join(g.home, ".abhed", "secrets.json"), []byte(`{"FAKE_TOKEN":"`+fakeVaultValue+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := g.start("-n", "about "+fakeVaultValue)
	g.ask(c, "the token is "+fakeVaultValue+", keep it")
	c.command("/rename again "+fakeVaultValue, "named")
	c.command("/export", "wrote ")
	c.command("/export out.jsonl", "wrote ")
	exit(c)
	for _, root := range []string{filepath.Join(g.home, ".abhed", "records"), filepath.Join(g.home, ".abhed", "exports"), filepath.Join(g.ws, "out.jsonl")} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				if data, _ := os.ReadFile(p); bytes.Contains(data, []byte(fakeVaultValue)) {
					t.Errorf("the stored secret is on disk in %s", p)
				}
			}
			return nil
		})
	}
	verified(t, g.record(), g.sessions()[0].ID)
}
