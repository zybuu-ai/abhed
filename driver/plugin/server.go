package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// Listen makes a Unix socket at path that only this user can connect to.
// The folder holding it must not be writable by anyone else, or another
// user could swap the socket for their own.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by other users; put the driver socket in a private folder", dir)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// Serve answers calls on l with d until ctx ends or l fails.
func Serve(ctx context.Context, l net.Listener, d driver.Driver) error {
	if err := driver.Check(d); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(ctx, newConn(c), d)
		}()
	}
}

func serveConn(parent context.Context, c *conn, d driver.Driver) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer func() { _ = c.close() }()
	go func() {
		<-ctx.Done()
		_ = c.close()
	}()

	hello, err := c.read()
	if err != nil || hello.Type != tHello {
		_ = c.write(errorFrame(driver.Errorf(driver.ErrVersion, "expected hello")))
		return
	}
	v := negotiate(hello.Versions)
	if v == 0 {
		_ = c.write(errorFrame(driver.Errorf(driver.ErrVersion, "no common protocol version: offered %v, supported %v", hello.Versions, Versions)))
		return
	}
	info := d.Info()
	if err := c.write(frame{Type: tHello, Version: v, Info: &info}); err != nil {
		return
	}
	call, err := c.read()
	if err != nil {
		return
	}
	if call.Type != tCall || call.Params == nil {
		_ = c.write(errorFrame(driver.Errorf(driver.ErrInvalid, "expected a call with params")))
		return
	}
	s := &session{ctx: ctx, cancel: cancel, c: c, d: d, p: call.Params}
	res, err := s.dispatch(call.Method)
	if err != nil {
		_ = c.write(errorFrame(err))
		return
	}
	if res == nil {
		return // the method sent its own ending
	}
	b, err := json.Marshal(res)
	if err != nil {
		_ = c.write(errorFrame(err))
		return
	}
	_ = c.write(frame{Type: tResult, Result: b})
}

type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	c      *conn
	d      driver.Driver
	p      *params
}

type empty struct{}

func (s *session) dispatch(method string) (any, error) {
	p := s.p
	switch method {
	case mCreate:
		if p.Spec == nil {
			return nil, driver.Errorf(driver.ErrInvalid, "create without a spec")
		}
		return s.d.Create(s.ctx, *p.Spec)
	case mStart:
		return s.d.Start(s.ctx, p.ID)
	case mStop:
		return s.d.Stop(s.ctx, p.ID, time.Duration(p.GraceMS)*time.Millisecond)
	case mDelete:
		return empty{}, s.d.Delete(s.ctx, p.ID)
	case mStatus:
		return s.d.Status(s.ctx, p.ID)
	case mExec:
		if p.Exec == nil {
			return nil, driver.Errorf(driver.ErrInvalid, "exec without a request")
		}
		in := s.stdin(nil)
		defer in.Close()
		req := *p.Exec
		req.Stdin, req.Stdout, req.Stderr = in, chunkWriter{s.c, tStdout}, chunkWriter{s.c, tStderr}
		return s.d.Exec(s.ctx, p.ID, req)
	case mAttach:
		if p.Attach == nil {
			return nil, driver.Errorf(driver.ErrInvalid, "attach without a request")
		}
		resize := make(chan driver.WinSize, 4)
		in := s.stdin(resize)
		defer in.Close()
		req := *p.Attach
		req.Stdin, req.Stdout, req.Resize = in, chunkWriter{s.c, tStdout}, resize
		return driver.Attach(s.ctx, s.d, p.ID, req)
	case mCopyIn:
		pr, pw := io.Pipe()
		go s.readData(pw)
		err := driver.CopyIn(s.ctx, s.d, p.ID, p.Path, pr)
		_ = pr.CloseWithError(errors.New("copy ended"))
		return empty{}, err
	case mCopyOut:
		w := chunkWriter{s.c, tData}
		if err := driver.CopyOut(s.ctx, s.d, p.ID, p.Path, w); err != nil {
			return nil, err
		}
		if err := s.c.write(frame{Type: tEOF}); err != nil {
			return nil, err
		}
		return empty{}, nil
	case mForward:
		rwc, err := driver.Forward(s.ctx, s.d, p.ID, p.Port)
		if err != nil {
			return nil, err
		}
		defer rwc.Close()
		if err := s.c.write(frame{Type: tResult, Result: json.RawMessage(`{}`)}); err != nil {
			return nil, err
		}
		go func() {
			_ = pump(s.c, rwc, tData, tEOF)
			s.cancel()
		}()
		s.readData(rwc)
		return nil, nil
	case mSnapshot:
		return driver.Snapshot(s.ctx, s.d, p.ID, p.Name)
	case mRestore:
		if p.Ref == nil {
			return nil, driver.Errorf(driver.ErrInvalid, "restore without a snapshot")
		}
		return empty{}, driver.Restore(s.ctx, s.d, p.ID, *p.Ref)
	}
	return nil, driver.Errorf(driver.ErrUnsupported, "unknown method %q", method)
}

// stdin feeds stdin frames to the returned reader and resize frames to
// resize. A connection that breaks cancels the call, so the driver kills
// what it started.
func (s *session) stdin(resize chan<- driver.WinSize) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := s.c.read()
			if err != nil {
				_ = pw.CloseWithError(err)
				s.cancel()
				return
			}
			switch f.Type {
			case tStdin:
				if _, err := pw.Write(f.Data); err != nil {
					continue // the command stopped reading; keep serving resizes
				}
			case tStdinEOF:
				_ = pw.Close()
			case tResize:
				if resize != nil {
					select {
					case resize <- driver.WinSize{Rows: f.Rows, Cols: f.Cols}:
					default:
					}
				}
			default:
				_ = pw.CloseWithError(errProtocol)
				s.cancel()
				return
			}
		}
	}()
	return pr
}

// readData writes data frames to w until eof; anything else cancels the call.
func (s *session) readData(w io.WriteCloser) {
	for {
		f, err := s.c.read()
		if err != nil {
			s.cancel()
			_ = closeWithError(w, err)
			return
		}
		switch f.Type {
		case tData:
			if _, err := w.Write(f.Data); err != nil {
				s.cancel()
				return
			}
		case tEOF:
			_ = w.Close()
			return
		default:
			s.cancel()
			_ = closeWithError(w, errProtocol)
			return
		}
	}
}

func closeWithError(w io.WriteCloser, err error) error {
	if p, ok := w.(*io.PipeWriter); ok {
		return p.CloseWithError(err)
	}
	return w.Close()
}
