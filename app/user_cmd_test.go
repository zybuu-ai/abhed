package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
)

// userWorkspace is a workspace with local accounts and a scratch home, so no
// test reads or writes the real ~/.abhed.
func userWorkspace(t *testing.T, authJSON string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_USERS_FILE", "")
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"auth":` + authJSON + `}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

// runUser runs `abhed user` and returns its exit code and what it wrote to stderr.
func runUser(t *testing.T, ws string, args ...string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldErr, oldOut := os.Stderr, os.Stdout
	os.Stderr, os.Stdout = w, w
	code := userCmd(ws, args)
	os.Stderr, os.Stdout = oldErr, oldOut
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return code, string(out)
}

// A password set with `user add` is one the person must change at first
// sign-in, as it is when an administrator sets one from the page.
func TestUserAddSetsMustChange(t *testing.T) {
	ws := userWorkspace(t, `{"mode":"local"}`)
	if code, out := runUser(t, ws, "add", "dave", "-password", "correct-horse-1"); code != 0 {
		t.Fatalf("user add = %d: %s", code, out)
	}
	st, err := auth.NewFileUserStore(filepath.Join(ws, ".abhed", "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.Get(context.Background(), "dave")
	if err != nil {
		t.Fatal(err)
	}
	if !u.MustChange {
		t.Fatal("user add left the password permanent")
	}
}

// Removing an account that does not exist fails and says so.
func TestUserRemoveUnknownFails(t *testing.T) {
	ws := userWorkspace(t, `{"mode":"local"}`)
	code, out := runUser(t, ws, "remove", "nosuch")
	if code != 1 || !strings.Contains(out, "no such user") || strings.Contains(out, "removed") {
		t.Fatalf("user remove nosuch = %d %q, want 1 and no such user", code, out)
	}
	if code, out := runUser(t, ws, "add", "dave", "-password", "correct-horse-1"); code != 0 {
		t.Fatalf("user add = %d: %s", code, out)
	}
	if code, out := runUser(t, ws, "remove", "dave"); code != 0 || !strings.Contains(out, "removed dave") {
		t.Fatalf("user remove dave = %d %q", code, out)
	}
}

// The commands that write accounts refuse a users file serve would refuse,
// with serve's message, and write nothing.
func TestUserCommandsRefuseAStateFileServeRefuses(t *testing.T) {
	ws := userWorkspace(t, `{"mode":"local"}`)
	file := filepath.Join(ws, "users.json")
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"),
		[]byte(`{"auth":{"mode":"local","users_file":"`+filepath.ToSlash(file)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "yuki", "-password", "correct-horse-1"},
		{"passwd", "yuki"},
		{"import"},
	} {
		code, out := runUser(t, ws, args...)
		if code != 1 || !strings.Contains(out, "refusing to start: the state file") {
			t.Errorf("user %s = %d %q, want 1 with the state-file refusal", args[0], code, out)
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("a refused users file was written: %v", err)
	}
}
