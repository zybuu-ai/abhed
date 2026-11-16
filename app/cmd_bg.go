package app

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/internal/bgcli"
)

// bgCmd is `abhed bg`, background jobs on a server, as the profile abhed
// login saved.
func bgCmd(workspace string, args []string, in io.Reader, out, errOut io.Writer) int {
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
	env := bgcli.Env{Ctx: ctx, ProfilesPath: path, In: in, Out: out, Err: errOut, InAgent: inAgentCommand}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) {
		env.Interactive = true
	}
	return bgcli.Main(env, args)
}
