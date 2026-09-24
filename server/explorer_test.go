package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// A folder made, a file renamed and a folder deleted from the Explorer each
// land on disk and in the record as the person's own command.
func TestExplorerChangesAreRecordedAsThePersonsOwn(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("a.txt", "one\n")
	wb.write("old/inner.txt", "x\n")

	for _, step := range []struct {
		endpoint string
		body     any
	}{
		{"folder", folderRequest{Path: "new/deeper"}},
		{"rename", renameRequest{From: "a.txt", To: "new/b's.txt"}},
		{"delete", folderRequest{Path: "old"}},
	} {
		if rec := wb.send("acme", "POST", step.endpoint, step.body); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.endpoint, rec.Code, rec.Body)
		}
	}
	if info, err := os.Stat(filepath.Join(wb.workspace, "new/deeper")); err != nil || !info.IsDir() {
		t.Fatalf("folder not made: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "new/b's.txt")); string(got) != "one\n" {
		t.Fatalf("rename lost the content: %q", got)
	}
	for _, gone := range []string{"a.txt", "old"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, gone)); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}

	byUser := map[string]bool{}
	for _, e := range wb.events() {
		if e.Type == agent.EvActionRequested && e.Actor == agent.ActorUser {
			for _, verb := range []string{"mkdir -p", "mv -n", "rm -r"} {
				if strings.Contains(string(e.Payload), verb) {
					byUser[verb] = true
				}
			}
		}
	}
	if len(byUser) != 3 {
		t.Fatalf("each change must be in the record as the person's command: %v", byUser)
	}
}

// The Explorer holds no power a save lacks: write and read rules, the server's
// state, the workspace root and paths outside it all refuse, and a folder is
// refused when anything inside it may not be written.
func TestExplorerIsHeldToTheSameRules(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "write(**/locked/**)", "read(**/.env)")
	})
	wb.write("outer/locked/keep.txt", "keep\n")
	wb.write(".env", "SECRET=1\n")
	wb.write(".abhed/config.json", "{}")
	wb.write("taken.txt", "x")
	wb.write("free.txt", "y")

	for name, c := range map[string]struct {
		endpoint string
		body     any
		want     int
	}{
		"folder holding a denied path": {"delete", folderRequest{Path: "outer"}, http.StatusForbidden},
		"rename into a denied folder":  {"rename", renameRequest{From: "free.txt", To: "outer/locked/free.txt"}, http.StatusForbidden},
		"new folder under a denied":    {"folder", folderRequest{Path: "outer/locked/sub"}, http.StatusForbidden},
		"read-denied file":             {"rename", renameRequest{From: ".env", To: "env.txt"}, http.StatusForbidden},
		"server state":                 {"delete", folderRequest{Path: ".abhed"}, http.StatusNotFound},
		"the workspace itself":         {"delete", folderRequest{Path: "."}, http.StatusNotFound},
		"traversal":                    {"folder", folderRequest{Path: "../escaped"}, http.StatusNotFound},
		"absolute":                     {"delete", folderRequest{Path: filepath.Join(wb.workspace, "free.txt")}, http.StatusNotFound},
		"over an existing file":        {"rename", renameRequest{From: "free.txt", To: "taken.txt"}, http.StatusConflict},
		"missing":                      {"delete", folderRequest{Path: "nothing.txt"}, http.StatusNotFound},
	} {
		if rec := wb.send("acme", "POST", c.endpoint, c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
	}
	for _, kept := range []string{"outer/locked/keep.txt", ".env", ".abhed/config.json", "taken.txt", "free.txt"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was changed by a refused request", kept)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(wb.workspace), "escaped")); err == nil {
		t.Error("a folder was made outside the workspace")
	}
	if rec := wb.send("other", "POST", "delete", folderRequest{Path: "free.txt"}); rec.Code == http.StatusOK {
		t.Fatalf("another tenant deleted from this session: %s", rec.Body)
	}
}

// Deleting a link removes the link, never the file it points at.
func TestExplorerDeletesALinkNotItsTarget(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("real.txt", "keep\n")
	if err := os.Symlink(filepath.Join(wb.workspace, "real.txt"), filepath.Join(wb.workspace, "link.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if rec := wb.send("acme", "POST", "delete", folderRequest{Path: "link.txt"}); rec.Code != http.StatusOK {
		t.Fatalf("delete link: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Lstat(filepath.Join(wb.workspace, "link.txt")); err == nil {
		t.Error("the link is still there")
	}
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "real.txt")); string(got) != "keep\n" {
		t.Fatalf("the link's target was touched: %q", got)
	}
}

// A folder reached through a link is judged where the link leads: the write
// rules hold for the target's own path, not only for the spelling sent.
func TestExplorerJudgesThePathALinkLeadsTo(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "write(**/locked/**)")
	})
	wb.write("locked/keep.txt", "keep\n")
	wb.write("free.txt", "y")
	if err := os.Symlink(filepath.Join(wb.workspace, "locked"), filepath.Join(wb.workspace, "a")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	for name, c := range map[string]struct {
		endpoint string
		body     any
	}{
		"delete through the link": {"delete", folderRequest{Path: "a/keep.txt"}},
		"rename out of it":        {"rename", renameRequest{From: "a/keep.txt", To: "out.txt"}},
		"rename into it":          {"rename", renameRequest{From: "free.txt", To: "a/free.txt"}},
		"new folder in it":        {"folder", folderRequest{Path: "a/sub"}},
	} {
		if rec := wb.send("acme", "POST", c.endpoint, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "locked/keep.txt")); string(got) != "keep\n" {
		t.Fatal("a file under a write-denied folder was changed through a link")
	}
	for _, p := range []string{"locked/free.txt", "locked/sub", "out.txt"} {
		if _, err := os.Lstat(filepath.Join(wb.workspace, p)); err == nil {
			t.Errorf("%s was made through a link", p)
		}
	}
}

// Moving or deleting a folder answers for everything in it: a read-denied file
// cannot be moved out from under its rule, a repository inside cannot be
// deleted, and a folder cannot land its contents on write-denied paths.
func TestExplorerJudgesAFoldersContents(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "read(**/cfg/prod.key)", "write(**/prod/config.yaml)")
	})
	wb.write("cfg/prod.key", "secret\n")
	wb.write("sub/.git/HEAD", "ref: refs/heads/main\n")
	wb.write("tmp/config.yaml", "x: 1\n")

	for name, c := range map[string]struct {
		endpoint string
		body     any
	}{
		"read-denied inside":        {"rename", renameRequest{From: "cfg", To: "open"}},
		"repository inside":         {"delete", folderRequest{Path: "sub"}},
		"contents onto denied path": {"rename", renameRequest{From: "tmp", To: "prod"}},
	} {
		rec := wb.send("acme", "POST", c.endpoint, c.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "this folder holds") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	for _, kept := range []string{"cfg/prod.key", "sub/.git/HEAD", "tmp/config.yaml"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was moved or removed", kept)
		}
	}
	if rec, _ := wb.file("open/prod.key"); rec.Code == http.StatusOK {
		t.Fatal("a read-denied file became readable at a new path")
	}
}

// A folder too large to check is refused, not changed unchecked.
func TestExplorerRefusesAFolderTooLargeToCheck(t *testing.T) {
	wb := manualBench(t, nil)
	for i := range maxExplorerEntries + 1 {
		if err := os.WriteFile(filepath.Join(wb.workspace, "big", fmt.Sprintf("%05d", i)), nil, 0o644); err != nil {
			if i == 0 {
				wb.write("big/00000", "")
				continue
			}
			t.Fatal(err)
		}
	}
	if rec := wb.send("acme", "POST", "delete", folderRequest{Path: "big"}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(wb.workspace, "big/00000")); err != nil {
		t.Fatal("a folder too large to check was deleted")
	}
}

// A rename that lost the race for its new name is a failure, even though the
// old name is gone and something now stands at the new one: mv -n put the
// entry inside the folder that took the name.
func TestExplorerRenameCountsOnlyTheEntryItMoved(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Mkdir(from, 0o755); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Lstat(from)
	if err := os.Mkdir(to, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, filepath.Join(to, "a")); err != nil {
		t.Fatal(err)
	}
	if moved(before, from, to) {
		t.Fatal("a folder that took the name first passed for the moved one")
	}
	if err := os.Rename(filepath.Join(to, "a"), filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	if !moved(before, from, filepath.Join(dir, "c")) {
		t.Fatal("a real move was not recognised")
	}
}
