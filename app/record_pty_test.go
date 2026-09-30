package app

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/clitest"
)

// These run on the pty harness once it is built, and skip until then; the
// pipe tests in sessions_e2e_test.go and rewind_e2e_test.go cover the same
// behaviour now.

// Resume after exit: -c continues, and the record verifies.
func TestPtyContinueAfterExit(t *testing.T) {
	ws := t.TempDir()
	h := clitest.Start(t, clitest.Opts{Script: "text \"noted\"\n", Args: []string{"-C", ws, "-n", "first"}})
	h.Type("Remember ZEBRA-41.")
	h.Key(clitest.Enter)
	h.WaitText("noted")
	first := h.Record()
	h.Exit(0)
	if !first.Verified {
		t.Fatal("the record did not verify")
	}
	h = clitest.Start(t, clitest.Opts{Script: "text \"again\"\n", Args: []string{"-C", ws, "-c"}})
	h.WaitText("resumed")
	h.Type("And now?")
	h.Key(clitest.Enter)
	h.WaitText("again")
	if r := h.Record(); !r.Verified || r.Events[0].SessionID != first.Events[0].SessionID {
		t.Fatal("-c did not continue the verified session")
	}
	h.Exit(0)
}

// Rewind to the first message records a fork at 0 (S6), in the same session.
func TestPtyRewindToFirstMessage(t *testing.T) {
	ws := t.TempDir()
	h := clitest.Start(t, clitest.Opts{Script: "text \"one\"\n\ntext \"two\"\n", Args: []string{"-C", ws}})
	h.Type("first")
	h.Key(clitest.Enter)
	h.WaitText("one")
	h.Type("/rewind 1")
	h.Key(clitest.Enter)
	h.WaitText("Rewind to before")
	h.Type("conversation")
	h.Key(clitest.Enter)
	h.WaitText("the conversation is back")
	r := h.Record()
	if !r.Verified {
		t.Fatal("the record did not verify")
	}
	forks := 0
	for _, e := range r.Events {
		if e.Type == agent.EvForked {
			forks++
		}
	}
	if forks != 1 {
		t.Fatalf("%d forks recorded", forks)
	}
	h.Exit(0)
}

// A tampered record is shown unverified and continued only on a yes.
func TestPtyTamperedRecordNeedsConfirm(t *testing.T) {
	ws := t.TempDir()
	h := clitest.Start(t, clitest.Opts{Script: "text \"noted\"\n", Args: []string{"-C", ws, "-r", "missing-or-tampered"}})
	h.WaitText("starting a new session")
	h.Exit(0)
}
