package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/server"
)

type doneAdapter struct{}

func (doneAdapter) Name() string                           { return "done" }
func (doneAdapter) Profile() model.Profile                 { return model.Profile{Name: "done", ContextWindow: 32000} }
func (doneAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (doneAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// gitPlantServed tries each way the eval found to have the person's next git
// command run something: fsmonitor, a hook, a commondir, attributes, moved hooks.
const gitPlantServed = "git config core.fsmonitor 'touch pwned'; echo planted > .git/hooks/pre-commit; " +
	"echo /tmp > .git/commondir; echo planted >> .git/info/attributes; mv .git/hooks .git/h2; echo r\"\"an"

// servedRepo is a workspace holding a git repository, its real path, and a
// copy of what git reads there before any command ran.
func servedRepo(t *testing.T) (string, map[string]string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	t.Setenv("HOME", t.TempDir())
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "init", "-q", "-b", "main", ws).CombinedOutput(); err != nil { // #nosec G204 -- test fixture
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "info", "attributes"), []byte("*.txt text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, f := range []string{".git/config", ".git/info/attributes"} {
		b, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		before[f] = string(b)
	}
	return ws, before
}

// checkServedPlant fails for each git file a served command changed.
func checkServedPlant(t *testing.T, ws string, before map[string]string, how string) {
	t.Helper()
	for f, want := range before {
		if b, _ := os.ReadFile(filepath.Join(ws, filepath.FromSlash(f))); string(b) != want {
			t.Errorf("%s changed %s: %q", how, f, b)
		}
	}
	for _, f := range []string{".git/hooks/pre-commit", ".git/commondir", ".git/h2", "pwned"} {
		if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(f))); err == nil {
			t.Errorf("%s made %s", how, f)
		}
	}
	if info, err := os.Stat(filepath.Join(ws, ".git", "hooks")); err != nil || !info.IsDir() {
		t.Errorf("%s moved .git/hooks: %v", how, err)
	}
}

// servedBench is a server built from serve's own sandbox and bash, with one
// session open.
type servedBench struct {
	t       *testing.T
	h       http.Handler
	session string
	bash    tools.Bash
}

func newServedBench(t *testing.T, ws string, edit func(*config.Config)) *servedBench {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	if edit != nil {
		edit(&cfg)
	}
	cfg, sb, bash, err := serveSandbox(cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(sb) })
	if sb.Tier() != sandbox.TierProcess {
		t.Skipf("this host serves under the %s tier; the process tier is the one tested here", sb.Tier())
	}
	s := server.New(server.Options{Workspace: ws, Config: cfg, Adapter: doneAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, bash)})
	b := &servedBench{t: t, h: s.Handler(), bash: bash}
	rec := b.do("POST", "/v1/sessions", `{"prompt":"work"}`)
	var created struct {
		SessionID string `json:"session_id"`
	}
	if rec.Code/100 != 2 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.SessionID == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	b.session = created.SessionID
	return b
}

func (b *servedBench) do(method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Abhed-Tenant", "acme")
	b.h.ServeHTTP(rec, req)
	return rec
}

// exec runs command as the person's ! command, the agent's bash tool.
func (b *servedBench) exec(command string) string {
	b.t.Helper()
	body, _ := json.Marshal(map[string]string{"command": command})
	rec := b.do("POST", "/v1/sessions/"+b.session+"/exec", string(body))
	var out struct {
		Output string `json:"output"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK {
		b.t.Fatalf("exec: %d %s", rec.Code, rec.Body.String())
	}
	return out.Output
}

// terminal runs one workbench terminal: a line, or an interactive shell fed
// input, and returns what it wrote once it exits.
func (b *servedBench) terminal(start map[string]any, input string) string {
	b.t.Helper()
	start["cols"], start["rows"] = 80, 24
	body, _ := json.Marshal(start)
	rec := b.do("POST", "/v1/sessions/"+b.session+"/pty", string(body))
	var resp struct {
		ID          string `json:"id"`
		Denied      string `json:"denied"`
		Interactive bool   `json:"interactive"`
		Lines       string `json:"lines"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.ID == "" || resp.Denied != "" {
		b.t.Fatalf("pty start: %d %s", rec.Code, rec.Body.String())
	}
	if start["interactive"] == true && !resp.Interactive {
		b.t.Fatalf("no interactive shell: %s", resp.Lines)
	}
	srv := httptest.NewServer(b.h)
	defer srv.Close()
	base := srv.URL + "/v1/sessions/" + b.session + "/pty/" + resp.ID
	if input != "" {
		go func() {
			time.Sleep(500 * time.Millisecond)
			req, _ := http.NewRequest("POST", base+"/input", strings.NewReader(input))
			req.Header.Set("X-Abhed-Tenant", "acme")
			if r, err := http.DefaultClient.Do(req); err == nil {
				_ = r.Body.Close()
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base, nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	var out strings.Builder
	event := ""
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "out":
			data, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "data: "))
			out.Write(data)
		}
	}
	return out.String()
}

// A served session's commands cannot write what git reads, whichever way they
// run: the person's ! command, a terminal line, the interactive shell. Git
// itself still works there, and the host's own git still reads the repository.
func TestServedSessionHoldsGitFiles(t *testing.T) {
	ws, before := servedRepo(t)
	b := newServedBench(t, ws, nil)
	if b.bash.Shell == nil {
		t.Fatal("the process tier gave serve no interactive shell")
	}
	if out := b.exec(gitPlantServed); !strings.Contains(out, "ran") {
		t.Fatalf("exec did not run: %s", out)
	}
	// Bubblewrap takes a planted commondir out before the next command.
	_ = b.exec("true")
	checkServedPlant(t, ws, before, "a ! command")

	if out := b.terminal(map[string]any{"command": gitPlantServed}, ""); !strings.Contains(out, "ran") {
		t.Fatalf("the terminal line did not run: %s", out)
	}
	_ = b.exec("true")
	checkServedPlant(t, ws, before, "a terminal line")

	if out := b.terminal(map[string]any{"interactive": true}, gitPlantServed+"; exit\n"); !strings.Contains(out, "ran") {
		t.Fatalf("the shell did not run the line: %s", out)
	}
	_ = b.exec("true")
	checkServedPlant(t, ws, before, "the interactive shell")

	g := "git -c user.name=a -c user.email=a@b "
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"command": "set -e; echo hi > f.txt; " + g + "add f.txt; " + g + "commit -q -m one; " +
		g + "branch b; " + g + "checkout -q b; echo two >> f.txt; " + g + "commit -q -am two; " + g + "checkout -q main; echo committed"})
	if res := b.bash.Run(context.Background(), sess, args); res.IsError || !strings.Contains(res.Content, "committed") {
		t.Fatalf("git in a served session: %s", res.Content)
	}
	// On macOS git cannot make a linked worktree's folder under .git/worktrees there.
	if runtime.GOOS == "linux" {
		args, _ := json.Marshal(map[string]string{"command": g + "worktree add -q wt b && echo added"})
		if res := b.bash.Run(context.Background(), sess, args); res.IsError || !strings.Contains(res.Content, "added") {
			t.Fatalf("git worktree add in a served session: %s", res.Content)
		}
	}
	if out, err := exec.Command("git", "-C", ws, "log", "--oneline", "--all").CombinedOutput(); err != nil || !strings.Contains(string(out), "two") {
		t.Fatalf("the host's git log: %v %s", err, out)
	}
}

// The CLI's sandbox is unchanged: serve alone turns git protection on.
func TestCLISandboxLeavesGitFilesAlone(t *testing.T) {
	ws, _ := servedRepo(t)
	cfg := config.Default()
	sb, err := buildSandbox(cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(sb) })
	if sb.Tier() != sandbox.TierProcess {
		t.Skipf("this host runs the CLI under the %s tier; the process tier is the one tested here", sb.Tier())
	}
	if cfg.Sandbox.ProtectGit {
		t.Fatal("the default configuration protects git")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := sb.Command(ctx, ws, "echo '# cli' >> .git/info/attributes").CombinedOutput(); err != nil {
		t.Fatalf("the CLI's sandbox refused a git file: %v %s", err, out)
	}
	served, ssb, _, err := serveSandbox(cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	_ = sandbox.Close(ssb)
	if !served.Sandbox.ProtectGit || cfg.Sandbox.ProtectGit {
		t.Fatal("serve's configuration must protect git, and leave the caller's alone")
	}
}
