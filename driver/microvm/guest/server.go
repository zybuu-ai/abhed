//go:build !windows

package guest

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Server runs the host's requests inside the guest.
type Server struct {
	// Env is every command's base environment; a request may add names, never
	// replace these.
	Env []string
	// Dir is the default working directory, and the base of a relative one.
	Dir string
	// Shell runs Command strings with -c, and is the terminal's shell.
	Shell string
	// Credential, when set, is who commands run as, so a command cannot
	// signal or replace the agent that reports its exit code.
	Credential *syscall.Credential
	// PowerOff is called after a shutdown request has been acknowledged.
	PowerOff func()
}

// ServeConn answers one call on rw and closes it.
func (s *Server) ServeConn(rw io.ReadWriteCloser) {
	defer func() { _ = rw.Close() }()
	c := newConn(rw)
	f, err := c.read()
	if err != nil {
		return
	}
	if f.Type != tHello || f.Version != Version {
		_ = c.write(frame{Type: tError, Message: "unsupported protocol version"})
		return
	}
	if c.write(frame{Type: tHello, Version: Version}) != nil {
		return
	}
	f, err = c.read()
	if err != nil {
		return
	}
	switch {
	case f.Type == tPing:
		_ = c.write(frame{Type: tOK})
	case f.Type == tShutdown:
		if c.write(frame{Type: tOK}) == nil && s.PowerOff != nil {
			s.PowerOff()
		}
	case f.Type == tExec && f.Exec != nil:
		code, err := s.exec(c, *f.Exec)
		if err != nil {
			_ = c.write(frame{Type: tError, Message: err.Error()})
			return
		}
		_ = c.write(frame{Type: tExit, Code: &code})
	default:
		_ = c.write(frame{Type: tError, Message: "expected exec, ping or shutdown, got " + f.Type})
	}
}

func (s *Server) env(extra []string) ([]string, error) {
	set := map[string]bool{}
	for _, kv := range s.Env {
		k, _, _ := strings.Cut(kv, "=")
		set[k] = true
	}
	out := append([]string(nil), s.Env...)
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, errors.New("env entry " + kv + " is not NAME=value")
		}
		if set[k] {
			return nil, errors.New("env " + k + " is set by the guest and cannot be replaced")
		}
		out = append(out, kv)
	}
	return out, nil
}

func (s *Server) dir(d string) string {
	if d == "" {
		return s.Dir
	}
	if !path.IsAbs(d) {
		d = path.Join(s.Dir, d)
	}
	return path.Clean(d)
}

// exec runs req, streaming its output, and returns its exit code. A
// connection that breaks before the command ends kills its process group.
func (s *Server) exec(c *conn, req ExecRequest) (int, error) {
	if err := req.validate(); err != nil {
		return 0, err
	}
	env, err := s.env(req.Env)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cmd *exec.Cmd
	switch {
	case req.TTY:
		cmd = exec.CommandContext(ctx, s.Shell, "-i")
		env = append(env, "TERM=xterm-256color")
	case req.Command != "":
		cmd = exec.CommandContext(ctx, s.Shell, "-c", req.Command)
	default:
		cmd = exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	}
	cmd.Env, cmd.Dir = env, s.dir(req.Cwd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: s.Credential}
	// The whole group goes when the command is cancelled, not only its shell.
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	if req.TTY {
		return s.terminal(ctx, cancel, c, cmd, req)
	}
	cmd.SysProcAttr.Setpgid = true
	stdin, pw := io.Pipe()
	cmd.Stdin = stdin
	cmd.Stdout = frameWriter{c, tStdout}
	cmd.Stderr = frameWriter{c, tStderr}
	go readInput(c, cancel, pw, nil)
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return 0, err
	}
	err = cmd.Wait()
	_ = stdin.Close()
	return exitCode(ctx, cmd, err)
}

func (s *Server) terminal(ctx context.Context, cancel context.CancelFunc, c *conn, cmd *exec.Cmd, req ExecRequest) (int, error) {
	size := &pty.Winsize{Rows: req.Rows, Cols: req.Cols}
	if size.Rows == 0 || size.Cols == 0 {
		size = &pty.Winsize{Rows: 30, Cols: 100}
	}
	cmd.SysProcAttr.Setsid, cmd.SysProcAttr.Setctty = true, true
	tty, err := pty.StartWithAttrs(cmd, size, cmd.SysProcAttr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tty.Close() }()
	go readInput(c, cancel, tty, tty)
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(frameWriter{c, tStdout}, tty)
	}()
	err = cmd.Wait()
	select {
	case <-copied:
	case <-time.After(time.Second):
	}
	return exitCode(ctx, cmd, err)
}

// readInput feeds stdin and resize frames to the command. A terminal keeps
// its input open at stdin_eof; a broken connection cancels the command.
func readInput(c *conn, cancel context.CancelFunc, w io.WriteCloser, tty *os.File) {
	for {
		f, err := c.read()
		if err != nil {
			cancel()
			if pw, ok := w.(*io.PipeWriter); ok {
				_ = pw.CloseWithError(err)
			}
			return
		}
		switch f.Type {
		case tStdin:
			if w != nil {
				if _, err := w.Write(f.Data); err != nil {
					w = nil
				}
			}
		case tStdinEOF:
			if tty == nil && w != nil {
				_ = w.Close()
				w = nil
			}
		case tResize:
			if tty != nil && f.Rows > 0 && f.Cols > 0 {
				_ = pty.Setsize(tty, &pty.Winsize{Rows: f.Rows, Cols: f.Cols})
			}
		default:
			cancel()
			return
		}
	}
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return cmd.Process.Kill()
}

// exitCode is how cmd ended: its exit status, or 128 plus the signal that
// ended it. A command that did not run, or was cancelled because the host
// went away, is an error.
func exitCode(ctx context.Context, cmd *exec.Cmd, err error) (int, error) {
	if cmd.ProcessState == nil {
		return 0, err
	}
	if ctx.Err() != nil {
		return 0, errors.New("the connection to the host ended")
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		return 0, err
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return cmd.ProcessState.ExitCode(), nil
}

// frameWriter is an io.Writer sending data frames of one type.
type frameWriter struct {
	c   *conn
	typ string
}

func (w frameWriter) Write(p []byte) (int, error) {
	if err := w.c.chunks(w.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
