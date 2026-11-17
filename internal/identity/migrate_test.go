package identity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// legacyHome writes what a 1.2.9 home holds: a chained record with sessions
// in two tenants, history, memory, a secret store and sign-in profiles.
func legacyHome(t *testing.T) (machine string, files map[string]string) {
	t.Helper()
	machine, _ = home.Machine()
	records := filepath.Join(machine, "records")
	for _, tenant := range []string{"default", "acme"} {
		st, err := local.Open(local.Options{Dir: records, Tenant: tenant, User: "yuvraj"})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"s-1", "s-2"} {
			if err := st.CreateSession(context.Background(), store.SessionRecord{ID: id, User: "yuvraj", Workspace: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			rec := agent.NewRecorder(st, id, "")
			for _, text := range []string{"one", "two", "three"} {
				if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	files = map[string]string{
		"history/abc.jsonl":             `{"line":"hello"}`,
		"ABHED.md":                      "# my notes\n",
		"secrets.json":                  `{"version":1}`,
		"profiles.json":                 `{"version":1,"profiles":{}}`,
		"projects/p-1/memory/MEMORY.md": "- remember this\n",
		"state/workspace-grants.json":   `{}`,
	}
	for rel, body := range files {
		p := filepath.Join(machine, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The machine's own stay put.
	if err := os.WriteFile(filepath.Join(machine, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return machine, files
}

func TestMigrateMovesTheDataWithMatchingHeads(t *testing.T) {
	s := testStore(t)
	machine, files := legacyHome(t)
	before, err := recordHeads(filepath.Join(machine, "records"))
	if err != nil || len(before) != 6 {
		t.Fatalf("heads before: %v %v", before, err)
	}
	e, _, err := s.Create("yuvraj", pw, "", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Migrate(s, e, "yuvraj")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 6 || res.Moved < len(files) {
		t.Fatalf("result %+v", res)
	}
	root := filepath.Join(s.Dir(), e.ID)
	after, err := recordHeads(filepath.Join(root, "records"))
	if err != nil || !sameHeads(before, after) {
		t.Fatalf("heads after the move differ: %v vs %v (%v)", before, after, err)
	}
	for rel, body := range files {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || string(got) != body {
			t.Errorf("%s in the account: %q %v", rel, got, err)
		}
		if _, err := os.Lstat(filepath.Join(machine, rel)); !os.IsNotExist(err) {
			t.Errorf("%s is still in ~/.abhed", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(machine, "config.json")); err != nil {
		t.Error("the machine's config moved")
	}
	if _, err := os.Stat(filepath.Join(machine, MovedNote)); err != nil {
		t.Error("no note says where the data went")
	}
	got, _ := s.Get("yuvraj")
	if got.LegacyUser != "yuvraj" {
		t.Errorf("legacy user %q", got.LegacyUser)
	}
	if LegacyData() {
		t.Error("data from before sign-in still found after the move")
	}
	// The record still opens for the account as the person's folder.
	if err := home.SetPerson(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.SetPerson("") })
	dir, _ := local.DefaultDir()
	st, err := local.Open(local.Options{Dir: dir, Tenant: "default", User: "device:yuvraj"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rep, err := st.Verify("s-1"); err != nil || !rep.OK {
		t.Fatalf("the moved session does not verify: %+v %v", rep, err)
	}
}

// A crash after some items moved resumes at the next start, and the heads
// still match.
func TestMigrateResumesAfterACrash(t *testing.T) {
	s := testStore(t)
	machine, files := legacyHome(t)
	before, _ := recordHeads(filepath.Join(machine, "records"))
	e, _, _ := s.Create("yuvraj", pw, "", "")
	root, _ := s.Root(e)
	// As a crash would leave it: the journal written, records and history
	// moved, history not yet noted.
	j := journal{Account: e.ID, LegacyUser: "yuvraj", State: stateMoving, Heads: before}
	for _, it := range LegacyItems {
		if _, err := os.Lstat(filepath.Join(machine, it)); err == nil {
			j.Items = append(j.Items, it)
		}
	}
	for _, it := range []string{"records", "history"} {
		if err := os.Rename(filepath.Join(machine, it), filepath.Join(root, it)); err != nil {
			t.Fatal(err)
		}
	}
	j.Done = []string{"records"}
	if err := writeJSON(s.journalPath(e), j); err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeMigration(s)
	if err != nil || !resumed {
		t.Fatalf("resume: %v %v", resumed, err)
	}
	after, _ := recordHeads(filepath.Join(root, "records"))
	if !sameHeads(before, after) {
		t.Fatal("heads differ after the resumed move")
	}
	for rel := range files {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s not moved: %v", rel, err)
		}
	}
	if again, err := ResumeMigration(s); again || err != nil {
		t.Errorf("a finished move resumed again: %v %v", again, err)
	}
}

// A record changed during the move (heads that differ) moves everything back.
func TestMigrateMovesBackWhenHeadsDiffer(t *testing.T) {
	s := testStore(t)
	machine, files := legacyHome(t)
	e, _, _ := s.Create("yuvraj", pw, "", "")
	j := journal{Account: e.ID, LegacyUser: "yuvraj", State: stateMoving,
		Heads: map[string]string{"default/s-1": "true 9 not-the-head"}}
	for _, it := range LegacyItems {
		if _, err := os.Lstat(filepath.Join(machine, it)); err == nil {
			j.Items = append(j.Items, it)
		}
	}
	if err := writeJSON(s.journalPath(e), j); err != nil {
		t.Fatal(err)
	}
	_, err := Migrate(s, e, "yuvraj")
	if err == nil || !strings.Contains(err.Error(), "moved back") {
		t.Fatalf("err = %v", err)
	}
	for rel := range files {
		if _, err := os.Stat(filepath.Join(machine, rel)); err != nil {
			t.Errorf("%s not back in ~/.abhed: %v", rel, err)
		}
	}
	if _, taken, _ := s.LegacyOwner(); taken {
		t.Error("the account took the data over though the move failed")
	}
}

func TestUnmigrateRoundTrips(t *testing.T) {
	s := testStore(t)
	machine, files := legacyHome(t)
	before, _ := recordHeads(filepath.Join(machine, "records"))
	e, _, _ := s.Create("yuvraj", pw, "", "")
	if _, err := Migrate(s, e, "yuvraj"); err != nil {
		t.Fatal(err)
	}
	n, err := Unmigrate(s)
	if err != nil || n == 0 {
		t.Fatalf("unmigrate: %d %v", n, err)
	}
	after, _ := recordHeads(filepath.Join(machine, "records"))
	if !sameHeads(before, after) {
		t.Fatal("heads differ after moving back")
	}
	for rel, body := range files {
		if got, _ := os.ReadFile(filepath.Join(machine, rel)); string(got) != body {
			t.Errorf("%s not back: %q", rel, got)
		}
	}
	if _, taken, _ := s.LegacyOwner(); taken {
		t.Error("the account still holds the legacy data")
	}
	if _, err := os.Stat(filepath.Join(machine, MovedNote)); !os.IsNotExist(err) {
		t.Error("the note stayed")
	}
}

// A session another Abhed is writing stops the move before anything moves.
func TestMigrateRefusesWhileASessionIsHeld(t *testing.T) {
	s := testStore(t)
	machine, _ := legacyHome(t)
	st, err := local.Open(local.Options{Dir: filepath.Join(machine, "records"), Tenant: "default", User: "yuvraj"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Acquire("s-1"); err != nil {
		t.Fatal(err)
	}
	// The store holds the session's flock on its own open of the lock
	// file, as another process would.
	e, _, _ := s.Create("yuvraj", pw, "", "")
	if _, err := Migrate(s, e, "yuvraj"); err == nil || !strings.Contains(err.Error(), "writing session") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(machine, "records")); err != nil {
		t.Error("the record moved while a session was held")
	}
}
