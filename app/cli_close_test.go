package app

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

type closeNoting struct {
	log *[]string
	err error
}

func (c closeNoting) Close() error {
	*c.log = append(*c.log, "sandbox")
	return c.err
}

// The sandbox closes before the record, so what the fence finds planted at
// Close is recorded, and its error is printed.
func TestCloseSandboxThenStore(t *testing.T) {
	var log []string
	var out bytes.Buffer
	err := closeSandboxThenStore(closeNoting{log: &log, err: errors.New("fence: a .abhed appeared in the workspace")},
		func() { log = append(log, "record") }, &out)
	if !slices.Equal(log, []string{"sandbox", "record"}) {
		t.Fatalf("closed in the order %v", log)
	}
	if err == nil {
		t.Fatal("the close error was not returned")
	}
	if out.String() != "abhed: fence: a .abhed appeared in the workspace\n" {
		t.Fatalf("printed %q", out.String())
	}
}

// A headless run that otherwise completed exits 1 when the close reports
// planted state or an unlistable workspace; a run that already failed keeps
// its own code, and the session is closed either way.
func TestHeadlessExitFailsOnCloseError(t *testing.T) {
	planted := errors.New("fence: a .abhed appeared in the workspace")
	for _, tc := range []struct {
		code int
		err  error
		want int
	}{
		{0, nil, 0},
		{0, planted, 1},
		{3, planted, 3},
		{2, nil, 2},
	} {
		closed := 0
		got := headlessExit(tc.code, func() error { closed++; return tc.err })
		if got != tc.want || closed != 1 {
			t.Errorf("code %d, close %v: exit %d (closed %d times), want %d", tc.code, tc.err, got, closed, tc.want)
		}
	}
}
