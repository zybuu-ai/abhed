package hostgit

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// A repository the agent can write can name programs in its own
// configuration. None of them runs when Abhed runs git on the host: not an
// fsmonitor on status, not a hook, a clean or smudge filter, or a textconv on
// worktree add, diff and commit.
func TestRepositoryProgramsDoNotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the planted programs are shell scripts")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo, marks := t.TempDir(), t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(Env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=evil diff=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "init")

	script := func(name string) string {
		p := filepath.Join(marks, name+".sh")
		body := "#!/bin/sh\ntouch " + filepath.Join(marks, name+".ran") + "\ncat\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hooks := filepath.Join(repo, ".git", "hooks")
	for _, h := range []string{"post-checkout", "pre-commit"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\ntouch "+filepath.Join(marks, h+".ran")+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run("config", "core.fsmonitor", script("fsmonitor"))
	run("config", "filter.evil.clean", script("clean"))
	run("config", "filter.evil.smudge", script("smudge"))
	run("config", "diff.evil.textconv", script("textconv"))
	// A driver whose name holds "=", which the -c form cannot name.
	run("config", "filter.a=b.clean", script("eqclean"))
	if err := os.WriteFile(filepath.Join(repo, "b.dat"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=evil diff=evil\n*.dat filter=a=b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the file look changed, so status and diff consult the filters.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello again\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for _, args := range [][]string{
		{"status", "--porcelain"},
		{"add", "-A"},
		{"diff", "--stat"},
		{"diff"},
		{"worktree", "add", "-b", "wt", filepath.Join(repo, ".abhed", "wt"), "HEAD"},
	} {
		if out, err := Command(ctx, repo, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	author := []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
	if out, err := New(ctx, repo).CommandWith(ctx, nil, author, "commit", "-qam", "change").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	ents, _ := os.ReadDir(marks)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".ran" {
			t.Errorf("the repository's program ran: %s", e.Name())
		}
	}
}

// A submodule has a configuration of its own, which the parent's switches do
// not reach: status and diff do not enter it.
func TestSubmoduleProgramsDoNotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the planted program is a shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base, marks := t.TempDir(), t.TempDir()
	env := append(Env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_ALLOW_PROTOCOL=https:file")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sub, repo := filepath.Join(base, "sub"), filepath.Join(base, "repo")
	for _, d := range []string{sub, repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		run(d, "init", "-q")
		if err := os.WriteFile(filepath.Join(d, "f.txt"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(d, "add", ".")
		run(d, "commit", "-qm", "init")
	}
	run(repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	run(repo, "commit", "-qm", "sub")
	monitor := filepath.Join(marks, "fsmonitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\ntouch "+filepath.Join(marks, "fsmonitor.ran")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(filepath.Join(repo, "sub"), "config", "core.fsmonitor", monitor)
	// A filter only the submodule's configuration names: the parent's list
	// of drivers cannot switch it off.
	clean := filepath.Join(marks, "clean.sh")
	if err := os.WriteFile(clean, []byte("#!/bin/sh\ntouch "+filepath.Join(marks, "clean.ran")+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(filepath.Join(repo, "sub"), "config", "filter.subonly.clean", clean)
	if err := os.WriteFile(filepath.Join(repo, "sub", ".gitattributes"), []byte("*.txt filter=subonly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "f.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(context.Background(), repo)
	for _, args := range [][]string{{"status", "--porcelain"}, {"diff", "--stat"}} {
		if out, err := r.Command(context.Background(), args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, m := range []string{"fsmonitor.ran", "clean.ran"} {
		if _, err := os.Stat(filepath.Join(marks, m)); err == nil {
			t.Errorf("the submodule's program ran: %s", m)
		}
	}
}

// Every git Abhed runs on the host goes through this package: no other
// non-test code starts git itself.
func TestNoOtherCodeRunsGit(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	direct := regexp.MustCompile(`exec\.Command(Context)?\([^)]*"git"`)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry holds no code to check
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			if path == filepath.Join(root, "internal", "hostgit") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil && direct.Match(data) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s runs git itself; use internal/hostgit", rel)
		}
		return nil
	})
}

// A repository's configuration can allow a transport by name, which beats
// protocol.allow; the environment's allow-list does not yield to it, so an
// ext:: rewrite runs no program.
func TestOnlyHTTPSTransportRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the planted program is a shell command")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo, marks := t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "protocol.ext.allow", "always"},
		{"config", "protocol.file.allow", "always"},
		{"config", "url.ext::sh -c touch% " + filepath.Join(marks, "ext.ran") + "% #.insteadOf", "https://git.example/"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out, _ := Command(context.Background(), repo, "ls-remote", "https://git.example/t/r.git").CombinedOutput()
	if _, err := os.Stat(filepath.Join(marks, "ext.ran")); err == nil {
		t.Fatalf("a transport the repository allowed ran a program:\n%s", out)
	}
}
