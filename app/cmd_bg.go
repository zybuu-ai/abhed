package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/bgcli"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// bgCmd is `abhed bg`, background jobs on a server, as the profile abhed
// login saved.
func bgCmd(workspace string, args []string, trust config.TrustChoice, in io.Reader, out, errOut io.Writer) int {
	if !profileNetwork(errOut) {
		return bgcli.ExitFailed
	}
	path, err := profilesPath(workspace)
	if err != nil {
		_, _ = io.WriteString(errOut, "abhed: "+err.Error()+"\n")
		return bgcli.ExitFailed
	}
	// Ctrl-C leaves attach; the job runs on.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := bgcli.Env{Ctx: ctx, ProfilesPath: path, In: in, Out: out, Err: errOut, InAgent: inAgentCommand,
		Workspace: workspace}
	// A snapshot carries nothing the person's read rules keep from the agent,
	// as their configuration for this workspace has them.
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Quiet: true, Trust: trust})
	if err != nil {
		env.SnapshotErr = fmt.Errorf("the configuration, which says what a snapshot may carry: %w", err)
	} else {
		env.Withheld = toolset.ReadWithheld(cfg, workspace)
	}
	// Nor a value from the person's secrets store.
	if red, err := secrets.Default().LoadRedactor(); err != nil {
		env.SnapshotErr = err
	} else {
		env.HoldsStored = func(b []byte) bool { return !bytes.Equal(red.Redact(b), b) }
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) {
		env.Interactive = true
	}
	return bgcli.Main(env, args)
}
