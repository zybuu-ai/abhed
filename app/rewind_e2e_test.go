package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func (g *sessRig) file(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(g.ws, name))
	return string(b), err == nil
}

func count(evs []agent.Event, t agent.EventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == t {
			n++
		}
	}
	return n
}

// Three turns with edits, then a rewind of code and conversation to before
// the second: the files are as they were, the next prompt does not see the
// abandoned turns, and the record holds the fork and each restore, and
// verifies.
func TestRewindCodeAndConversation(t *testing.T) {
	g := newSessRig(t)
	c := g.start("-mode", "accept-edits")
	g.ask(c, "write a.txt=one")
	g.ask(c, "write a.txt=two")
	g.ask(c, "write b.txt=new")
	if a, _ := g.file("a.txt"); a != "two" {
		t.Fatalf("a.txt = %q before the rewind", a)
	}
	c.command("/rewind 2", "Rewind to before")
	fmt.Fprintln(c.stdin, "both")
	c.waitFor(func(out string) bool { return strings.Contains(out, "the conversation is back") }, "the rewind")
	if a, _ := g.file("a.txt"); a != "one" {
		t.Fatalf("a.txt = %q after the rewind, want one", a)
	}
	if _, ok := g.file("b.txt"); ok {
		t.Fatal("b.txt, made after the point, is still there")
	}
	if body := g.ask(c, "What happened?"); strings.Contains(body, "a.txt=two") || !strings.Contains(body, "a.txt=one") {
		t.Fatalf("the conversation after the rewind:\n%s", body)
	}
	exit(c)
	id := g.sessions()[0].ID
	evs := verified(t, g.record(), id)
	if count(evs, agent.EvForked) != 1 || count(evs, agent.EvFileRestored) != 2 || count(evs, agent.EvCheckpoint) != 3 {
		t.Fatalf("record: %d forks, %d restores, %d checkpoints", count(evs, agent.EvForked), count(evs, agent.EvFileRestored), count(evs, agent.EvCheckpoint))
	}
	// Nothing was deleted: the abandoned prompts are still in the record.
	texts := ""
	for _, e := range evs {
		if e.Type == agent.EvUserMessage {
			texts += string(e.Payload)
		}
	}
	if !strings.Contains(texts, "a.txt=two") || !strings.Contains(texts, "b.txt=new") {
		t.Fatal("the abandoned prompts left the record")
	}
}

// Rewinding to the first prompt is a fork at 0 in the same session, never
// an unrecorded new one.
func TestRewindToTheFirstPrompt(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "Remember the codeword ZEBRA-41.")
	g.ask(c, "And OSPREY-58.")
	c.command("/rewind 2", "Rewind to before")
	fmt.Fprintln(c.stdin, "conversation")
	c.waitFor(func(out string) bool { return strings.Contains(out, "the conversation is back") }, "the rewind")
	if body := g.ask(c, "Which codewords?"); strings.Contains(body, "ZEBRA") || strings.Contains(body, "OSPREY") {
		t.Fatalf("the rewound conversation reached the model:\n%s", body)
	}
	exit(c)
	s := g.sessions()
	if len(s) != 1 {
		t.Fatalf("a rewind started another session: %+v", s)
	}
	evs := verified(t, g.record(), s[0].ID)
	var f agent.Forked
	for _, e := range evs {
		if e.Type == agent.EvForked {
			_ = json.Unmarshal(e.Payload, &f)
			if f.ThroughSeq != 0 || e.Actor != agent.ActorUser {
				t.Fatalf("fork %+v by %s", f, e.Actor)
			}
		}
	}
	if count(evs, agent.EvForked) != 1 {
		t.Fatal("no recorded fork")
	}
	// -c goes on from the rewound conversation.
	c = g.start("-c")
	if body := g.ask(c, "Now?"); strings.Contains(body, "ZEBRA") || !strings.Contains(body, "Which codewords?") {
		t.Fatalf("-c after the rewind:\n%s", body)
	}
	exit(c)
}

// Checkpoints outlive the process: after an exit and -c, /undo puts the
// last turn's file back from the record's blobs.
func TestUndoAfterResume(t *testing.T) {
	g := newSessRig(t)
	c := g.start("-mode", "accept-edits")
	g.ask(c, "write a.txt=one")
	g.ask(c, "write a.txt=two")
	exit(c)

	c = g.start("-c", "-mode", "accept-edits")
	c.waitFor(func(out string) bool { return strings.Contains(out, "resumed") }, "the resume")
	c.command("/undo", "restored")
	if a, _ := g.file("a.txt"); a != "one" {
		t.Fatalf("a.txt = %q after /undo", a)
	}
	c.command("/undo", "removed")
	if _, ok := g.file("a.txt"); ok {
		t.Fatal("undoing the creation left the file")
	}
	exit(c)
	evs := verified(t, g.record(), g.sessions()[0].ID)
	if count(evs, agent.EvFileRestored) != 2 {
		t.Fatalf("%d file.restored", count(evs, agent.EvFileRestored))
	}
}

// A restore is the person's write, put to policy: a path a deny rule keeps
// is not put back, and the refusal is recorded.
func TestRewindRestoreRespectsDeny(t *testing.T) {
	g := newSessRig(t)
	c := g.start("-mode", "accept-edits")
	g.ask(c, "write keep.txt=one")
	g.ask(c, "write keep.txt=two")
	exit(c)

	c = g.start("-c", "-deny", "write("+filepath.Join(g.ws, "keep.txt")+")")
	c.waitFor(func(out string) bool { return strings.Contains(out, "resumed") }, "the resume")
	c.command("/undo", "not restored")
	if a, _ := g.file("keep.txt"); a != "two" {
		t.Fatalf("keep.txt = %q; the deny rule did not hold", a)
	}
	exit(c)
	evs := verified(t, g.record(), g.sessions()[0].ID)
	if count(evs, agent.EvFileRestored) != 0 || count(evs, agent.EvActionDenied) == 0 {
		t.Fatalf("%d restores, %d denials", count(evs, agent.EvFileRestored), count(evs, agent.EvActionDenied))
	}
}

// A file policy keeps from being read, or one named as keys, is not copied
// into the record's blobs before an edit: its checkpoint says why, and it is
// not offered for restore.
func TestNoCheckpointOfSecretOrDeniedFiles(t *testing.T) {
	g := newSessRig(t)
	c := g.start("-mode", "accept-edits", "-deny", "read("+filepath.Join(g.ws, "private.txt")+")")
	g.ask(c, "write .env=TOKEN=FIRST-7731")
	g.ask(c, "write .env=TOKEN=SECOND-7731")
	g.ask(c, "write private.txt=PRIVATE-FIRST-7731")
	g.ask(c, "write private.txt=PRIVATE-SECOND-7731")
	c.command("/undo", "no copy was kept")
	exit(c)
	if got, _ := g.file("private.txt"); got != "PRIVATE-SECOND-7731" {
		t.Fatalf("private.txt = %q", got)
	}
	_ = filepath.Walk(filepath.Join(g.home, ".abhed", "records"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if data, _ := os.ReadFile(p); strings.Contains(string(data), "FIRST-7731") && strings.Contains(p, "blobs") {
				t.Errorf("a secret file's content was kept in %s", p)
			}
		}
		return nil
	})
	evs := verified(t, g.record(), g.sessions()[0].ID)
	skipped := 0
	for _, e := range evs {
		var cp agent.CheckpointSaved
		if e.Type == agent.EvCheckpoint && json.Unmarshal(e.Payload, &cp) == nil && cp.Skipped != "" {
			skipped++
		}
	}
	if skipped != 2 {
		t.Fatalf("%d skipped checkpoints, want 2 (the second edit of each file)", skipped)
	}
}

// The names that hold keys are matched without case, in the file's name or
// any folder above it.
func TestNoCheckpointNames(t *testing.T) {
	for _, p := range []string{"/w/.ENV", "/w/prod.env", "/w/.envrc", "/w/.git-credentials", "/w/.pgpass",
		"/w/app/credentials.json", "/w/main.tfvars", "/h/.azure/token", "/h/.aws/config", "/h/.kube/config",
		"/h/.npmrc", "/h/.pypirc", "/h/.netrc", "/w/c.P12", "/w/c.pfx", "/w/server.key"} {
		if noCheckpoint(nil, p) == "" {
			t.Errorf("%s is checkpointed", p)
		}
	}
	for _, p := range []string{"/w/main.go", "/w/README.md", "/w/environment.go"} {
		if why := noCheckpoint(nil, p); why != "" {
			t.Errorf("%s is skipped: %s", p, why)
		}
	}
}
