package identity

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/home"
)

// A guest folder is made, becomes the person's folder, and leaves nothing
// once ended.
func TestGuestEndLeavesNothing(t *testing.T) {
	testStore(t)
	g, err := EnterGuest()
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := home.Person(); p != g.Root {
		t.Fatalf("person = %q, want the guest folder %q", p, g.Root)
	}
	if err := home.Check(g.Root); err != nil {
		t.Fatalf("the guest folder is not one home accepts: %v", err)
	}
	rec, _ := home.PersonPath("records", "default", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A running guest is not swept.
	if removed, _ := SweepGuests(); len(removed) != 0 {
		t.Fatalf("a live guest was swept: %v", removed)
	}
	if err := g.End(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.Root); !os.IsNotExist(err) {
		t.Fatalf("the guest folder is still there: %v", err)
	}
	if !home.Legacy() {
		t.Error("the person's folder still names the ended guest")
	}
	assertNoGuests(t)
}

func assertNoGuests(t *testing.T) {
	t.Helper()
	dir, _ := home.MachinePath(home.GuestDir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("left in %s: %v", dir, names)
	}
}

// TestGuestHelper is the child the crash test kills: it enters guest,
// writes into the folder, says where, and waits.
func TestGuestHelper(t *testing.T) {
	if os.Getenv("ABHED_GUEST_HELPER") != "1" {
		t.Skip("run by TestGuestCrashIsSweptOnNextStart")
	}
	g, err := EnterGuest()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(g.Root, "history.jsonl"), []byte("secret prompt\n"), 0o600)
	os.Stdout.WriteString(g.Root + "\n")
	time.Sleep(time.Minute)
}

// kill -9 leaves the folder; the next start's sweep removes it.
func TestGuestCrashIsSweptOnNextStart(t *testing.T) {
	testStore(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuestHelper$")
	cmd.Env = append(os.Environ(), "ABHED_GUEST_HELPER=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	root, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	root = strings.TrimSpace(root)
	if removed, _ := SweepGuests(); len(removed) != 0 {
		t.Fatalf("a live guest was swept: %v", removed)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("kill -9 should leave the folder for the sweep: %v", err)
	}
	removed, err := SweepGuests()
	if err != nil || len(removed) != 1 || removed[0] != root {
		t.Fatalf("sweep removed %v, %v; want %s", removed, err, root)
	}
	assertNoGuests(t)
}

// A folder being made is left alone while young; a stray link is removed,
// never followed.
func TestSweepLeavesYoungNewAndRemovesLinks(t *testing.T) {
	testStore(t)
	dir, _ := home.MachinePath(home.GuestDir)
	young := filepath.Join(dir, newPrefix+"abc")
	if err := os.MkdirAll(young, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	keep := filepath.Join(victim, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepGuests(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Error("a young folder being made was swept")
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); !os.IsNotExist(err) {
		t.Error("a stray link stayed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("the sweep followed a link")
	}
	old := time.Now().Add(-2 * youngNew)
	if err := os.Chtimes(young, old, old); err != nil {
		t.Fatal(err)
	}
	if removed, _ := SweepGuests(); len(removed) != 1 {
		t.Errorf("an old unfinished folder was not swept: %v", removed)
	}
}
