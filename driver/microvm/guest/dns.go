package guest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// EgressDNSPort is the host's vsock port for lookups. The host listens on it
// only for a sandbox whose egress allows the network and has a resolver.
const EgressDNSPort = 1026

// tDNS asks the guest to start its resolver on 127.0.0.1:53.
const tDNS = "dns"

// DNS asks the guest to answer lookups on its loopback through the host.
func DNS(ctx context.Context, rw io.ReadWriteCloser) error { return simple(ctx, rw, tDNS) }

// dnsTimeout bounds one lookup through the host.
const dnsTimeout = 10 * time.Second

// maxDNSInFlight bounds the lookups the guest passes on at once.
const maxDNSInFlight = 64

// ServeDNS answers queries on udp and tcp by passing each to the host over
// a connection from dial, framed with a two-byte length as DNS over TCP is,
// until both are closed. The host holds the credential and applies the
// egress rules; the guest only carries the bytes.
func ServeDNS(udp net.PacketConn, tcp net.Listener, dial func() (io.ReadWriteCloser, error)) {
	slots := make(chan struct{}, maxDNSInFlight)
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = c.Close()
				continue
			}
			go func() {
				defer func() { <-slots; _ = c.Close() }()
				for {
					_ = c.SetDeadline(time.Now().Add(dnsTimeout))
					q, err := readFrame(c)
					if err != nil {
						return
					}
					a, err := exchange(dial, q)
					if err != nil || writeFrame(c, a) != nil {
						return
					}
				}
			}()
		}
	}()
	buf := make([]byte, 0xFFFF)
	for {
		n, from, err := udp.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		go func() {
			defer func() { <-slots }()
			if a, err := exchange(dial, q); err == nil {
				_, _ = udp.WriteTo(a, from)
			}
		}()
	}
}

// exchange sends one query to the host and reads its answer.
func exchange(dial func() (io.ReadWriteCloser, error), q []byte) ([]byte, error) {
	c, err := dial()
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if d, ok := c.(interface{ SetDeadline(time.Time) error }); ok {
		_ = d.SetDeadline(time.Now().Add(dnsTimeout))
	}
	if err := writeFrame(c, q); err != nil {
		return nil, err
	}
	return readFrame(c)
}

func readFrame(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeFrame(w io.Writer, b []byte) error {
	if len(b) > 0xFFFF {
		return errors.New("a DNS message over 65535 bytes")
	}
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...)) // #nosec G115 -- checked
	if err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	return nil
}
