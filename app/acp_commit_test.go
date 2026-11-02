package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// §7.12: the engine reads the staged diff itself, leaves out key files,
// withholds stored secrets, records where it goes first, and answers with
// the message.
func TestStudioCommitMessage(t *testing.T) {
	model := newFIMServer(t, "Here is the commit message:\n```\nAdd area helper\n\nCompute a rectangle's area.\n```")
	r := completionRig(t, model.URL, "")
	secret := "stored-secret-value-0123456789abcdef"
	store := filepath.Join(os.Getenv("HOME"), ".abhed", "secrets.json")
	if err := os.WriteFile(store, []byte(`{"FAKE_TOKEN":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_SECRETS_FILE", store)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	r.write("area.go", "package geo\n\nfunc Area(w, h int) int { return w * h }\n// "+secret+"\n")
	r.write(".env", "TOKEN=plain-key-value-123\n")
	git("add", "area.go", ".env")
	id := r.open()
	var res struct {
		Message  string `json:"message"`
		Excluded int    `json:"excluded"`
		Empty    string `json:"empty"`
	}
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Message != "Add area helper\n\nCompute a rectangle's area." || res.Excluded != 1 {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	sent := fmt.Sprint(model.bodies)
	model.mu.Unlock()
	if !strings.Contains(sent, "func Area") || strings.Contains(sent, "plain-key-value") || strings.Contains(sent, secret) {
		t.Fatalf("sent %s", sent)
	}
	req, _ := r.recorded(id, agent.EvCommitRequested)
	gen, _ := r.recorded(id, agent.EvCommitMessage)
	if len(req) != 1 || req[0]["files"] != float64(1) || req[0]["excluded"] != float64(1) || len(gen) != 1 || gen[0]["outcome"] != "served" {
		t.Fatalf("requested %v generated %v", req, gen)
	}

	// Nothing staged: nothing sent.
	git("reset", "-q")
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Empty == "" || res.Message != "" {
		t.Fatalf("%+v", res)
	}
}

// A staged file a read rule denies is left out, and the repository's own
// diff.external, textconv and fsmonitor programs never run.
func TestStudioCommitMessageRunsNoRepositoryProgram(t *testing.T) {
	model := newFIMServer(t, "Change things")
	r := completionRig(t, model.URL, `,"permissions":{"deny":["read(**/hidden/**)"]}`)
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat \"$1\" 2>/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	r.write(".gitattributes", "*.go diff=evil\n")
	r.write("a.go", "package a\n")
	r.write("hidden/p.go", "package p // PRIVATE\n")
	git("add", "a.go", "hidden/p.go", ".gitattributes")
	// Set after the test's own git has run, so only the engine's could run them.
	git("config", "diff.external", script)
	git("config", "diff.evil.textconv", script)
	git("config", "core.fsmonitor", script)
	id := r.open()
	var res struct {
		Message  string `json:"message"`
		Excluded int    `json:"excluded"`
	}
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Message != "Change things" || res.Excluded != 1 {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	sent := strings.Join(model.bodies, "\n")
	model.mu.Unlock()
	if strings.Contains(sent, "PRIVATE") || !strings.Contains(sent, "package a") {
		t.Fatalf("sent %.400s", sent)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a program the repository names ran")
	}
}

// With nothing staged, the message is for the working tree's changes; a model
// that gives no message is a clear error, never an empty answer.
func TestStudioCommitMessageUnstagedAndEmpty(t *testing.T) {
	model := newFIMServer(t, "Tune the loop")
	r := completionRig(t, model.URL, "")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	r.write("loop.go", "package a\n\nvar n = 1\n")
	git("add", "loop.go")
	git("commit", "-q", "-m", "first")
	r.write("loop.go", "package a\n\nvar n = 2\n")
	id := r.open()
	var res struct {
		Message string `json:"message"`
		Source  string `json:"source"`
	}
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Message != "Tune the loop" || res.Source != "unstaged" {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	sent := strings.Join(model.bodies, "\n")
	model.mu.Unlock()
	if !strings.Contains(sent, "var n = 2") {
		t.Fatalf("the working tree's change was not sent: %.300s", sent)
	}

	quiet := newFIMServer(t, "")
	r2 := completionRig(t, quiet.URL, "")
	git2 := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", r2.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git2("init", "-q")
	r2.write("a.go", "package a\n")
	git2("add", "a.go")
	id2 := r2.open()
	got := r2.cl.call("_abhed/commitMessage", map[string]any{"sessionId": id2})
	if got.Error == nil || !strings.Contains(got.Error.Message, "gave no message") {
		t.Fatalf("%+v %s", got.Error, got.Result)
	}
}

// The QA case: a gitignored file force-added is left out, and a lower-case
// secret-named JSON key in a file that is sent has its value withheld.
func TestStudioCommitMessageLeavesOutIgnoredAndNamedSecrets(t *testing.T) {
	model := newFIMServer(t, "Configure the service")
	r := completionRig(t, model.URL, "")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	r.write(".gitignore", "secrets.local.json\n")
	r.write("secrets.local.json", `{"note":"IGNORED-CONTENT"}`+"\n")
	r.write("config/app.json", `{"name":"svc","internal_api_key":"internal-9f8e7d6c5b4a"}`+"\n")
	// A name that reads as pathspec magic is still a path, judged and sent.
	r.write(":!odd.txt", "ODD-CONTENT\n")
	git("add", ".gitignore", "config/app.json", "./:!odd.txt")
	git("add", "-f", "secrets.local.json")
	id := r.open()
	var res struct {
		Message  string `json:"message"`
		Excluded int    `json:"excluded"`
	}
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Message != "Configure the service" || res.Excluded != 1 {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	sent := strings.Join(model.bodies, "\n")
	model.mu.Unlock()
	if strings.Contains(sent, "IGNORED-CONTENT") || strings.Contains(sent, "internal-9f8e7d6c5b4a") || !strings.Contains(sent, "internal_api_key") || !strings.Contains(sent, "ODD-CONTENT") {
		t.Fatalf("sent %.600s", sent)
	}
}

// Secrets on a staged diff's added lines are withheld: YAML, a block
// scalar, = files and upper-case names, each line starting with +.
func TestStudioCommitMessageWithholdsSecretsOnAddedLines(t *testing.T) {
	model := newFIMServer(t, "Add settings")
	r := completionRig(t, model.URL, "")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	r.write("values.yaml", "db:\n  password: Zq81kLm0Pw7xA\n  cert: |\n    plain-line\n  private_key: |\n    BlockSecret9x7\n  user: bob\n")
	r.write("app.properties", "spring.datasource.password=PropSecret8y6\nname=svc\n")
	r.write("deploy.vars", "DB_PASSWORD=UpperSecret7z5\n")
	r.write("settings.py", "api_key = \"PySecret6w4a1\"\nDEBUG = True\n")
	git("add", "values.yaml", "app.properties", "deploy.vars", "settings.py")
	id := r.open()
	var res struct {
		Message string `json:"message"`
	}
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Message != "Add settings" {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	sent := strings.Join(model.bodies, "\n")
	model.mu.Unlock()
	for _, s := range []string{"Zq81kLm0Pw7xA", "BlockSecret9x7", "PropSecret8y6", "UpperSecret7z5", "PySecret6w4a1"} {
		if strings.Contains(sent, s) {
			t.Errorf("%s reached the model", s)
		}
	}
	for _, s := range []string{"user: bob", "name=svc", "DEBUG = True", "plain-line"} {
		if !strings.Contains(sent, s) {
			t.Errorf("%q was lost from the diff", s)
		}
	}
}

// The QA case for rotation: a block scalar's value changes, its key stays a
// context line, and neither the old value nor the new reaches the model.
func TestStudioCommitMessageRotatedBlockSecret(t *testing.T) {
	model := newFIMServer(t, "Rotate the password")
	r := completionRig(t, model.URL, "")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	r.write("values.yaml", "db:\n  password: |\n    OldBlockSecret1\n  user: bob\n")
	git("add", "values.yaml")
	git("commit", "-q", "-m", "first")
	r.write("values.yaml", "db:\n  password: |\n    NewBlockSecret2\n  user: bob\n")
	git("add", "values.yaml")
	id := r.open()
	var res struct {
		Message string `json:"message"`
	}
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Message != "Rotate the password" {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	sent := strings.Join(model.bodies, "\n")
	model.mu.Unlock()
	if strings.Contains(sent, "OldBlockSecret1") || strings.Contains(sent, "NewBlockSecret2") || !strings.Contains(sent, "user: bob") {
		t.Fatalf("sent %.600s", sent)
	}
}
