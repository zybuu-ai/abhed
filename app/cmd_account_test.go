package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/modelkeys"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// accountHome is a fresh home with fast hashes.
func accountHome(t *testing.T) string {
	t.Helper()
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv(home.EnvPerson, "")
	return h
}

// runAccount runs `abhed account args...` with input as its stdin lines.
func runAccount(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out, errOut bytes.Buffer
	code := accountCmd(args, f, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestAccountCreateSigninSignout(t *testing.T) {
	accountHome(t)
	code, out, errOut := runAccount(t, "correct horse\ncorrect horse\n", "create", "-email", "y@example.com", "yuvraj")
	if code != 0 || !strings.Contains(out, "Recovery code") {
		t.Fatalf("create: %d %q %q", code, out, errOut)
	}
	if code, out, _ := runAccount(t, "", "whoami"); code != 0 || !strings.Contains(out, "yuvraj") {
		t.Fatalf("whoami after create: %d %q", code, out)
	}
	if code, _, _ := runAccount(t, "", "signout"); code != 0 {
		t.Fatal("signout")
	}
	if code, _, _ := runAccount(t, "", "whoami"); code != 1 {
		t.Fatal("whoami after signout says someone is signed in")
	}
	if code, _, errOut := runAccount(t, "wrong pass 1\nwrong pass 2\nwrong pass 3\n", "signin", "yuvraj"); code == 0 {
		t.Fatalf("wrong passwords signed in: %q", errOut)
	}
	// Three wrong tries in a row make the next one wait, right or not.
	if code, _, errOut := runAccount(t, "correct horse\n", "signin", "yuvraj"); code != 1 || !strings.Contains(errOut, "too many wrong passwords") {
		t.Fatalf("after three wrong tries: %d %q", code, errOut)
	}
}

func TestAccountResetAsOSUserIsReported(t *testing.T) {
	accountHome(t)
	if code, _, e := runAccount(t, "correct horse\ncorrect horse\n", "create", "a1"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runAccount(t, "a1\nnew password 1\nnew password 1\n", "reset", "-as-os-user", "a1"); code != 0 {
		t.Fatalf("reset: %s", e)
	}
	code, _, errOut := runAccount(t, "new password 1\n", "signin", "a1")
	if code != 0 || !strings.Contains(errOut, "without the recovery code") {
		t.Fatalf("signin after an OS reset: %d %q", code, errOut)
	}
}

func TestAccountRemoveKeepsDataUnlessAsked(t *testing.T) {
	accountHome(t)
	if code, _, e := runAccount(t, "correct horse\ncorrect horse\n", "create", "a1"); code != 0 {
		t.Fatal(e)
	}
	s, _ := identity.Open()
	e, _ := s.Get("a1")
	dir := filepath.Join(s.Dir(), e.ID)
	if code, _, _ := runAccount(t, "nope\n", "rm", "a1"); code == 0 {
		t.Fatal("removed without confirmation")
	}
	if code, _, _ := runAccount(t, "a1\n", "rm", "a1"); code != 0 {
		t.Fatal("rm")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the data went with the account: %v", err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("a removed account is still signed in")
	}
}

func TestAccountIsRefusedToTheAgent(t *testing.T) {
	for _, verb := range []string{"create", "signin", "signout", "reset", "rm", "list", "whoami"} {
		if selfAdmin([]string{"account", verb}) == "" {
			t.Errorf("abhed account %s is not refused to the agent", verb)
		}
	}
}

// adopt moves a 1.2.9 home's data into the account; unmigrate moves it back.
func TestAccountAdoptAndUnmigrate(t *testing.T) {
	h := accountHome(t)
	legacy := filepath.Join(h, ".abhed", "history", "x.jsonl")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A model key in the Keychain from before sign-in goes with the rest, and back.
	chain := memKeychain{"ANTHROPIC_API_KEY": "sk-legacy-0123456789"}
	defer modelkeys.Use(chain, "")()
	if code, _, e := runAccount(t, "correct horse\ncorrect horse\n", "create", "a1"); code != 0 {
		t.Fatal(e)
	}
	code, out, errOut := runAccount(t, "", "adopt")
	if code != 0 || !strings.Contains(out, "Moved") {
		t.Fatalf("adopt: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("the history stayed in ~/.abhed")
	}
	if _, left := chain["ANTHROPIC_API_KEY"]; left || len(chain) != 1 {
		t.Fatalf("the legacy key was not taken over: %v", chain)
	}
	if code, out, errOut := runAccount(t, "", "unmigrate"); code != 0 || !strings.Contains(out, "back") {
		t.Fatalf("unmigrate: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("the history did not come back")
	}
	if chain["ANTHROPIC_API_KEY"] != "sk-legacy-0123456789" || len(chain) != 1 {
		t.Fatalf("the key did not come back: %v", chain)
	}
}

// The first account is remembered only once the earlier data has moved: a
// move that fails leaves it unremembered, so no other start runs as it on
// half the data.
func TestCreateAndAdoptRemembersOnlyAfterTheMove(t *testing.T) {
	h := accountHome(t)
	records := filepath.Join(h, ".abhed", "records")
	st, err := local.Open(local.Options{Dir: records, Tenant: "default", User: "yuvraj"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateSession(context.Background(), store.SessionRecord{ID: "s-1", User: "yuvraj", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Acquire("s-1"); err != nil { // another abhed writing it
		t.Fatal(err)
	}
	p, _ := answers(t, "yuvraj\ncorrect horse\ncorrect horse\n")
	s, _ := identity.Open()
	if _, err := createAndAdopt(s, p); err == nil {
		t.Fatal("the move went ahead while a session was being written")
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Fatal("the account is remembered though its data did not move")
	}
}

// The keys from before sign-in go only to the account that owns that data.
func TestAdoptKeysOnlyForTheLegacyOwner(t *testing.T) {
	accountHome(t)
	legacy := filepath.Join(os.Getenv("HOME"), ".abhed", "history", "x.jsonl")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chain := memKeychain{}
	defer modelkeys.Use(chain, "")()
	for _, who := range []string{"a1", "b1"} {
		if code, _, e := runAccount(t, "correct horse\ncorrect horse\n", "create", who); code != 0 {
			t.Fatal(e)
		}
	}
	if code, _, e := runAccount(t, "", "adopt", "a1"); code != 0 {
		t.Fatal(e)
	}
	// A key stored before sign-in again (an older build) is not b1's to take.
	chain["ANTHROPIC_API_KEY"] = "sk-legacy-0123456789"
	if code, _, e := runAccount(t, "", "adopt", "b1"); code != 0 {
		t.Fatal(e)
	}
	if chain["ANTHROPIC_API_KEY"] != "sk-legacy-0123456789" || len(chain) != 1 {
		t.Fatalf("b1 took the earlier key: %v", chain)
	}
}
