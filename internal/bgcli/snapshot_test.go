package bgcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/bgjob"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// untar is a snapshot's files by name.
func untar(t *testing.T, tgz []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			t.Fatalf("%s is type %c", h.Name, h.Typeflag)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

// The snapshot is the repository's tracked files as they are in the working
// tree, and nothing the person's read rules withhold, named like a key,
// holding a key or a stored secret, untracked, or a link.
func TestStartSendsASnapshotOfTheTrackedFiles(t *testing.T) {
	r := newRig(t)
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "main.go"), "package main\n")
	write(t, filepath.Join(repo, "pkg", "a.go"), "package pkg // committed\n")
	write(t, filepath.Join(repo, "gone.txt"), "deleted in the working tree\n")
	write(t, filepath.Join(repo, "private", "notes.md"), "withheld by a deny rule\n")
	write(t, filepath.Join(repo, "keys", "id_rsa"), "not really a key\n")
	write(t, filepath.Join(repo, "config.py"), "AWS_SECRET_ACCESS_KEY = \"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEYzz\"\n")
	write(t, filepath.Join(repo, "settings.txt"), "token=stored-value-7f3a\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "x")
	write(t, filepath.Join(repo, "pkg", "a.go"), "package pkg // edited, not staged\n")
	write(t, filepath.Join(repo, "untracked.txt"), "never sent\n")
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	r.workspace = filepath.Join(repo, "pkg") // started from a folder inside the repository
	r.withheld = func(abs string) bool { return strings.HasSuffix(filepath.ToSlash(abs), "/private/notes.md") }
	r.stored = func(b []byte) bool { return bytes.Contains(b, []byte("stored-value-7f3a")) }

	res := r.run("", false, "", "start", "fix", "it")
	if res.code != ExitOK {
		t.Fatalf("start: %+v", res)
	}
	if len(r.srv.snapshots) != 1 || r.srv.started[0].Workspace.Kind != bgjob.WorkspaceSnapshot || r.srv.started[0].Workspace.Snapshot != "snap_1" {
		t.Fatalf("sent %d snapshots, started %+v", len(r.srv.snapshots), r.srv.started)
	}
	files := untar(t, r.srv.snapshots[0])
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "main.go,pkg/a.go" {
		t.Fatalf("the snapshot holds %v; start said %s", names, res.err)
	}
	if files["pkg/a.go"] != "package pkg // edited, not staged\n" {
		t.Fatalf("pkg/a.go as sent: %q", files["pkg/a.go"])
	}
	for _, want := range []string{"read rules withhold: private/notes.md", "config.py", "keys/id_rsa", "settings.txt", "link (a link)", "sent 2 tracked file(s)"} {
		if !strings.Contains(res.err, want) {
			t.Errorf("the start does not say %q:\n%s", want, res.err)
		}
	}
}

// Outside a repository start says so, unless asked to start empty; a
// job that kept its patch is read from the job, not its session.
func TestStartOutsideARepositoryAndTheKeptPatch(t *testing.T) {
	r := newRig(t)
	if res := r.run("", false, "", "start", "x"); res.code != ExitUsage || !strings.Contains(res.err, "-no-snapshot") {
		t.Fatalf("outside a repository: %+v", res)
	}
	if len(r.srv.started) != 0 || len(r.srv.snapshots) != 0 {
		t.Fatal("something was sent")
	}
	r.srv.job.Result = &bgjob.Result{Bytes: 30}
	if res := r.run("", false, "", "logs", "-patch", "bg_1"); res.code != ExitOK || res.out != "--- a/kept.txt\n+++ b/kept.txt\n" {
		t.Fatalf("the kept patch: %+v", res)
	}
}

// A snapshot that cannot be checked against the person's rules or secrets
// is not sent.
func TestAnUncheckedSnapshotIsNotSent(t *testing.T) {
	r := newRig(t)
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "a.go"), "package a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "x")
	var out, errOut bytes.Buffer
	code := Main(Env{ProfilesPath: r.profiles, Out: &out, Err: &errOut, Workspace: repo,
		SnapshotErr: errors.New("the secrets store cannot be loaded")}, []string{"start", "x"})
	if code != ExitFailed || !strings.Contains(errOut.String(), "no snapshot was sent") {
		t.Fatalf("start: %d %s", code, errOut.String())
	}
	if len(r.srv.snapshots) != 0 || len(r.srv.started) != 0 {
		t.Fatal("something was sent")
	}
}
