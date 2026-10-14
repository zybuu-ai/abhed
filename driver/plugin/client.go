package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// Client is a driver.Driver served by another process. It refuses a call
// for a capability the driver did not declare before sending it.
type Client struct {
	name   string
	socket string
	info   driver.Info
}

// Dial checks the driver at socket answers as name and returns a client
// for it. The driver's Info is fixed from here on: a later connection that
// reports anything else is refused, since what was registered no longer
// describes what answers.
func Dial(ctx context.Context, name, socket string) (*Client, error) {
	cl := &Client{name: name, socket: socket}
	c, info, err := cl.open(ctx)
	if err != nil {
		return nil, err
	}
	_ = c.close()
	cl.info = info
	if err := driver.Check(cl); err != nil {
		return nil, err
	}
	return cl, nil
}

// Info is the driver's, with command added where it has exec and attach:
// this client runs a session's commands through them.
func (cl *Client) Info() driver.Info {
	info := cl.info
	if cl.commands() && !info.Has(driver.CapCommand) {
		info.Capabilities = append(slices.Clone(info.Capabilities), driver.CapCommand)
	}
	return info
}

func (cl *Client) fail(kind error, format string, args ...any) error {
	return driver.Errorf(kind, "driver %s: %s", cl.name, fmt.Sprintf(format, args...))
}

// open connects and completes the handshake.
func (cl *Client) open(ctx context.Context) (*conn, driver.Info, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", cl.socket)
	if err != nil {
		return nil, driver.Info{}, cl.fail(driver.ErrUnavailable, "%v", err)
	}
	c := newConn(nc)
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	if err := c.write(frame{Type: tHello, Versions: Versions}); err != nil {
		_ = c.close()
		return nil, driver.Info{}, cl.fail(driver.ErrUnavailable, "%v", err)
	}
	f, err := c.read()
	if err != nil {
		_ = c.close()
		return nil, driver.Info{}, cl.fail(driver.ErrUnavailable, "handshake: %v", err)
	}
	switch {
	case f.Type == tError && f.Error != nil:
		_ = c.close()
		return nil, driver.Info{}, cl.fail(driver.KindOf(f.Error.Code), "%s", f.Error.Message)
	case f.Type != tHello || f.Info == nil:
		_ = c.close()
		return nil, driver.Info{}, cl.fail(driver.ErrVersion, "handshake answered with %q", f.Type)
	case !slices.Contains(Versions, f.Version):
		_ = c.close()
		return nil, driver.Info{}, cl.fail(driver.ErrVersion, "driver chose protocol version %d, which was not offered", f.Version)
	case f.Info.Name != cl.name:
		_ = c.close()
		return nil, driver.Info{}, cl.fail(driver.ErrInvalid, "answers as %q", f.Info.Name)
	case cl.info.Name != "" && !sameInfo(cl.info, *f.Info):
		_ = c.close()
		return nil, driver.Info{}, cl.fail(driver.ErrUnavailable, "changed since it was registered (now %s %v)", f.Info.Version, f.Info.Capabilities)
	}
	return c, *f.Info, nil
}

func sameInfo(a, b driver.Info) bool {
	return a.Name == b.Name && a.Version == b.Version && slices.Equal(a.Capabilities, b.Capabilities)
}

// start opens a connection, sends the call, and closes the connection when
// ctx ends. stop releases the watcher; the caller still closes the conn.
func (cl *Client) start(ctx context.Context, method string, p params) (*conn, func(), error) {
	c, _, err := cl.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = c.close()
		case <-done:
		}
	}()
	stop := func() { close(done) }
	if err := c.write(frame{Type: tCall, Method: method, Params: &p}); err != nil {
		stop()
		_ = c.close()
		return nil, nil, cl.fail(driver.ErrUnavailable, "%v", err)
	}
	return c, stop, nil
}

// end reads the frame that ends a call into out.
func (cl *Client) end(f frame, out any) error {
	switch f.Type {
	case tResult:
		if out == nil {
			return nil
		}
		if len(f.Result) == 0 {
			return cl.fail(driver.ErrInternal, "result without a body")
		}
		if err := json.Unmarshal(f.Result, out); err != nil {
			return cl.fail(driver.ErrInternal, "malformed result: %v", err)
		}
		return nil
	case tError:
		if f.Error == nil {
			return cl.fail(driver.ErrInternal, "error frame without an error")
		}
		return cl.fail(driver.KindOf(f.Error.Code), "%s", f.Error.Message)
	}
	return cl.fail(driver.ErrInternal, "unexpected %q frame", f.Type)
}

func (cl *Client) readErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return cl.fail(driver.ErrUnavailable, "connection ended before the call did: %v", err)
}

func (cl *Client) simple(ctx context.Context, method string, p params, out any) error {
	c, stop, err := cl.start(ctx, method, p)
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()
	defer stop()
	f, err := c.read()
	if err != nil {
		return cl.readErr(ctx, err)
	}
	return cl.end(f, out)
}

func (cl *Client) Create(ctx context.Context, spec driver.Spec) (driver.Status, error) {
	var st driver.Status
	return st, cl.simple(ctx, mCreate, params{Spec: &spec}, &st)
}

func (cl *Client) Start(ctx context.Context, id string) (driver.Status, error) {
	var st driver.Status
	return st, cl.simple(ctx, mStart, params{ID: id}, &st)
}

func (cl *Client) Stop(ctx context.Context, id string, grace time.Duration) (driver.Status, error) {
	var st driver.Status
	return st, cl.simple(ctx, mStop, params{ID: id, GraceMS: grace.Milliseconds()}, &st)
}

func (cl *Client) Delete(ctx context.Context, id string) error {
	return cl.simple(ctx, mDelete, params{ID: id}, nil)
}

func (cl *Client) Status(ctx context.Context, id string) (driver.Status, error) {
	var st driver.Status
	return st, cl.simple(ctx, mStatus, params{ID: id}, &st)
}

// stream sends in as stdin and resize frames, writes stdout and stderr
// frames, and returns the exit code only from a result that carries one.
func (cl *Client) stream(ctx context.Context, method string, p params, in io.Reader, resize <-chan driver.WinSize, stdout, stderr io.Writer) (driver.ExecResult, error) {
	c, stop, err := cl.start(ctx, method, p)
	if err != nil {
		return driver.ExecResult{}, err
	}
	defer func() { _ = c.close() }()
	defer stop()
	go func() { _ = pump(c, in, tStdin, tStdinEOF) }()
	if resize != nil {
		go func() {
			for ws := range resize {
				if c.write(frame{Type: tResize, Rows: ws.Rows, Cols: ws.Cols}) != nil {
					return
				}
			}
		}()
	}
	for {
		f, err := c.read()
		if err != nil {
			return driver.ExecResult{}, cl.readErr(ctx, err)
		}
		switch f.Type {
		case tStdout, tStderr:
			w := stdout
			if f.Type == tStderr {
				w = stderr
			}
			if w != nil {
				if _, err := w.Write(f.Data); err != nil {
					return driver.ExecResult{}, err
				}
			}
			continue
		}
		var res struct {
			ExitCode *int `json:"exit_code"`
		}
		if err := cl.end(f, &res); err != nil {
			return driver.ExecResult{}, err
		}
		if res.ExitCode == nil {
			return driver.ExecResult{}, cl.fail(driver.ErrInternal, "%s ended without an exit code", method)
		}
		return driver.ExecResult{ExitCode: *res.ExitCode}, nil
	}
}

func (cl *Client) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.ExecResult, error) {
	if err := req.Validate(); err != nil {
		return driver.ExecResult{}, err
	}
	return cl.stream(ctx, mExec, params{ID: id, Exec: &req}, req.Stdin, nil, req.Stdout, req.Stderr)
}

func (cl *Client) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.ExecResult, error) {
	if !cl.info.Has(driver.CapAttach) {
		return driver.ExecResult{}, cl.fail(driver.ErrUnsupported, "attach is not supported")
	}
	return cl.stream(ctx, mAttach, params{ID: id, Attach: &req}, req.Stdin, req.Resize, req.Stdout, nil)
}

func (cl *Client) CopyIn(ctx context.Context, id, dst string, tar io.Reader) error {
	if !cl.info.Has(driver.CapCopy) {
		return cl.fail(driver.ErrUnsupported, "copy is not supported")
	}
	c, stop, err := cl.start(ctx, mCopyIn, params{ID: id, Path: dst})
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()
	defer stop()
	go func() { _ = pump(c, tar, tData, tEOF) }()
	f, err := c.read()
	if err != nil {
		return cl.readErr(ctx, err)
	}
	return cl.end(f, nil)
}

func (cl *Client) CopyOut(ctx context.Context, id, src string, tar io.Writer) error {
	if !cl.info.Has(driver.CapCopy) {
		return cl.fail(driver.ErrUnsupported, "copy is not supported")
	}
	c, stop, err := cl.start(ctx, mCopyOut, params{ID: id, Path: src})
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()
	defer stop()
	sawEOF := false
	for {
		f, err := c.read()
		if err != nil {
			return cl.readErr(ctx, err)
		}
		switch f.Type {
		case tData:
			if sawEOF {
				return cl.fail(driver.ErrInternal, "data after eof")
			}
			if _, err := tar.Write(f.Data); err != nil {
				return err
			}
			continue
		case tEOF:
			sawEOF = true
			continue
		}
		if err := cl.end(f, nil); err != nil {
			return err
		}
		if !sawEOF {
			return cl.fail(driver.ErrInternal, "copy ended without eof")
		}
		return nil
	}
}

func (cl *Client) Forward(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if !cl.info.Has(driver.CapForward) {
		return nil, cl.fail(driver.ErrUnsupported, "forward is not supported")
	}
	c, stop, err := cl.start(ctx, mForward, params{ID: id, Port: port})
	if err != nil {
		return nil, err
	}
	f, err := c.read()
	if err == nil {
		err = cl.end(f, nil)
	} else {
		err = cl.readErr(ctx, err)
	}
	stop() // the stream outlives the call's context
	if err != nil {
		_ = c.close()
		return nil, err
	}
	_ = c.c.SetDeadline(time.Time{})
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := c.read()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			switch f.Type {
			case tData:
				if _, err := pw.Write(f.Data); err != nil {
					return
				}
			case tEOF:
				_ = pw.Close()
				return
			default:
				_ = pw.CloseWithError(errProtocol)
				_ = c.close()
				return
			}
		}
	}()
	return &stream{c: c, r: pr}, nil
}

type stream struct {
	c *conn
	r *io.PipeReader
}

func (s *stream) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *stream) Write(p []byte) (int, error) { return chunkWriter{s.c, tData}.Write(p) }
func (s *stream) Close() error {
	_ = s.c.write(frame{Type: tEOF})
	_ = s.r.Close()
	return s.c.close()
}

func (cl *Client) Snapshot(ctx context.Context, id, name string) (driver.SnapshotRef, error) {
	var ref driver.SnapshotRef
	if !cl.info.Has(driver.CapSnapshot) {
		return ref, cl.fail(driver.ErrUnsupported, "snapshot is not supported")
	}
	return ref, cl.simple(ctx, mSnapshot, params{ID: id, Name: name}, &ref)
}

func (cl *Client) Restore(ctx context.Context, id string, ref driver.SnapshotRef) error {
	if !cl.info.Has(driver.CapSnapshot) {
		return cl.fail(driver.ErrUnsupported, "restore is not supported")
	}
	return cl.simple(ctx, mRestore, params{ID: id, Ref: &ref}, nil)
}
