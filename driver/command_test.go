package driver_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

func TestCommandCapability(t *testing.T) {
	if err := driver.Check(minimal{caps: []driver.Capability{driver.CapCommand}}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("command declared without a Commander: %v", err)
	}
	ctx := context.Background()
	undeclared := drivertest.New("fake")
	if _, err := driver.Command(ctx, undeclared, "a", driver.CommandRequest{Command: "true"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("an undeclared command: %v", err)
	}
	f := drivertest.New("fake", driver.CapCommand)
	if err := driver.Check(f); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if _, err := f.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []driver.CommandRequest{{}, {Command: "true", Shell: true}} {
		if _, err := driver.Command(ctx, f, "a", req); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%+v: %v", req, err)
		}
	}
	cmd, err := driver.Command(ctx, f, "a", driver.CommandRequest{Command: "echo hi > made.txt; echo $ABHED_SANDBOX"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "a" {
		t.Fatalf("%q %v", out, err)
	}
	if b, err := os.ReadFile(ws + "/made.txt"); err != nil || string(b) != "hi\n" {
		t.Fatalf("made %q %v", b, err)
	}
	if f.Calls("command") != 1 {
		t.Fatalf("calls %d", f.Calls("command"))
	}
}
