//go:build !windows

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/driver"
)

// A driver in another process cannot hand out a process of its own, so a
// session's command or terminal there is this binary started again as a
// bridge: it calls the driver's exec, or attach for a shell, with its own
// stdin, stdout and stderr, and exits with the command's status. Killing
// the bridge ends the call, and the driver ends the command.

// commandArg makes this binary the bridge: `abhed commandArg NAME SOCKET ID`,
// with the request in commandEnv.
const commandArg = "__abhed_plugin_command"

// commandEnv carries the bridge's request; forwardEnv names the variables
// of the bridge's own environment it passes to the command.
const (
	commandEnv = "ABHED_PLUGIN_COMMAND"
	forwardEnv = "ABHED_PLUGIN_FORWARD"
)

func init() {
	if len(os.Args) == 5 && os.Args[1] == commandArg {
		os.Exit(runCommand(os.Args[2], os.Args[3], os.Args[4]))
	}
}

// executable is this binary, which runs the bridge; tests replace it.
var executable = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Command builds the bridge for req, for the caller to start and own. It
// counts as running for as long as the caller runs it.
func (cl *Client) Command(ctx context.Context, id string, req driver.CommandRequest) (*exec.Cmd, error) {
	if !cl.commands() {
		return nil, cl.fail(driver.ErrUnsupported, "a session's commands need exec and attach")
	}
	if req.Shell != (req.Command == "") {
		return nil, cl.fail(driver.ErrInvalid, "exactly one of a command and a shell must be asked for")
	}
	exe, err := executable()
	if err != nil {
		return nil, cl.fail(driver.ErrUnavailable, "finding this binary to run the command through: %v", err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, cl.fail(driver.ErrInternal, "%v", err)
	}
	cmd := exec.CommandContext(ctx, exe, commandArg, cl.name, cl.socket, id) // #nosec G204 -- this binary, with the bridge's arguments
	cmd.Env = []string{"PATH=/usr/bin:/bin", commandEnv + "=" + string(b)}
	return cmd, nil
}

// ForwardEnv has the bridge pass the named variables, already in cmd.Env,
// to the command.
func (cl *Client) ForwardEnv(cmd *exec.Cmd, names []string) {
	var have []string
	for _, kv := range cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == forwardEnv && v != "" {
			have = strings.Split(v, ",")
		}
	}
	for _, n := range names {
		if n != "" && !strings.ContainsAny(n, ",=") && !slices.Contains(have, n) {
			have = append(have, n)
		}
	}
	cmd.Env = append(slices.DeleteFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, forwardEnv+"=") }),
		forwardEnv+"="+strings.Join(have, ","))
}

// commands reports whether the driver can run a session's commands and
// terminals: through its exec and its attach.
func (cl *Client) commands() bool { return cl.info.Has(driver.CapAttach) }

// runCommand is the bridge.
func runCommand(name, socket, id string) int {
	var req driver.CommandRequest
	if err := json.Unmarshal([]byte(os.Getenv(commandEnv)), &req); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: the command's request: %v\n", err)
		return 126
	}
	var env []string
	if names := os.Getenv(forwardEnv); names != "" {
		for _, n := range strings.Split(names, ",") {
			if v, ok := os.LookupEnv(n); ok {
				env = append(env, n+"="+v)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	go func() { <-sigs; cancel() }()
	cl, err := Dial(ctx, name, socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 126
	}
	var res driver.ExecResult
	if req.Shell {
		res, err = shell(ctx, cl, id, req.Cwd)
	} else {
		res, err = cl.Exec(ctx, id, driver.ExecRequest{Command: req.Command, Cwd: req.Cwd, Env: env,
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 126
	}
	return res.ExitCode
}

// shell attaches to the sandbox's shell on this terminal, raw, following
// its size.
func shell(ctx context.Context, cl *Client, id, cwd string) (driver.ExecResult, error) {
	fd := int(os.Stdin.Fd()) // #nosec G115 -- a file descriptor fits an int
	var size driver.WinSize
	if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
		size = driver.WinSize{Rows: ws.Rows, Cols: ws.Cols}
	}
	if term.IsTerminal(fd) {
		if old, err := term.MakeRaw(fd); err == nil {
			defer func() { _ = term.Restore(fd, old) }()
		}
	}
	resize := make(chan driver.WinSize, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
					select {
					case resize <- driver.WinSize{Rows: ws.Rows, Cols: ws.Cols}:
					default:
					}
				}
			}
		}
	}()
	return cl.Attach(ctx, id, driver.AttachRequest{Cwd: cwd, Size: size, Stdin: os.Stdin, Stdout: os.Stdout, Resize: resize})
}
