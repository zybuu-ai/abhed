package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// recordHome gives a test its own home with a record holding sessions for
// workspace ws, and returns the store (closed) and the session ids.
func recordHome(t *testing.T, ws string, prompts ...string) []string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	rec, err := openRecord(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	var ids []string
	for _, p := range prompts {
		id := newConversationID()
		if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: ws, User: cliUser()}); err != nil {
			t.Fatal(err)
		}
		r := agent.NewRecorder(rec, id, "")
		_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: p})
		_, _ = r.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "done: " + p})
		_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted, Turns: 1})
		ids = append(ids, id)
	}
	return ids
}

func runRecord(t *testing.T, ws string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := recordCmd(ws, args, config.TrustRefused, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestRecordListShowVerify(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "fix the login bug", "write the docs")

	code, out, _ := runRecord(t, ws, "list")
	if code != 0 || !strings.Contains(out, ids[0]) || !strings.Contains(out, "write the docs") {
		t.Fatalf("list: %d %s", code, out)
	}
	if strings.Index(out, ids[1]) > strings.Index(out, ids[0]) {
		t.Fatalf("newest first: %s", out)
	}
	if code, out, _ := runRecord(t, t.TempDir(), "list"); code != 0 || strings.Contains(out, ids[0]) {
		t.Fatalf("another workspace's list: %s", out)
	}
	code, out, _ = runRecord(t, ws, "show", ids[0][:8])
	if code != 0 || !strings.Contains(out, "fix the login bug") || !strings.Contains(out, "session.ended") {
		t.Fatalf("show: %d %s", code, out)
	}
	code, out, _ = runRecord(t, ws, "verify")
	if code != 0 || strings.Count(out, "ok ") != 3 || !strings.Contains(out, "not proof against the machine's owner") {
		t.Fatalf("verify: %d %s", code, out)
	}
}

func TestRecordVerifyFailsOnATamperedLine(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "the original words")
	dir, _ := local.DefaultDir()
	p := filepath.Join(dir, "default", ids[0]+".jsonl")
	data, _ := os.ReadFile(p)
	_ = os.WriteFile(p, bytes.Replace(data, []byte("original"), []byte("replaced"), 1), 0o600)

	code, out, _ := runRecord(t, ws, "verify", ids[0])
	if code != 1 || !strings.Contains(out, "FAILED") || !strings.Contains(out, "at seq 1") {
		t.Fatalf("verify: %d %s", code, out)
	}
	if code, _, _ := runRecord(t, ws, "verify"); code != 1 {
		t.Fatal("verify of everything passed over a tampered session")
	}
	// The file named by its path is checked in the record, with its head.
	if code, out, _ := runRecord(t, ws, "verify", p); code != 1 || !strings.Contains(out, "FAILED") {
		t.Fatalf("verify by path: %d %s", code, out)
	}
}

func TestRecordExportAndVerifyTheCopy(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "export me")
	code, _, errOut := runRecord(t, ws, "export", ids[0])
	if code != 0 {
		t.Fatalf("export: %s", errOut)
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".abhed", "exports", ids[0]+".jsonl")
	if !strings.Contains(errOut, p) {
		t.Fatalf("export went to %s, want %s", errOut, p)
	}
	if entries, _ := os.ReadDir(ws); len(entries) != 0 {
		t.Fatal("the export was written into the workspace")
	}
	if code, out, _ := runRecord(t, ws, "verify", p); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("verify the export: %d %s", code, out)
	}
	for _, f := range []string{"html", "txt"} {
		out := filepath.Join(t.TempDir(), "x."+f)
		if code, _, e := runRecord(t, ws, "export", ids[0], "-format", f, "-o", out); code != 0 {
			t.Fatalf("export %s: %s", f, e)
		}
		if data, _ := os.ReadFile(out); !bytes.Contains(data, []byte("export me")) {
			t.Fatalf("%s export: %s", f, data)
		}
	}
}

func TestRecordPruneNeedsConfirmAndLeavesATombstone(t *testing.T) {
	ws := t.TempDir()
	ids := recordHome(t, ws, "old work", "keep this")
	if code, _, errOut := runRecord(t, ws, "prune", ids[0]); code == 0 || !strings.Contains(errOut, "-yes") {
		t.Fatalf("prune without a terminal or -yes: %d %s", code, errOut)
	}
	code, out, _ := runRecord(t, ws, "prune", ids[0], "-yes")
	if code != 0 || !strings.Contains(out, "tombstone") {
		t.Fatalf("prune: %d %s", code, out)
	}
	if _, out, _ := runRecord(t, ws, "list"); strings.Contains(out, ids[0]) || !strings.Contains(out, ids[1]) {
		t.Fatalf("list after prune: %s", out)
	}
	if code, out, _ := runRecord(t, ws, "verify"); code != 0 {
		t.Fatalf("verify after prune: %s", out)
	}
	if code, out, _ := runRecord(t, ws, "prune", "-older-than", "30d", "-yes"); code != 0 || !strings.Contains(out, "nothing to prune") {
		t.Fatalf("nothing old enough: %d %s", code, out)
	}
	if code, _, _ := runRecord(t, ws, "prune"); code != 2 {
		t.Fatal("prune with nothing named")
	}
}
