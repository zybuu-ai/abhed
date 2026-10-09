package app

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/zybuu-ai/abhed/internal/sandboxcli"
)

// sandboxCmd is `abhed sandbox`, a server's sandboxes from this command
// line, as the profile abhed login saved.
func sandboxCmd(workspace string, args []string, in io.Reader, out, errOut io.Writer) int {
	path, err := profilesPath(workspace)
	if err != nil {
		_, _ = io.WriteString(errOut, "abhed: "+err.Error()+"\n")
		return sandboxcli.ExitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := sandboxcli.Env{
		Ctx:          ctx,
		ProfilesPath: path,
		KnownHosts:   filepath.Join(filepath.Dir(path), "sandbox_known_hosts"),
		In:           in,
		Out:          out,
		Err:          errOut,
		InAgent:      inAgentCommand,
		SSH:          runSSH,
	}
	if f, ok := in.(*os.File); ok {
		if o, ok := out.(*os.File); ok {
			env.Term = sandboxcli.NewTerminal(f, o)
		}
	}
	if exe, err := os.Executable(); err == nil {
		env.Executable = exe
	}
	return sandboxcli.Main(env, args)
}
