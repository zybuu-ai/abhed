// Package forward carries a byte stream to a port on a sandbox's own
// loopback. The stream always enters through the driver's channel: a relay
// command run inside the sandbox like any other, which connects from there.
// So a sandbox with no network is reached without being given one, and a
// forward can reach only what the sandbox itself could connect to.
package forward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// ReadyWait bounds how long a relay may take to connect.
const ReadyWait = 15 * time.Second

// ready is the byte a relay writes once it is connected.
const ready = 'R'

// Exit codes of the relay that are its own, not the connection's.
const (
	exitRefused = 70
	exitNoTool  = 71
)

// RelayLine is the shell line, run inside a sandbox, that connects to port
// on its loopback and joins the connection to its stdin and stdout. It uses
// bash's /dev/tcp, or nc where there is no bash. It ends when the service
// closes the connection; stdin is kept on fd 4, since a background job's
// own stdin is /dev/null.
func RelayLine(port int) string {
	p := strconv.Itoa(port)
	return `if command -v bash >/dev/null 2>&1; then exec bash -c 'exec 3<>"/dev/tcp/127.0.0.1/$1" || exit 70; printf R; exec 4<&0; cat <&4 >&3 & w=$!; cat <&3; kill "$w" 2>/dev/null; exit 0' abhed-forward ` + p + `; fi
if command -v nc >/dev/null 2>&1; then printf R; exec nc 127.0.0.1 ` + p + `; fi
echo "the sandbox has neither bash nor nc to reach its port" >&2; exit 71`
}

// Stream is one forwarded connection: a running relay.
type Stream struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *capped
	kill   func()
	in     atomic.Int64
	out    atomic.Int64

	once    sync.Once
	onClose func(in, out int64)
	closed  chan struct{}
}

// Open starts cmd, a relay built from RelayLine, and returns the stream
// once the relay says it is connected. kill ends the relay and what it
// started; onClose runs once, after it has ended, with the bytes each way.
func Open(ctx context.Context, cmd *exec.Cmd, port int, kill func(), onClose func(in, out int64)) (*Stream, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, driver.Errorf(driver.ErrInternal, "%v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, driver.Errorf(driver.ErrInternal, "%v", err)
	}
	errs := &capped{max: 4096}
	cmd.Stderr = errs
	if err := cmd.Start(); err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "the relay was not started: %v", err)
	}
	s := &Stream{cmd: cmd, stdin: stdin, stdout: stdout, stderr: errs, kill: kill, onClose: onClose, closed: make(chan struct{})}
	got := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := io.ReadFull(stdout, b[:])
		if err == nil && b[0] != ready {
			err = errors.New("the relay answered unexpectedly")
		}
		got <- err
	}()
	wait := time.NewTimer(ReadyWait)
	defer wait.Stop()
	select {
	case err = <-got:
	case <-ctx.Done():
		err = ctx.Err()
	case <-wait.C:
		err = errors.New("the relay did not connect in time")
	}
	if err != nil {
		s.end()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, s.refusal(port, err)
	}
	return s, nil
}

// refusal says why a relay ended before it connected.
func (s *Stream) refusal(port int, err error) error {
	msg := strings.TrimSpace(s.stderr.String())
	code := -1
	if s.cmd.ProcessState != nil {
		code = s.cmd.ProcessState.ExitCode()
	}
	switch {
	case code == exitRefused:
		return driver.Errorf(driver.ErrUnavailable, "nothing accepted a connection on port %d inside the sandbox", port)
	case code == exitNoTool:
		return driver.Errorf(driver.ErrUnavailable, "the sandbox has neither bash nor nc, so its port cannot be reached")
	case msg != "":
		return driver.Errorf(driver.ErrUnavailable, "forwarding to port %d: %s", port, msg)
	}
	return driver.Errorf(driver.ErrUnavailable, "forwarding to port %d: %v", port, err)
}

func (s *Stream) Read(p []byte) (int, error) {
	n, err := s.stdout.Read(p)
	s.out.Add(int64(n))
	return n, err
}

func (s *Stream) Write(p []byte) (int, error) {
	n, err := s.stdin.Write(p)
	s.in.Add(int64(n))
	return n, err
}

// CloseWrite ends what is sent, leaving the reply to be read.
func (s *Stream) CloseWrite() error { return s.stdin.Close() }

// Close ends the relay. It is safe to call more than once.
func (s *Stream) Close() error {
	s.end()
	return nil
}

// Done is closed once the stream has ended.
func (s *Stream) Done() <-chan struct{} { return s.closed }

func (s *Stream) end() {
	s.once.Do(func() {
		_ = s.stdin.Close()
		s.kill()
		_ = s.cmd.Wait()
		if s.onClose != nil {
			s.onClose(s.in.Load(), s.out.Load())
		}
		close(s.closed)
	})
}

// capped keeps the first max bytes written to it.
type capped struct {
	mu  sync.Mutex
	max int
	b   bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// Set is the streams of one incarnation of an instance, closed together
// when it stops.
type Set struct {
	mu      sync.Mutex
	streams map[io.Closer]struct{}
	closed  bool
}

// Add tracks c; it fails, and closes c, once the set is closed.
func (s *Set) Add(c io.Closer) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return driver.Errorf(driver.ErrConflict, "the instance stopped")
	}
	if s.streams == nil {
		s.streams = map[io.Closer]struct{}{}
	}
	s.streams[c] = struct{}{}
	s.mu.Unlock()
	return nil
}

// Remove stops tracking c.
func (s *Set) Remove(c io.Closer) {
	s.mu.Lock()
	delete(s.streams, c)
	s.mu.Unlock()
}

// Close closes every stream and refuses new ones.
func (s *Set) Close() {
	s.mu.Lock()
	s.closed = true
	all := make([]io.Closer, 0, len(s.streams))
	for c := range s.streams {
		all = append(all, c)
	}
	s.streams = nil
	s.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

// Len is how many streams are open.
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}
