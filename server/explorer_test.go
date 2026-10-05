package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
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

// Rules on command text (the console denies rm flags) do not refuse the
// Explorer's actions, which are recorded under their own names, by the person.
func TestExplorerActionsAreNotJudgedAsCommandText(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny,
			"bash(rm -*)", "bash(* rm -*)", "bash(mv -*)", "bash(mkdir -*)")
	})
	wb.write("a.txt", "one\n")
	wb.write("old/inner.txt", "x\n")
	wb.write("gone.txt", "x\n")
	for _, step := range []struct {
		endpoint string
		body     any
	}{
		{"folder", folderRequest{Path: "new"}},
		{"rename", renameRequest{From: "a.txt", To: "new/a.txt"}},
		{"delete", folderRequest{Path: "gone.txt"}},
		{"delete", folderRequest{Path: "old"}},
	} {
		if rec := wb.send("acme", "POST", step.endpoint, step.body); rec.Code != http.StatusOK {
			t.Errorf("%s %v: %d %s", step.endpoint, step.body, rec.Code, rec.Body)
		}
	}
	for _, gone := range []string{"a.txt", "gone.txt", "old"} {
		if _, err := os.Lstat(filepath.Join(wb.workspace, gone)); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	calls := map[string]string{}
	byUser := map[string]bool{}
	for _, e := range wb.events() {
		var p struct {
			CallID string `json:"call_id"`
			Tool   string `json:"tool"`
			By     string `json:"by"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		switch {
		case e.Type == agent.EvActionRequested && e.Actor == agent.ActorUser:
			calls[p.CallID] = p.Tool
		case e.Type == agent.EvActionApproved && p.By == "user":
			byUser[calls[p.CallID]] = true
		}
	}
	for _, action := range []string{"mkdir", "rename", "delete"} {
		if !byUser[action] {
			t.Errorf("%s is not in the record as the person's own action: %v %v", action, calls, byUser)
		}
	}
}

// Rules on paths still hold for the Explorer's actions: a write rule and a
// rule naming the action both refuse a delete, and a rename rule sees the new name.
func TestExplorerDeleteStillMeetsPathRules(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "write(**/locked/**)", "delete(**/keep.txt)", "rename(**/final.txt)")
	})
	wb.write("locked/a.txt", "a\n")
	wb.write("keep.txt", "k\n")
	wb.write("draft.txt", "d\n")
	if rec := wb.send("acme", "POST", "rename", renameRequest{From: "draft.txt", To: "final.txt"}); rec.Code != http.StatusForbidden {
		t.Errorf("rename onto a name a rename rule denies: %d %s", rec.Code, rec.Body)
	}
	// The refusal is in the record, as the person's denied rename.
	renames, denied := map[string]bool{}, false
	for _, e := range wb.events() {
		var p struct {
			CallID string `json:"call_id"`
			Tool   string `json:"tool"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		switch e.Type {
		case agent.EvActionRequested:
			renames[p.CallID] = p.Tool == "rename"
		case agent.EvActionDenied:
			denied = denied || renames[p.CallID]
		}
	}
	if !denied {
		t.Error("the refused rename is not recorded as a denied action")
	}
	for _, p := range []string{"locked/a.txt", "locked", "keep.txt"} {
		if rec := wb.send("acme", "POST", "delete", folderRequest{Path: p}); rec.Code != http.StatusForbidden {
			t.Errorf("delete %s: %d %s", p, rec.Code, rec.Body)
		}
	}
	for _, kept := range []string{"locked/a.txt", "keep.txt"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was deleted against a rule", kept)
		}
	}
}

// A rule naming the action holds for what is inside a folder: deleting or
// renaming the folder, or any folder above it, is refused, and a rename is also
// judged by where each entry lands.
func TestExplorerActionRulesReachAFoldersContents(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "delete(**/keep/**)", "rename(**/locked/**)", "rename(**/pinned.txt)")
	})
	wb.write("keep/inner/k.txt", "k\n")
	wb.write("outer/keep/o.txt", "o\n")
	wb.write("a/b/keep/deep.txt", "d\n")
	wb.write("locked/l.txt", "l\n")
	wb.write("infra/mods/locked/m.tf", "m\n")
	wb.write("drafts/notes/pinned.txt", "p\n")
	wb.write("free/f.txt", "f\n")
	wb.write("archive/a.txt", "a\n")

	for name, c := range map[string]struct {
		endpoint string
		body     any
	}{
		"delete the named folder":        {"delete", folderRequest{Path: "keep"}},
		"delete a folder holding it":     {"delete", folderRequest{Path: "outer"}},
		"delete a distant ancestor":      {"delete", folderRequest{Path: "a"}},
		"rename the named folder":        {"rename", renameRequest{From: "locked", To: "unlocked"}},
		"rename an ancestor":             {"rename", renameRequest{From: "infra", To: "infra2"}},
		"rename a folder holding a file": {"rename", renameRequest{From: "drafts", To: "old"}},
		"contents land on a denied name": {"rename", renameRequest{From: "free", To: "archive/locked"}},
	} {
		if rec := wb.send("acme", "POST", c.endpoint, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	for _, kept := range []string{"keep/inner/k.txt", "outer/keep/o.txt", "a/b/keep/deep.txt", "locked/l.txt",
		"infra/mods/locked/m.tf", "drafts/notes/pinned.txt", "free/f.txt"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was moved or removed against a rule", kept)
		}
	}
	// A folder with nothing the rules name still goes.
	wb.write("scratch/s.txt", "s\n")
	if rec := wb.send("acme", "POST", "delete", folderRequest{Path: "scratch"}); rec.Code != http.StatusOK {
		t.Fatalf("an unrestricted folder: %d %s", rec.Code, rec.Body)
	}
}

// A folder rename puts the rule to each entry where it is and where it lands,
// separately: a rule on either location alone refuses the move.
func TestExplorerRenameJudgesContentsAtBothEnds(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "rename(**/src/locked/**)", "rename(**/archive/**/*.tf)")
	})
	wb.write("src/locked/x.txt", "x\n")
	wb.write("mods/m.tf", "m\n")
	wb.write("archive/a.txt", "a\n")
	if rec := wb.send("acme", "POST", "rename", renameRequest{From: "src", To: "dst"}); rec.Code != http.StatusForbidden {
		t.Errorf("a rule on where the contents are: %d %s", rec.Code, rec.Body)
	}
	if rec := wb.send("acme", "POST", "rename", renameRequest{From: "mods", To: "archive/mods"}); rec.Code != http.StatusForbidden {
		t.Errorf("a rule on where the contents land: %d %s", rec.Code, rec.Body)
	}
	for _, kept := range []string{"src/locked/x.txt", "mods/m.tf"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was moved against a rule", kept)
		}
	}
}

// A folder's contents are judged with their folder's links followed, and a
// folder inside is judged by its trailing-separator form, as rm sees them.
func TestExplorerContentsJudgedAsRmSeesThem(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "delete(**/realdir/secret/*.txt)", "delete(**/vault/)")
	})
	wb.write("realdir/secret/s.txt", "s\n")
	if err := os.Symlink("realdir", filepath.Join(wb.workspace, "alias")); err != nil {
		t.Fatal(err)
	}
	wb.write("box/vault/v.txt", "v\n")
	if rec := wb.send("acme", "POST", "delete", folderRequest{Path: "alias/secret"}); rec.Code != http.StatusForbidden {
		t.Errorf("a rule on the linked folder's real path: %d %s", rec.Code, rec.Body)
	}
	if rec := wb.send("acme", "POST", "delete", folderRequest{Path: "box"}); rec.Code != http.StatusForbidden {
		t.Errorf("a rule on a folder inside by its trailing /: %d %s", rec.Code, rec.Body)
	}
	for _, kept := range []string{"realdir/secret/s.txt", "box/vault/v.txt"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was removed against a rule", kept)
		}
	}
}

// Rules written relative to the workspace, or with a leading ./, bind the
// Explorer as the absolute paths it acts on.
func TestExplorerMeetsWorkspaceRelativeRules(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "delete(apps/**/vault/**)",
			"rename(./infra/**/pinned/**)", "write(docs/**/frozen/**)", "read(secrets/*)")
	})
	wb.write("apps/mobile [beta]/vault/secret.txt", "s\n")
	wb.write("infra/terraform/modules/pinned/p.tf", "p\n")
	wb.write("docs/user guide/frozen/f.md", "f\n")
	wb.write("secrets/key", "k\n")
	wb.write("free.txt", "y\n")

	for name, c := range map[string]struct {
		endpoint string
		body     any
	}{
		"delete under a delete rule":  {"delete", folderRequest{Path: "apps/mobile [beta]/vault/secret.txt"}},
		"rename under a rename rule":  {"rename", renameRequest{From: "infra/terraform/modules/pinned/p.tf", To: "p.tf"}},
		"delete under a write rule":   {"delete", folderRequest{Path: "docs/user guide/frozen/f.md"}},
		"folder holding a write rule": {"delete", folderRequest{Path: "docs/user guide"}},
		"rename into a write rule":    {"rename", renameRequest{From: "free.txt", To: "docs/user guide/frozen/free.txt"}},
		"rename out of a read rule":   {"rename", renameRequest{From: "secrets/key", To: "key.txt"}},
	} {
		if rec := wb.send("acme", "POST", c.endpoint, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	for _, kept := range []string{"apps/mobile [beta]/vault/secret.txt", "infra/terraform/modules/pinned/p.tf",
		"docs/user guide/frozen/f.md", "secrets/key", "free.txt"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was changed against a rule", kept)
		}
	}
	if rec, _ := wb.file("secrets/key"); rec.Code == http.StatusOK {
		t.Error("a file under a relative read rule was served")
	}
}

// With an added directory, a relative rule binds the live session in that
// directory as well as the Explorer in the workspace.
func TestRelativeRulesBindAnAddedDirectory(t *testing.T) {
	extra := t.TempDir()
	wb := manualBench(t, func(c *config.Config) {
		c.AdditionalDirs = append(c.AdditionalDirs, extra)
		c.Permissions.Deny = append(c.Permissions.Deny, "write(notes/**)", "delete(notes/**)")
	})
	wb.write("notes/a.md", "a\n")
	if rec := wb.send("acme", "POST", "delete", folderRequest{Path: "notes/a.md"}); rec.Code != http.StatusForbidden {
		t.Errorf("explorer delete under a relative rule: %d %s", rec.Code, rec.Body)
	}
	wb.s.mu.Lock()
	live := wb.s.running[wb.session]
	wb.s.mu.Unlock()
	if live == nil {
		t.Fatal("no live session")
	}
	target := filepath.Join(extra, "notes/x.md")
	args, _ := json.Marshal(map[string]string{"path": target, "content": "x"})
	res, err := live.Loop.Manual(context.Background(), live.Loop.Session, "write", "rel-1", args)
	if err != nil || !res.IsError {
		t.Errorf("a write in the added directory under a relative rule ran: %+v %v", res, err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the file was written")
	}
}

// A rename removes the entry from its old path, so a delete rule on the old
// path, or on anything inside it, refuses or asks for the rename as well.
func TestExplorerRenameMeetsDeleteRules(t *testing.T) {
	for _, c := range []struct {
		name     string
		deny     string
		ask      string
		from, to string
		want     int
		reason   string
	}{
		{name: "rename vault", deny: "delete(**/vault/**)", from: "vault", to: "open", want: http.StatusForbidden},
		{name: "rename a parent holding vault", deny: "delete(**/vault/**)", from: "outer", to: "outer2", want: http.StatusForbidden},
		{name: "move a file out of vault", deny: "delete(**/vault/**)", from: "vault/v.txt", to: "v.txt", want: http.StatusForbidden},
		{name: "no rule", from: "vault", to: "open", want: http.StatusOK},
		{name: "a delete ask is recorded as the rename's reason", ask: "delete(**/vault/**)", from: "vault/v.txt", to: "v.txt",
			want: http.StatusOK, reason: "matched ask rule delete(**/vault/**)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			wb := manualBench(t, func(cfg *config.Config) {
				if c.deny != "" {
					cfg.Permissions.Deny = append(cfg.Permissions.Deny, c.deny)
				}
				if c.ask != "" {
					cfg.Permissions.Ask = append(cfg.Permissions.Ask, c.ask)
				}
			})
			wb.write("vault/v.txt", "v\n")
			wb.write("outer/vault/o.txt", "o\n")
			rec := wb.send("acme", "POST", "rename", renameRequest{From: c.from, To: c.to})
			if rec.Code != c.want {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, c.want)
			}
			if c.want != http.StatusOK {
				for _, kept := range []string{"vault/v.txt", "outer/vault/o.txt"} {
					if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
						t.Errorf("%s was moved against a rule", kept)
					}
				}
				if !strings.Contains(rec.Body.String(), c.deny) {
					t.Errorf("the refusal does not name the rule: %s", rec.Body)
				}
			}
			if c.reason == "" {
				return
			}
			found := false
			for _, e := range wb.events() {
				var p struct {
					Tool   string `json:"tool"`
					Reason string `json:"reason"`
				}
				_ = json.Unmarshal(e.Payload, &p)
				found = found || (e.Type == agent.EvActionRequested && p.Tool == "rename" && p.Reason == c.reason)
			}
			if !found {
				t.Errorf("the rename is not recorded as asked by %q", c.reason)
			}
		})
	}
}

// A policy hook sees a rename's old path as a delete, so a hook that refuses
// deleting a path refuses moving it away.
func TestExplorerRenameReachesHooksAsDelete(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("vault/v.txt", "v\n")
	wb.write("free.txt", "f\n")
	wb.s.mu.Lock()
	live := wb.s.running[wb.session]
	wb.s.mu.Unlock()
	if live == nil {
		t.Fatal("no live session")
	}
	vault := string(filepath.Separator) + "vault" + string(filepath.Separator)
	live.Loop.Policy.Hooks = append(live.Loop.Policy.Hooks, func(tool string, args json.RawMessage) *policy.Result {
		var a struct{ Path string }
		_ = json.Unmarshal(args, &a)
		if tool == "delete" && strings.Contains(a.Path, vault) {
			return &policy.Result{Decision: policy.Deny, Reason: "hook keeps the vault"}
		}
		return nil
	})
	if rec := wb.send("acme", "POST", "rename", renameRequest{From: "vault/v.txt", To: "v.txt"}); rec.Code != http.StatusForbidden {
		t.Errorf("a rename out of what the hook keeps: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(wb.workspace, "vault/v.txt")); err != nil {
		t.Error("the file was moved against the hook")
	}
	if rec := wb.send("acme", "POST", "rename", renameRequest{From: "free.txt", To: "free2.txt"}); rec.Code != http.StatusOK {
		t.Errorf("a rename the hook does not name: %d %s", rec.Code, rec.Body)
	}
}

// On a disk that folds case, core/VAULT opens core/vault: every spelling of a
// folder a rule keeps is refused, for each Explorer action.
func TestExplorerRulesHoldForEveryCaseTheDiskOpens(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "delete(**/vault/**)", "write(**/frozen/**)")
	})
	wb.write("svc/core/vault/keys/master.txt", "keep\n")
	wb.write("ops/frozen/deep/f.txt", "keep\n")
	wb.write("free.txt", "f\n")
	if _, err := os.Stat(filepath.Join(wb.workspace, "FREE.TXT")); err != nil {
		t.Skip("this disk keeps case, so another spelling is another path")
	}
	for name, c := range map[string]struct {
		endpoint string
		body     any
	}{
		"delete VAULT":           {"delete", folderRequest{Path: "svc/core/VAULT"}},
		"delete through CORE":    {"delete", folderRequest{Path: "svc/CORE/Vault/keys"}},
		"rename Vault":           {"rename", renameRequest{From: "svc/core/Vault", To: "svc/core/open"}},
		"move a file out":        {"rename", renameRequest{From: "svc/core/VAULT/keys/master.txt", To: "master.txt"}},
		"rename FROZEN":          {"rename", renameRequest{From: "ops/FROZEN", To: "ops/thawed"}},
		"new folder under FROZE": {"folder", folderRequest{Path: "ops/FROZEN/newdir"}},
		"delete in Frozen":       {"delete", folderRequest{Path: "ops/Frozen/deep"}},
		"move a file into it":    {"rename", renameRequest{From: "free.txt", To: "ops/fRoZeN/free.txt"}},
	} {
		if rec := wb.send("acme", "POST", c.endpoint, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	for _, kept := range []string{"svc/core/vault/keys/master.txt", "ops/frozen/deep/f.txt", "free.txt"} {
		if _, err := os.Stat(filepath.Join(wb.workspace, kept)); err != nil {
			t.Errorf("%s was changed against a rule", kept)
		}
	}
	for _, made := range []string{"svc/core/open", "master.txt", "ops/thawed", "ops/frozen/newdir"} {
		if _, err := os.Lstat(filepath.Join(wb.workspace, made)); err == nil {
			t.Errorf("%s was made against a rule", made)
		}
	}
}

// A change a write rule refuses is in the record, as the person's denied
// action with the rule that denied it, as a delete rule's refusal is.
func TestExplorerWriteRuleRefusalsAreRecorded(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "write(**/frozen/**)")
	})
	wb.write("ops/frozen/deep/f.txt", "keep\n")
	wb.write("free.txt", "f\n")
	for _, c := range []struct {
		endpoint, action string
		body             any
	}{
		{"folder", "mkdir", folderRequest{Path: "ops/frozen/newdir"}},
		{"rename", "rename", renameRequest{From: "ops/frozen", To: "ops/thawed"}},
		{"rename", "rename", renameRequest{From: "free.txt", To: "ops/frozen/free.txt"}},
		{"delete", "delete", folderRequest{Path: "ops/frozen/deep"}},
	} {
		before := len(wb.events())
		if rec := wb.send("acme", "POST", c.endpoint, c.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s, want 403", c.action, rec.Code, rec.Body)
		}
		evs := wb.events()[before:]
		var id string
		denied := false
		for _, e := range evs {
			var p struct {
				CallID string `json:"call_id"`
				Tool   string `json:"tool"`
				Reason string `json:"reason"`
				By     string `json:"by"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			switch {
			case e.Type == agent.EvActionRequested && e.Actor == agent.ActorUser && p.Tool == c.action:
				id = p.CallID
			case e.Type == agent.EvActionDenied && id != "" && p.CallID == id:
				denied = p.By == agent.ByPolicy && strings.Contains(p.Reason, "write(**/frozen/**)")
			}
		}
		if id == "" || !denied {
			t.Errorf("the refused %s %+v is not recorded as a policy denial: %d new events", c.action, c.body, len(evs))
		}
	}
}

// With two servers on one store, an Explorer change on the server that does
// not hold the session answers 409, as a save does, and changes nothing; so
// does one a write rule refuses, which that server cannot record.
func TestExplorerOnTheServerNotHoldingTheSessionIsRefused(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "write(**/frozen/**)")
	dir := t.TempDir()
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	node := func() (*Server, http.Handler) {
		s := New(Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{},
			Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{}), Store: st})
		return s, s.Handler()
	}
	a, ha := node()
	_, hb := node()
	do := func(h http.Handler, endpoint string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", endpoint, strings.NewReader(string(raw)))
		r.Header.Set("X-Abhed-User", "alice")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var created createResponse
	if w := do(ha, "/v1/sessions", map[string]string{"prompt": "work"}); json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	id := created.SessionID
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		st.mu.Lock()
		ended := st.ended[id]
		st.mu.Unlock()
		if ended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first turn did not end")
		}
	}
	a.mu.Lock()
	delete(a.running, id) // as a restart would leave it
	a.mu.Unlock()
	for _, f := range []string{"keep.txt", "old/inner.txt"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := "/v1/sessions/" + id + "/"
	// B opens the session to view it; then A's first change claims it.
	if w := do(hb, base+"accept", map[string]string{"path": "none.txt", "content": ""}); w.Code == http.StatusConflict {
		t.Fatalf("B could not open the finished session: %s", w.Body)
	}
	if w := do(ha, base+"folder", folderRequest{Path: "held"}); w.Code != http.StatusOK {
		t.Fatalf("A's change: %d %s", w.Code, w.Body)
	}
	for name, c := range map[string]struct {
		endpoint string
		body     any
	}{
		"mkdir":                    {"folder", folderRequest{Path: "fromb"}},
		"rename":                   {"rename", renameRequest{From: "keep.txt", To: "moved.txt"}},
		"delete":                   {"delete", folderRequest{Path: "old"}},
		"mkdir under a write rule": {"folder", folderRequest{Path: "frozen/x"}},
		"rename into a write rule": {"rename", renameRequest{From: "keep.txt", To: "frozen/keep.txt"}},
	} {
		w := do(hb, base+c.endpoint, c.body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "continued elsewhere") {
			t.Errorf("%s on B while A holds the session: %d %s, want 409", name, w.Code, w.Body)
		}
	}
	for _, kept := range []string{"keep.txt", "old/inner.txt", "held"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s is gone", kept)
		}
	}
	for _, made := range []string{"fromb", "moved.txt", "frozen"} {
		if _, err := os.Lstat(filepath.Join(dir, made)); err == nil {
			t.Errorf("%s was made by the server not holding the session", made)
		}
	}
}

// A write rule written against a link to a folder holds for what is saved,
// uploaded or changed through the link, not only for the folder it leads to.
func TestWriteRuleOnALinkedFolderHoldsThroughIt(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "write(**/a/**)")
	})
	wb.write("open/keep.txt", "keep\n")
	wb.write("free.txt", "y")
	if err := os.Symlink(filepath.Join(wb.workspace, "open"), filepath.Join(wb.workspace, "a")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	keep := contentHash([]byte("keep\n"))
	for name, c := range map[string]struct {
		method, endpoint string
		body             any
	}{
		"save through the link":   {"PUT", "file", saveRequest{Path: "a/keep.txt", Content: "changed\n", Base: keep}},
		"new file through it":     {"PUT", "file", saveRequest{Path: "a/new.txt", Content: "new\n"}},
		"delete through the link": {"POST", "delete", folderRequest{Path: "a/keep.txt"}},
		"rename into it":          {"POST", "rename", renameRequest{From: "free.txt", To: "a/free.txt"}},
		"new folder in it":        {"POST", "folder", folderRequest{Path: "a/sub"}},
	} {
		if rec := wb.send("acme", c.method, c.endpoint, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := wb.dropFile("acme", ptr("a"), "up.txt", []byte("up\n")); rec.Code != http.StatusForbidden {
		t.Errorf("upload into the link: %d %s", rec.Code, rec.Body)
	}
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "open/keep.txt")); string(got) != "keep\n" {
		t.Fatalf("a file was changed through a write-denied link: %q", got)
	}
	for _, p := range []string{"open/new.txt", "open/free.txt", "open/sub", "open/up.txt"} {
		if _, err := os.Lstat(filepath.Join(wb.workspace, p)); err == nil {
			t.Errorf("%s was made through a write-denied link", p)
		}
	}
	// The folder's own spelling is still open.
	if rec := wb.send("acme", "PUT", "file", saveRequest{Path: "open/keep.txt", Content: "ok\n", Base: keep}); rec.Code != http.StatusOK {
		t.Fatalf("a save to the folder itself: %d %s", rec.Code, rec.Body)
	}
}
