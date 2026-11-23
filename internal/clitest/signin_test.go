//go:build unix

package clitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// leftOf lists what a guest left in ~/.abhed/guest and any session record
// written anywhere under ~/.abhed.
func leftOf(t *testing.T, osHome string) (guests, records []string) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(osHome, ".abhed", "guest"))
	for _, e := range entries {
		guests = append(guests, e.Name())
	}
	_ = filepath.WalkDir(filepath.Join(osHome, ".abhed"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") && filepath.Base(p) != "index.jsonl" &&
			strings.Contains(p, string(filepath.Separator)+"records"+string(filepath.Separator)) {
			records = append(records, p)
		}
		return nil
	})
	return guests, records
}

// guestTurn starts an interactive guest, has one exchange and returns the run.
func guestTurn(t *testing.T) *Run {
	t.Helper()
	h := StartRun(t, Opts{NoAccount: true, Args: []string{"-guest"}, Script: `text "guest reply"`})
	h.WaitText("Guest: nothing is kept")
	h.Type("remember PELICAN-7")
	h.Key(Enter)
	h.WaitText("guest reply")
	guests, records := leftOf(t, h.Home())
	if len(guests) != 1 || len(records) == 0 {
		t.Fatalf("while running, a guest folder holding the record: guests %v records %v", guests, records)
	}
	return h
}

func TestGuestLeavesNothingAfterANormalExit(t *testing.T) {
	t.Parallel()
	h := guestTurn(t)
	h.Exit(0)
	if guests, records := leftOf(t, h.Home()); len(guests) != 0 || len(records) != 0 {
		t.Fatalf("left after exit: guests %v records %v", guests, records)
	}
}

func TestGuestLeavesNothingAfterSIGTERM(t *testing.T) {
	t.Parallel()
	h := guestTurn(t)
	if err := h.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The interactive loop ends as it does on an interrupt; what matters
	// here is what is left.
	h.Wait(20 * time.Second)
	if guests, records := leftOf(t, h.Home()); len(guests) != 0 || len(records) != 0 {
		t.Fatalf("left after SIGTERM: guests %v records %v", guests, records)
	}
}

// kill -9 leaves the folder; the next start of abhed, of any kind, removes it.
func TestGuestKilledIsRemovedAtTheNextStart(t *testing.T) {
	t.Parallel()
	h := guestTurn(t)
	if err := h.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.Wait(20 * time.Second)
	if guests, _ := leftOf(t, h.Home()); len(guests) != 1 {
		t.Fatalf("kill -9 should leave the folder for the next start: %v", guests)
	}
	next := exec.Command(Binary(t), "-guest", "-p", "hi")
	next.Dir = h.Workspace()
	next.Env = h.env()
	if out, err := next.CombinedOutput(); err != nil {
		t.Fatalf("the next start: %v\n%s", err, out)
	}
	if guests, records := leftOf(t, h.Home()); len(guests) != 0 || len(records) != 0 {
		t.Fatalf("left after the next start: guests %v records %v", guests, records)
	}
}

// A headless run nobody signed in to runs as guest, says so on stderr, and
// keeps nothing; one that names -guest says nothing.
func TestHeadlessWithNoSignInIsGuest(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{NoAccount: true, Args: []string{"-p", "hi"}, Script: `text "done"`})
	if !strings.Contains(h.Stderr(), "running as guest, nothing is kept") || !strings.Contains(h.Stdout(), "done") {
		t.Fatalf("stdout %q stderr %q", h.Stdout(), h.Stderr())
	}
	if guests, records := leftOf(t, h.Home()); len(guests) != 0 || len(records) != 0 {
		t.Fatalf("left: guests %v records %v", guests, records)
	}
	h = piped(t, Opts{NoAccount: true, Args: []string{"-guest", "-p", "hi"}, Script: `text "done"`})
	if strings.Contains(h.Stderr(), "guest") {
		t.Errorf("an explicit -guest still noted: %q", h.Stderr())
	}
}

// A machine whose organisation requires sign-in never runs a headless task
// as guest: it exits 10 and says what to run.
func TestHeadlessSignInRequiredExits10(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{NoAccount: true, Managed: `{"auth":{"client":{"sign_in":"required"}}}`,
		Args: []string{"-p", "hi"}, Script: `text "done"`})
	if code := h.Wait(time.Second); code != 10 {
		t.Fatalf("exit %d, want 10; stderr %q", code, h.Stderr())
	}
	if !strings.Contains(h.Stderr(), "sign-in required") || len(h.Requests()) != 0 {
		t.Fatalf("stderr %q, %d model requests", h.Stderr(), len(h.Requests()))
	}
	h = piped(t, Opts{NoAccount: true, Managed: `{"auth":{"client":{"sign_in":"required"}}}`,
		Args: []string{"-p", "-output-format", "json", "hi"}, Script: `text "done"`})
	if code := h.Wait(time.Second); code != 10 || !strings.Contains(h.Stdout(), `"error":"signin_required"`) {
		t.Fatalf("json result line: exit %d stdout %q", code, h.Stdout())
	}
	h = piped(t, Opts{NoAccount: true, Managed: `{"auth":{"client":{"guest":false}}}`,
		Args: []string{"-guest", "-p", "hi"}, Script: `text "done"`})
	if code := h.Wait(time.Second); code != 10 {
		t.Fatalf("-guest with guest off: exit %d", code)
	}
}

// A signed-in machine runs headless as the account, and keeps the record.
func TestHeadlessUsesTheRememberedSignIn(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "hi"}, Script: `text "done"`})
	if strings.Contains(h.Stderr(), "guest") {
		t.Fatalf("a signed-in run noted guest: %q", h.Stderr())
	}
	r := h.Record()
	if len(r.Events) == 0 {
		t.Fatal("the account's run kept no record")
	}
	// session.started says whose session it was.
	if r.Events[0].Type != "session.started" || !strings.Contains(string(r.Events[0].Payload), `"principal":{"kind":"account","owner":"device:tester"}`) {
		t.Fatalf("session.started: %s", r.Events[0].Payload)
	}
}

// The first start of a machine with no account asks who is working before
// anything else; creating an account signs in and the session starts.
func TestFirstStartAsksWhoIsWorking(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{NoAccount: true, Script: `text "hello there"`})
	h.WaitText("who is working?")
	h.WaitText("Guest")
	h.Type("1\r") // create an account
	h.WaitText("Username")
	h.Type("pat\r")
	h.WaitText("Password")
	h.Type("pat password 1\r")
	h.WaitText("Type it again")
	h.Type("pat password 1\r")
	h.WaitText("Recovery code")
	h.WaitText("Signed in as pat")
	h.Type("hi\r")
	h.WaitText("hello there")
	h.Exit(0)
}

// abhed init before anyone signed in does not make the machine look like one
// an earlier version used: a later headless run is still a guest's, says
// so, and keeps nothing, at the machine's root or anywhere else.
func TestInitBeforeSignInLeavesLaterRunsGuest(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{NoAccount: true, Args: []string{"init"}, Script: `text "done"`})
	if code := h.Wait(time.Second); code != 0 {
		t.Fatalf("init: exit %d\n%s", code, h.Output())
	}
	if o := string(h.Output()); !strings.Contains(o, "for this run only; nothing is kept") || strings.Contains(o, "trusts it as written") {
		t.Fatalf("a guest's init said its trust is kept:\n%s", h.Output())
	}
	next := exec.Command(Binary(t), "-p", "hi")
	next.Dir = h.Workspace()
	next.Env = h.env()
	out, _ := next.CombinedOutput()
	if strings.Contains(string(out), "running as before") || !strings.Contains(string(out), "running as guest, nothing is kept") {
		t.Fatalf("the run after init:\n%s", out)
	}
	if guests, records := leftOf(t, h.Home()); len(guests) != 0 || len(records) != 0 {
		t.Fatalf("left: guests %v records %v", guests, records)
	}
	for _, p := range []string{"trust.json", "records", "history"} {
		if _, err := os.Lstat(filepath.Join(h.Home(), ".abhed", p)); err == nil {
			t.Errorf("~/.abhed/%s was written before anyone signed in", p)
		}
	}
}
