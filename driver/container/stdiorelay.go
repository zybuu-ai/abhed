package container

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

// Where the engine runs in a VM or on another host, no host socket can be
// bound into a container. The relay inside then carries each connection to
// the egress proxy over its own stdio, through a long-lived `engine exec`:
// connections are multiplexed as frames of type, stream id and length.

// stdioRelayArg makes this binary, bound into the container, the stdio relay.
const stdioRelayArg = "__abhed_egress_relay_stdio"

func init() {
	if len(os.Args) == 3 && os.Args[1] == stdioRelayArg {
		os.Exit(runStdioRelay(os.Args[2]))
	}
}

// runStdioRelay listens on addr inside the container and serves each
// connection over stdin and stdout until stdin ends.
func runStdioRelay(addr string) int {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed egress relay: listening on %s: %v\n", addr, err)
		return 126
	}
	err = serveMux(ln, os.Stdin, os.Stdout)
	_ = ln.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "abhed egress relay: %v\n", err)
		return 1
	}
	return 0
}

const (
	frameOpen  byte = 1 // a connection was accepted inside
	frameData  byte = 2
	frameEOF   byte = 3 // the sender will write no more on the stream
	frameClose byte = 4 // the stream is gone
	frameReady byte = 5 // the relay listens inside
)

// maxFrame bounds one frame's payload; a larger length ends the relay.
const maxFrame = 64 << 10

// streamQueue bounds what one stream may have waiting to be written; a
// stream whose reader falls that far behind is closed, not let stall the rest.
const streamQueue = 256

// mux is one end of the multiplexed stdio.
type mux struct {
	wmu sync.Mutex
	w   *bufio.Writer

	mu      sync.Mutex
	streams map[uint32]*stream
}

type stream struct {
	conn net.Conn
	in   chan []byte // nil payload: end of data
	// sentEOF and gotEOF are each direction's end; with both the stream goes.
	sentEOF, gotEOF bool
}

func newMux(w io.Writer) *mux {
	return &mux{w: bufio.NewWriter(w), streams: map[uint32]*stream{}}
}

func (m *mux) send(typ byte, id uint32, p []byte) error {
	var h [9]byte
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:5], id)
	binary.BigEndian.PutUint32(h[5:9], uint32(len(p))) // #nosec G115 -- at most maxFrame
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if _, err := m.w.Write(h[:]); err != nil {
		return err
	}
	if _, err := m.w.Write(p); err != nil {
		return err
	}
	return m.w.Flush()
}

// add starts the stream id over conn: what conn sends goes out as frames,
// and what arrives for it is written to conn in order. first, when set, is
// sent before anything conn sends; a failure drops the stream.
func (m *mux) add(id uint32, conn net.Conn, first func() error) {
	s := &stream{conn: conn, in: make(chan []byte, streamQueue)}
	m.mu.Lock()
	m.streams[id] = s
	m.mu.Unlock()
	if first != nil && first() != nil {
		m.drop(id, false)
		return
	}
	go func() {
		for p := range s.in {
			if p == nil {
				closeWrite(conn)
				m.half(id, false)
				continue
			}
			if _, err := conn.Write(p); err != nil {
				m.drop(id, true)
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if m.send(frameData, id, buf[:n]) != nil {
					m.drop(id, false)
					return
				}
			}
			if err != nil {
				if m.live(id) {
					_ = m.send(frameEOF, id, nil)
					m.half(id, true)
				}
				return
			}
		}
	}()
}

// live reports whether stream id is still open.
func (m *mux) live(id uint32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id] != nil
}

// half notes one direction of stream id ended: sent when this end sent its
// EOF, else when the other end's was written out. With both, it goes.
func (m *mux) half(id uint32, sent bool) {
	m.mu.Lock()
	s := m.streams[id]
	if s != nil {
		if sent {
			s.sentEOF = true
		} else {
			s.gotEOF = true
		}
	}
	both := s != nil && s.sentEOF && s.gotEOF
	m.mu.Unlock()
	if both {
		m.drop(id, false)
	}
}

// closeWrite ends c's sending side where it has one, so the far end reads EOF.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// drop closes stream id, telling the other end when tell is set.
func (m *mux) drop(id uint32, tell bool) {
	m.mu.Lock()
	s := m.streams[id]
	delete(m.streams, id)
	if s != nil {
		close(s.in)
	}
	m.mu.Unlock()
	if s == nil {
		return
	}
	_ = s.conn.Close()
	if tell {
		_ = m.send(frameClose, id, nil)
	}
}

func (m *mux) closeAll() {
	m.mu.Lock()
	ids := make([]uint32, 0, len(m.streams))
	for id := range m.streams {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.drop(id, false)
	}
}

// deliver hands p to stream id; a stream too far behind is closed.
func (m *mux) deliver(id uint32, p []byte) {
	m.mu.Lock()
	s := m.streams[id]
	full := false
	if s != nil {
		select {
		case s.in <- p:
		default:
			full = true
		}
	}
	m.mu.Unlock()
	if full {
		m.drop(id, true)
	}
}

// read runs the frames from r until it ends; open handles a new stream.
func (m *mux) read(r io.Reader, open func(id uint32)) error {
	return m.readReady(r, open, nil)
}

// readReady is read, calling ready when the other end says it listens.
func (m *mux) readReady(r io.Reader, open func(id uint32), ready func()) error {
	br := bufio.NewReader(r)
	var h [9]byte
	for {
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return err
		}
		typ, id, n := h[0], binary.BigEndian.Uint32(h[1:5]), binary.BigEndian.Uint32(h[5:9])
		if n > maxFrame {
			return fmt.Errorf("a frame of %d bytes is over the limit", n)
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(br, p); err != nil {
			return err
		}
		switch typ {
		case frameOpen:
			if open == nil {
				return errors.New("an open frame from the host")
			}
			open(id)
		case frameData:
			if len(p) > 0 {
				m.deliver(id, p)
			}
		case frameEOF:
			m.deliver(id, nil)
		case frameClose:
			m.drop(id, false)
		case frameReady:
			if ready != nil {
				ready()
			}
		default:
			return fmt.Errorf("unknown frame type %d", typ)
		}
	}
}

// serveMux is the inside end: each connection accepted on ln becomes a
// stream, until r ends.
func serveMux(ln net.Listener, r io.Reader, w io.Writer) error {
	m := newMux(w)
	defer m.closeAll()
	if err := m.send(frameReady, 0, nil); err != nil {
		return err
	}
	go func() {
		var next uint32
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			next++
			id := next
			m.add(id, c, func() error { return m.send(frameOpen, id, nil) })
		}
	}()
	err := m.read(r, nil)
	_ = ln.Close()
	return err
}

// dialMux is the host end: each stream the inside opens is joined to a new
// connection from dial, until r ends.
func dialMux(r io.Reader, w io.Writer, dial func() (net.Conn, error)) error {
	return dialMuxReady(r, w, dial, nil)
}

// dialMuxReady is dialMux, calling ready once the inside listens.
func dialMuxReady(r io.Reader, w io.Writer, dial func() (net.Conn, error), ready func()) error {
	m := newMux(w)
	defer m.closeAll()
	return m.readReady(r, func(id uint32) {
		c, err := dial()
		if err != nil {
			_ = m.send(frameClose, id, nil)
			return
		}
		m.add(id, c, nil)
	}, ready)
}
