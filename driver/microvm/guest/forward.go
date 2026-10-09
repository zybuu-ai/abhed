package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
)

// EgressPort is the host's vsock port for the egress broker. The guest
// connects to it; the host listens on it only for a sandbox whose policy
// allows the network.
const EgressPort = 1025

// maxForwarded bounds the guest's open connections to the broker.
const maxForwarded = 128

// Forward listens on 127.0.0.1:port inside the guest and joins each
// connection to one dial opens to the host's broker. It is how commands
// told to use the proxy at that address reach it with no network device.
func Forward(port uint16, dial func() (io.ReadWriteCloser, error)) (net.Listener, error) {
	if port == 0 {
		return nil, errors.New("no egress port")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		return nil, err
	}
	sem := make(chan struct{}, maxForwarded)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case sem <- struct{}{}:
			default:
				_ = c.Close()
				continue
			}
			go func() {
				defer func() { <-sem }()
				up, err := dial()
				if err != nil {
					_ = c.Close()
					return
				}
				Join(c, up)
			}()
		}
	}()
	return ln, nil
}

// Join copies both ways between a and b until either ends, then closes both.
func Join(a, b io.ReadWriteCloser) {
	var once sync.Once
	end := func() { once.Do(func() { _ = a.Close(); _ = b.Close() }) }
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); end(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); end(); done <- struct{}{} }()
	<-done
	<-done
}

// Network asks the guest to forward 127.0.0.1:port to the host's broker.
func Network(ctx context.Context, rw io.ReadWriteCloser, port uint16) error {
	defer func() { _ = rw.Close() }()
	stop := watch(ctx, rw)
	defer stop()
	c, err := open(rw)
	if err != nil {
		return ctxErr(ctx, err)
	}
	if err := c.write(frame{Type: tNet, Port: port}); err != nil {
		return ctxErr(ctx, err)
	}
	f, err := c.read()
	if err != nil {
		return ctxErr(ctx, err)
	}
	switch f.Type {
	case tOK:
		return nil
	case tError:
		return &Error{Message: f.Message}
	}
	return fmt.Errorf("%w: unexpected %q frame", errProtocol, f.Type)
}
