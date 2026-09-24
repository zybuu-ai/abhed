package server

import (
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
