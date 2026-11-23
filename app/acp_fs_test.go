package app

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The editor's file system over ACP, for an engine on another machine: inside
// the session's roots only, Abhed's state and withheld paths never served,
// writes through the write tool recorded by: user, denials holding for the
// person, search, quick open, a watch and the SCM view's read-only git.
func TestStudioFS(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"deny":["read(**/secret.txt)","write(**/locked/**)","delete(**/keep.txt)"]}`)
	// Paths the rig's process sees: a filter's marker goes outside the workspace.
	outside := t.TempDir()
	if !slices.Contains(r.cl.conn.acpFeatures(), "fs") {
		t.Fatalf("features %v", r.cl.conn.acpFeatures())
	}
	id := r.open()
	ws := r.ws
	r.write("a.txt", "alpha\nneedle one\n")
	r.write("sub/b.go", "package b\n// Needle two\n")
	r.write("secret.txt", "needle hidden\n")
	r.write("keep.txt", "keep\n")
	r.write(".abhed/notes.md", "needle state\n")
	p := func(kv ...any) map[string]any {
		m := map[string]any{"sessionId": id}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}

	var dir struct {
		Entries []struct {
			Name string
			Type int
		} `json:"entries"`
	}
	r.cl.ok("_abhed/fs/readDir", p("path", ws), &dir)
	var names []string
	for _, e := range dir.Entries {
		names = append(names, e.Name)
	}
	if !slices.Contains(names, "a.txt") || !slices.Contains(names, "sub") || slices.Contains(names, "secret.txt") || slices.Contains(names, ".abhed") {
		t.Fatalf("listing %v", names)
	}
	var read struct{ Content, Sha256 string }
	r.cl.ok("_abhed/fs/read", p("path", filepath.Join(ws, "a.txt")), &read)
	if b, _ := base64.StdEncoding.DecodeString(read.Content); string(b) != "alpha\nneedle one\n" {
		t.Fatalf("read %q", b)
	}
	for _, refused := range []string{filepath.Join(ws, "secret.txt"), filepath.Join(ws, ".abhed", "notes.md"), "/etc/hosts", filepath.Join(ws, "..", "x")} {
		r.cl.refused(errPolicy, "_abhed/fs/read", p("path", refused))
	}

	// A write is the write tool's, on the manual path, recorded by: user.
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	r.cl.ok("_abhed/fs/write", p("path", filepath.Join(ws, "a.txt"), "content", enc("alpha\nedited\n"), "overwrite", true), nil)
	if r.read("a.txt") != "alpha\nedited\n" {
		t.Fatalf("not written: %q", r.read("a.txt"))
	}
	r.cl.ok("_abhed/fs/write", p("path", filepath.Join(ws, "new.txt"), "content", enc("new\n"), "create", true), nil)
	r.cl.refused(errRefused, "_abhed/fs/write", p("path", filepath.Join(ws, "new.txt"), "content", enc("x"), "create", true))
	r.cl.refused(errPolicy, "_abhed/fs/write", p("path", filepath.Join(ws, "locked", "f.txt"), "content", enc("x"), "create", true))
	r.cl.refused(errPolicy, "_abhed/fs/write", p("path", filepath.Join(ws, ".abhed", "config.json"), "content", enc("{}"), "create", true))
	r.cl.refused(errRefused, "_abhed/fs/write", p("path", filepath.Join(ws, "bin.dat"), "content", base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}), "create", true))
	r.cl.refused(errPolicy, "_abhed/fs/delete", p("path", filepath.Join(ws, "keep.txt")))
	r.cl.ok("_abhed/fs/rename", p("path", filepath.Join(ws, "new.txt"), "to", filepath.Join(ws, "renamed.txt")), nil)
	r.cl.refused(errPolicy, "_abhed/fs/rename", p("path", filepath.Join(ws, "renamed.txt"), "to", filepath.Join(ws, "locked", "x.txt")))
	r.cl.ok("_abhed/fs/mkdir", p("path", filepath.Join(ws, "d")), nil)
	r.cl.ok("_abhed/fs/delete", p("path", filepath.Join(ws, "renamed.txt")), nil)
	if _, err := os.Stat(filepath.Join(ws, "renamed.txt")); err == nil {
		t.Fatal("not deleted")
	}
	evs := r.events(id)
	byUser := 0
	for _, e := range evs {
		if strings.Contains(string(e.Type), "action") || e.Type == agent.EvManualEdit {
			byUser++
		}
	}
	if byUser == 0 {
		t.Fatal("the person's file operations were not recorded")
	}

	// Search and quick open skip state and withheld files.
	var found struct {
		Matches []struct {
			Path   string
			Line   int
			Ranges [][2]int
		} `json:"matches"`
	}
	r.cl.ok("_abhed/fs/search", p("path", ws, "query", "needle"), &found)
	var paths []string
	for _, m := range found.Matches {
		paths = append(paths, m.Path)
	}
	if !slices.Contains(paths, "sub/b.go") || slices.Contains(paths, "secret.txt") || slices.Contains(paths, ".abhed/notes.md") {
		t.Fatalf("search %v", paths)
	}
	r.cl.ok("_abhed/fs/search", p("path", ws, "query", "Needle", "caseSensitive", true, "include", []string{"**/*.go"}), &found)
	if len(found.Matches) != 1 || found.Matches[0].Path != "sub/b.go" || found.Matches[0].Line != 1 || found.Matches[0].Ranges[0] != [2]int{3, 9} {
		t.Fatalf("case-sensitive search %+v", found.Matches)
	}
	var files struct{ Files []string }
	r.cl.ok("_abhed/fs/files", p("path", ws, "query", "bgo"), &files)
	if !slices.Equal(files.Files, []string{"sub/b.go"}) {
		t.Fatalf("files %v", files.Files)
	}

	// A link is deleted and renamed as itself, never its target.
	r.write("target.txt", "kept\n")
	if err := os.Symlink(filepath.Join(ws, "target.txt"), filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r.cl.ok("_abhed/fs/rename", p("path", filepath.Join(ws, "link.txt"), "to", filepath.Join(ws, "link2.txt")), nil)
	if fi, err := os.Lstat(filepath.Join(ws, "link2.txt")); err != nil || fi.Mode()&os.ModeSymlink == 0 || r.read("target.txt") != "kept\n" {
		t.Fatalf("renaming a link moved its target: %v %v", fi, err)
	}
	r.cl.ok("_abhed/fs/delete", p("path", filepath.Join(ws, "link2.txt")), nil)
	if _, err := os.Lstat(filepath.Join(ws, "link2.txt")); err == nil || r.read("target.txt") != "kept\n" {
		t.Fatal("deleting a link deleted its target, or not the link")
	}
	// A folder holding a file the delete rules keep is not renamed away.
	r.write("box/keep.txt", "keep\n")
	r.write("box/other.txt", "x\n")
	r.cl.refused(errPolicy, "_abhed/fs/rename", p("path", filepath.Join(ws, "box"), "to", filepath.Join(ws, "moved")))
	if r.read("box/keep.txt") != "keep\n" {
		t.Fatal("renaming the folder moved a kept file")
	}
	// A delete is recorded as the person's edit, not a restore.
	var deleted bool
	for _, e := range r.events(id) {
		if e.Type == agent.EvManualEdit && strings.Contains(string(e.Payload), "link2.txt") {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("the delete was not recorded as manual.edit")
	}

	// A watch says what changed.
	from := r.cl.mark()
	r.cl.ok("_abhed/fs/watch", p("path", ws, "recursive", true, "id", "w1"), nil)
	time.Sleep(200 * time.Millisecond)
	r.write("watched.txt", "x\n")
	r.cl.waitFor(from, "a change", func(m rpcMessage) bool {
		return m.Method == "_abhed/fs/changed" && strings.Contains(string(m.Params), "watched.txt")
	})
	r.cl.ok("_abhed/fs/unwatch", p("id", "w1"), nil)

	// The SCM view's git: status and the HEAD version, read only.
	if _, err := exec.LookPath("git"); err == nil {
		for _, a := range [][]string{{"init", "-q"}, {"add", "a.txt"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "c"}} {
			cmd := exec.Command("git", append([]string{"-C", ws}, a...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", a, err, out)
			}
		}
		r.write("a.txt", "changed\n")
		var st struct {
			Changes []struct{ X, Y, Path string } `json:"changes"`
		}
		r.cl.ok("_abhed/fs/git", p("op", "status"), &st)
		var changed []string
		for _, c := range st.Changes {
			changed = append(changed, filepath.Base(c.Path)+c.X+c.Y)
		}
		if !slices.Contains(changed, "a.txt M") || slices.ContainsFunc(changed, func(s string) bool { return strings.HasPrefix(s, "secret.txt") }) {
			t.Fatalf("status %v", changed)
		}
		var orig struct{ Content string }
		r.cl.ok("_abhed/fs/git", p("op", "original", "path", filepath.Join(ws, "a.txt")), &orig)
		if b, _ := base64.StdEncoding.DecodeString(orig.Content); string(b) != "alpha\nedited\n" {
			t.Fatalf("original %q", b)
		}
		r.cl.refused(errPolicy, "_abhed/fs/git", p("op", "original", "path", filepath.Join(ws, "secret.txt")))

		// The reviewer's repro: a clean filter in the repository's configuration runs, if at all,
		// in the sandbox, never on the host; and GIT_DIR in the engine's environment changes nothing.
		marker := filepath.Join(outside, "filter-ran")
		cfg := exec.Command("git", "-C", ws, "config", "filter.x.clean", "touch "+marker+"; cat")
		if out, err := cfg.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		r.write(".gitattributes", "a.txt filter=x\n")
		r.write("a.txt", "changed again\n")
		other := t.TempDir()
		if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		t.Setenv("GIT_WORK_TREE", other)
		r.cl.call("_abhed/fs/git", p("op", "status"))
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a repository's clean filter ran on the host")
		}
		if line := gitLine("status"); !strings.Contains(line, "unset") || !strings.Contains(line, "'protocol.allow=never'") || !strings.Contains(line, "'--no-optional-locks'") {
			t.Fatalf("git line %s", line)
		}
	}
}

// With no sandbox, the SCM view's git runs only in a repository that
// configures no filter, diff or merge driver.
func TestGitDrivers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if d := gitDrivers(context.Background(), dir); d != "" {
		t.Fatalf("a plain repository has drivers %q", d)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "filter.x.clean", "touch /tmp/never").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if d := gitDrivers(context.Background(), dir); d != "filter.x.clean" {
		t.Fatalf("drivers %q", d)
	}
}

// The none tier wraps a command but holds nothing: the SCM view's git then
// gets the no-sandbox check (gitDrivers), not the wrapper.
func TestGitSandboxNoneTier(t *testing.T) {
	wrap := func(ctx context.Context, cwd, line string) *exec.Cmd { return exec.Command("true") }
	if gitSandbox(tools.Bash{Sandbox: wrap, Isolation: tools.Isolation{Tier: "none"}}) != nil {
		t.Fatal("the none tier counted as a sandbox")
	}
	if gitSandbox(tools.Bash{Sandbox: wrap, Isolation: tools.Isolation{Tier: "process"}, RanUnder: func() string { return "none" }}) != nil {
		t.Fatal("a sandbox chosen as none after start counted as one")
	}
	if gitSandbox(tools.Bash{Sandbox: wrap, Isolation: tools.Isolation{Tier: "process"}}) == nil {
		t.Fatal("the process tier did not count")
	}
}
