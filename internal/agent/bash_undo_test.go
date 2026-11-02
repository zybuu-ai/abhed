package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// /undo takes back what a bash command did: a changed file gets its content
// back, a removed one returns, and one the command made is removed.
func TestUndoTakesBackABashCommand(t *testing.T) {
	dir := tempDir(t)
	sess, _ := tools.NewSession(dir)
	undo := NewUndoLog(sess.RestoreFile, sess.RemoveFile)
	sess.Checkpoint, sess.BashCheckpoint = undo.Record, undo.RecordBefore
	a, b, c := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt"), filepath.Join(dir, "c.txt")
	_ = os.WriteFile(a, []byte("a before"), 0o600)
	_ = os.WriteFile(b, []byte("b before"), 0o600)
	undo.BeginTurn()
	args, _ := json.Marshal(map[string]string{"command": "echo after > a.txt; rm b.txt; echo new > c.txt", "description": "change"})
	if res := (tools.Bash{}).Run(context.Background(), sess, args); res.IsError {
		t.Fatal(res.Content)
	}
	if got := undo.Changed(); len(got) != 3 {
		t.Fatalf("changed %v", got)
	}
	if _, err := undo.Undo(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{a: "a before", b: "b before"} {
		if got, _ := os.ReadFile(p); string(got) != want {
			t.Errorf("%s = %q", filepath.Base(p), got)
		}
	}
	if _, err := os.Stat(c); !os.IsNotExist(err) {
		t.Error("the made file is still there")
	}
}

// A file the snapshot could not keep is named, not written empty, and has
// no baseline to diff against.
func TestUndoNamesABashChangeWithNoCopy(t *testing.T) {
	dir := tempDir(t)
	sess, _ := tools.NewSession(dir)
	undo := NewUndoLog(sess.RestoreFile, sess.RemoveFile)
	big := filepath.Join(dir, "big.bin")
	_ = os.WriteFile(big, []byte("now"), 0o600)
	undo.RecordBefore(tools.FileBefore{Path: big, Existed: true, Skipped: "it was over the limit"})
	if _, _, known := undo.Original(big); known {
		t.Fatal("a file with no copy has a baseline")
	}
	_, err := undo.Undo()
	if err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Fatalf("err %v", err)
	}
	if got, _ := os.ReadFile(big); string(got) != "now" {
		t.Fatalf("big.bin = %q", got)
	}
}
