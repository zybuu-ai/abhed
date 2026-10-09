package microvm

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/driver/microvm/guest"
)

// procMachine is a VM run by one VMM process, reached through the Unix
// socket its vsock bridge listens on.
type procMachine struct {
	cmd    *exec.Cmd
	sock   string
	exited chan struct{}
	out    *tailBuffer
	// reap kills what may outlive the process (its cgroup); cleanup removes
	// the boot's files once it has exited. Either may be nil.
	reap, cleanup func() error

	// guest are the listeners for the guest's connections, by vsock port.
	guest map[uint32]net.Listener

	once    sync.Once
	killErr error
}

func (m *procMachine) guestListener(port uint32) net.Listener { return m.guest[port] }

// startMachine starts cmd, whose output is the guest console.
func startMachine(cmd *exec.Cmd, sock string, guestLns map[uint32]net.Listener, reap, cleanup func() error) (*procMachine, error) {
	out := &tailBuffer{max: 8 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		closeAll(guestLns)
		return nil, err
	}
	m := &procMachine{cmd: cmd, sock: sock, exited: make(chan struct{}), out: out, reap: reap, cleanup: cleanup, guest: guestLns}
	go func() { _ = cmd.Wait(); close(m.exited) }()
	return m, nil
}

func (m *procMachine) done() <-chan struct{} { return m.exited }

func (m *procMachine) console() string { return m.out.String() }

func (m *procMachine) dial(ctx context.Context) (net.Conn, error) {
	c, err := dialUnix(ctx, m.sock)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	err = guest.HybridConnect(c, guest.Port)
	_ = c.SetDeadline(time.Time{})
	if !stop() || err != nil {
		_ = c.Close()
		if err == nil {
			err = ctx.Err()
		}
		return nil, err
	}
	return c, nil
}

func (m *procMachine) kill() error {
	m.once.Do(func() {
		closeAll(m.guest)
		if m.cmd.Process != nil {
			_ = m.cmd.Process.Kill()
		}
		var errs []error
		if m.reap != nil {
			errs = append(errs, m.reap())
		}
		select {
		case <-m.exited:
			if m.cleanup != nil {
				errs = append(errs, m.cleanup())
			}
		case <-time.After(10 * time.Second):
			errs = append(errs, errors.New("the VMM did not exit after it was killed"))
		}
		m.killErr = errors.Join(errs...)
	})
	return m.killErr
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

// String is the last 2 KiB, printable characters only: the console is the
// guest's to write, so it is shown, never interpreted.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	b := t.b
	if len(b) > 2048 {
		b = b[len(b)-2048:]
	}
	s := string(b)
	t.mu.Unlock()
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && (r < 0x7f || r > 0x9f) && r != 0xfffd) {
			return r
		}
		return -1
	}, strings.TrimSpace(s))
}

func closeAll(lns map[uint32]net.Listener) {
	for _, l := range lns {
		_ = l.Close()
	}
}

// listenGuestPorts listens at path(port) for each port, before the VM is
// started; own gives each socket to the user the VMM runs as.
func listenGuestPorts(ports []uint32, path func(uint32) string, own func(string) error) (map[uint32]net.Listener, error) {
	lns := map[uint32]net.Listener{}
	for _, p := range ports {
		ln, err := net.Listen("unix", path(p))
		if err == nil && own != nil {
			if err = own(path(p)); err != nil {
				_ = ln.Close()
			}
		}
		if err != nil {
			closeAll(lns)
			return nil, err
		}
		lns[p] = ln
	}
	return lns, nil
}
