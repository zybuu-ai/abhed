package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHome points the OS home at a fresh folder and forgets any person.
func fakeHome(t *testing.T) string {
	t.Helper()
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv(EnvPerson, "")
	if err := SetPerson(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetPerson("") })
	return h
}

func mkRoot(t *testing.T, h, kind, name string) string {
	t.Helper()
	root := filepath.Join(h, DirName, kind, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPersonDefaultsToMachine(t *testing.T) {
	h := fakeHome(t)
	m, err := Machine()
	if err != nil || m != filepath.Join(h, DirName) {
		t.Fatalf("Machine = %q, %v", m, err)
	}
	p, err := Person()
	if err != nil || p != m {
		t.Fatalf("Person = %q, %v; want %q", p, err, m)
	}
	if !Legacy() {
		t.Error("no person chosen, but not legacy")
	}
	if got, _ := PersonPath("records"); got != filepath.Join(m, "records") {
		t.Errorf("PersonPath = %q", got)
	}
}

func TestSetPersonMovesPersonPathsOnly(t *testing.T) {
	h := fakeHome(t)
	root := mkRoot(t, h, AccountsDir, "a1")
	if err := SetPerson(root); err != nil {
		t.Fatal(err)
	}
	if got, _ := PersonPath("records"); got != filepath.Join(root, "records") {
		t.Errorf("PersonPath = %q", got)
	}
	if got, _ := MachinePath("config.json"); got != filepath.Join(h, DirName, "config.json") {
		t.Errorf("MachinePath = %q", got)
	}
	if Legacy() {
		t.Error("a person is chosen, but legacy")
	}
	// A child Abhed reads it from the environment.
	if os.Getenv(EnvPerson) != root {
		t.Errorf("%s = %q", EnvPerson, os.Getenv(EnvPerson))
	}
	mu.Lock()
	person = ""
	mu.Unlock()
	if got, err := Person(); err != nil || got != root {
		t.Errorf("child Person = %q, %v", got, err)
	}
}

func TestCheckRefuses(t *testing.T) {
	h := fakeHome(t)
	m := filepath.Join(h, DirName)
	good := mkRoot(t, h, GuestDir, "g1")
	if err := Check(good); err != nil {
		t.Fatalf("a guest folder: %v", err)
	}
	open := mkRoot(t, h, AccountsDir, "open")
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m, AccountsDir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	nested := mkRoot(t, h, AccountsDir, "a/b")
	cases := map[string]string{
		"machine itself":    m,
		"the accounts dir":  filepath.Join(m, AccountsDir),
		"another kind":      mkRoot(t, h, "records", "x"),
		"nested":            nested,
		"outside":           t.TempDir(),
		"relative":          "accounts/a",
		"unclean":           filepath.Join(m, AccountsDir) + "/../" + AccountsDir + "/a",
		"open to others":    open,
		"a link":            link,
		"missing":           filepath.Join(m, AccountsDir, "nobody"),
		"name with a space": filepath.Join(m, AccountsDir, "a b"),
	}
	for name, root := range cases {
		if err := Check(root); err == nil {
			t.Errorf("%s (%s): accepted", name, root)
		}
		if err := SetPerson(root); err == nil {
			t.Errorf("%s: SetPerson accepted", name)
		}
	}
}

// A person folder handed down that Abhed did not make is an error, never
// the machine's folder, so a child cannot be steered into another's state
// and quietly fall back.
func TestEnvPersonMustCheck(t *testing.T) {
	h := fakeHome(t)
	t.Setenv(EnvPerson, filepath.Join(h, "elsewhere"))
	if p, err := Person(); err == nil {
		t.Fatalf("Person = %q with a bad %s", p, EnvPerson)
	}
	if _, err := PersonPath("records"); err == nil {
		t.Fatal("PersonPath with a bad environment")
	}
}

func TestExpandState(t *testing.T) {
	h := fakeHome(t)
	if got := ExpandState("~/.abhed/skills"); got != filepath.Join(h, DirName, "skills") {
		t.Errorf("legacy: %q", got)
	}
	root := mkRoot(t, h, AccountsDir, "a1")
	if err := SetPerson(root); err != nil {
		t.Fatal(err)
	}
	if got := ExpandState("~/.abhed/skills"); got != filepath.Join(root, "skills") {
		t.Errorf("person: %q", got)
	}
	if got := ExpandState("~/team/skills"); got != filepath.Join(h, "team", "skills") {
		t.Errorf("home: %q", got)
	}
	if got := ExpandState("/abs/x"); got != "/abs/x" {
		t.Errorf("absolute: %q", got)
	}
	if got := ExpandState("~/.abhedx/y"); strings.HasPrefix(got, root) {
		t.Errorf("a look-alike prefix went to the person: %q", got)
	}
}
