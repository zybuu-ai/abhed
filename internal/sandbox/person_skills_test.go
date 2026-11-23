package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/home"
)

// personHome makes two accounts with skills under a fresh home, chooses the
// first, and returns both skills folders.
func personHome(t *testing.T) (mine, theirs string) {
	t.Helper()
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv(home.EnvPerson, "")
	acc := filepath.Join(h, home.DirName, home.AccountsDir)
	for _, n := range []string{"a1", "b2"} {
		if err := os.MkdirAll(filepath.Join(acc, n, "skills"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(acc, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := home.SetPerson(filepath.Join(acc, "a1")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.SetPerson("") })
	return filepath.Join(acc, "a1", "skills"), filepath.Join(acc, "b2", "skills")
}

// A command may read the person's own skills and nothing else in ~/.abhed:
// another account's skills stay inside the denied state.
func TestProcessProfileGrantsOnlyThePersonsSkills(t *testing.T) {
	mine, theirs := personHome(t)
	prof := NewProcess(DefaultPolicy(workspace(t))).seatbeltProfile()
	h, _ := os.UserHomeDir()
	if !strings.Contains(prof, `(deny file-read* (subpath "`+filepath.Join(h, home.DirName)+`"))`) {
		t.Error("the profile does not deny ~/.abhed")
	}
	if !strings.Contains(prof, `(allow file-read* (subpath "`+mine+`"))`) {
		t.Errorf("the person's skills %s are not readable", mine)
	}
	if strings.Contains(prof, theirs) {
		t.Errorf("another account's skills %s appear in the profile", theirs)
	}
}

func TestFenceSpecGrantsOnlyThePersonsSkills(t *testing.T) {
	p := fencePolicy(t)
	mine, theirs := personHome(t)
	s := NewFence(p).spec()
	if !slices.Contains(s.Exec, mine) {
		t.Errorf("exec %v lacks the person's skills %s", s.Exec, mine)
	}
	for _, list := range [][]string{s.Exec, s.Within, s.Write} {
		if slices.Contains(list, theirs) {
			t.Errorf("another account's skills %s granted: %v", theirs, list)
		}
	}
	h, _ := os.UserHomeDir()
	if !slices.Contains(s.Deny, filepath.Join(h, home.DirName)) {
		t.Errorf("deny lacks ~/.abhed: %v", s.Deny)
	}
}

// A command on the process tier and on the fence reads neither the accounts
// store, another account's folder, nor a guest's: all of it is inside the
// denied ~/.abhed. Run for real where the tier is available.
func TestCommandsCannotReachAccountsOrGuests(t *testing.T) {
	mine, theirs := personHome(t)
	h, _ := os.UserHomeDir()
	m := filepath.Join(h, home.DirName)
	secrets := map[string]string{
		filepath.Join(m, home.AccountsDir, "accounts.json"):                    "HASH-CANARY",
		filepath.Join(filepath.Dir(theirs), "history", "h.jsonl"):              "THEIR-CANARY",
		filepath.Join(filepath.Dir(mine), "records", "default", "s.jsonl"):     "MINE-CANARY",
		filepath.Join(m, home.GuestDir, "g1", "records", "default", "s.jsonl"): "GUEST-CANARY",
	}
	for p, body := range secrets {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tiers := map[string]Sandbox{"process": NewProcess(DefaultPolicy(workspace(t)))}
	fp := fencePolicy(t)
	t.Setenv("HOME", h) // fencePolicy moved HOME; the canaries are under this one
	tiers["fence"] = NewFence(fp)
	ran := 0
	for name, sb := range tiers {
		if ok, why := sb.Available(); !ok {
			t.Logf("%s: not available here (%s)", name, why)
			continue
		}
		ran++
		for p, body := range secrets {
			out, err := sb.Command(context.Background(), workspace(t), "cat "+shellQuote(p)).CombinedOutput()
			if err == nil || strings.Contains(string(out), body) {
				t.Errorf("%s: a command read %s: %v %q", name, p, err, out)
			}
		}
	}
	if ran == 0 {
		t.Skip("no confining tier available on this machine")
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
