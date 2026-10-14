package microvm

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"
)

// dnsTimeout bounds one lookup through the proxy's resolver.
const dnsTimeout = 10 * time.Second

// maxDNSMessage bounds a query or an answer, as DNS over TCP frames them.
const maxDNSMessage = 0xFFFF

// serveDNS answers the guest's lookups: each connection carries queries
// framed with a two-byte length, and each is passed to the proxy's resolver
// with the sandbox's DNS credential, which the guest never sees, so the
// egress rules judge it as they judge the sandbox's connections.
func (d *Driver) serveDNS(ln net.Listener, socket, cred string) {
	sem := make(chan struct{}, maxEgressConns)
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
			defer func() { <-sem; _ = c.Close() }()
			for {
				_ = c.SetDeadline(time.Now().Add(time.Minute))
				q, err := readDNSFrame(c)
				if err != nil {
					return
				}
				a, err := d.lookup(socket, cred, q)
				if err != nil {
					slog.Debug("microvm: a lookup through the egress proxy failed", "err", err)
					return
				}
				if writeDNSFrame(c, a) != nil {
					return
				}
			}
		}()
	}
}

// lookup sends one query to the proxy's resolver as its socket expects:
// the credential, then the query, each length-prefixed.
func (d *Driver) lookup(socket, cred string, q []byte) ([]byte, error) {
	up, err := d.dialEgress(socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = up.Close() }()
	_ = up.SetDeadline(time.Now().Add(dnsTimeout))
	if err := writeDNSFrame(up, []byte(cred)); err != nil {
		return nil, err
	}
	if err := writeDNSFrame(up, q); err != nil {
		return nil, err
	}
	return readDNSFrame(up)
}

func readDNSFrame(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeDNSFrame(w io.Writer, b []byte) error {
	if len(b) > maxDNSMessage {
		return errors.New("a DNS message over 65535 bytes")
	}
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...)) // #nosec G115 -- checked
	return err
}
