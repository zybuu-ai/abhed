//go:build !windows

package plugin

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

// A session's command on a driver in another process runs through this
// binary as a bridge to the driver's exec: its output and exit status come
// back, and forwarded variables reach the command and no others.
func TestCommandBridgesToExec(t *testing.T) {
	ctx := context.Background()
	fake := drivertest.New("fake", driver.CapAttach)
	var gotEnv []string
	fake.ExecFunc = func(_ context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		gotEnv = req.Env
		_, _ = req.Stdout.Write([]byte("ran " + req.Command + " in " + req.Cwd + "\n"))
		_, _ = req.Stderr.Write([]byte("warned\n"))
		return driver.ExecResult{ExitCode: 3}, nil
	}
	cl, err := Dial(ctx, "fake", serve(t, fake))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Create(ctx, driver.Spec{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	cmd, err := driver.Command(ctx, cl, "a", driver.CommandRequest{Command: "make test", Cwd: "src"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, "HTTP_PROXY=http://c:t@127.0.0.1:1", "OTHER=x")
	cl.ForwardEnv(cmd, []string{"HTTP_PROXY"})
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("exit: %v", err)
	}
	if string(out) != "ran make test in src\n" || stderr.String() != "warned\n" {
		t.Fatalf("out %q, err %q", out, stderr.String())
	}
	if len(gotEnv) != 1 || gotEnv[0] != "HTTP_PROXY=http://c:t@127.0.0.1:1" {
		t.Fatalf("env reaching the command %q", gotEnv)
	}
	// A driver without attach has no shell to give, so no commands.
	plain, err := Dial(ctx, "fake", serve(t, drivertest.New("fake")))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Info().Has(driver.CapCommand) {
		t.Fatal("command declared without attach")
	}
	if _, err := plain.Command(ctx, "a", driver.CommandRequest{Command: "x"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("command without attach: %v", err)
	}
}
