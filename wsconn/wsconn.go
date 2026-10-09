// Package wsconn is a small WebSocket (RFC 6455) implementation for Abhed's
// streams: the server side of an upgrade, a client, and the frames between
// them. It does only what those streams need: text and binary messages,
// fragmentation, ping, pong and the close handshake, with no extensions.
//
// Everything a peer sends is untrusted. Frames are checked against the
// masking rule for their direction, reserved bits, opcodes, control-frame
// limits and minimal length encodings; frames and messages are bounded in
// size, text is checked to be UTF-8, and any protocol error closes the
// connection with the code RFC 6455 names for it.
//
// It is an experimental API until 2.0, and may change in a minor release.
package wsconn

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// MessageType is a data message's kind.
type MessageType int

const (
	Text   MessageType = 1
	Binary MessageType = 2
)

// Opcodes.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// Close codes (RFC 6455 §7.4.1).
const (
	CloseNormal        = 1000
	CloseGoingAway     = 1001
	CloseProtocolError = 1002
	CloseUnsupported   = 1003
	CloseNoStatus      = 1005
	CloseInvalidData   = 1007
	ClosePolicy        = 1008
	CloseTooBig        = 1009
	CloseInternal      = 1011
)

// Defaults for Limits left at zero.
const (
	DefaultMaxFrame   = 1 << 20
	DefaultMaxMessage = 1 << 20
	// maxControl is the largest control-frame payload RFC 6455 allows.
	maxControl = 125
	// closeWait bounds how long a close waits for the peer's close.
	closeWait = 2 * time.Second
)

// Limits bound what a peer may send.
type Limits struct {
	// MaxFrame is the largest frame payload accepted.
	MaxFrame int64
	// MaxMessage is the largest message, all its fragments together.
	MaxMessage int64
	// WriteTimeout, when set, bounds each frame's write: a peer that stops
	// reading has its connection closed at once, rather than holding every
	// writer (and the reader's answer to a ping) behind the write lock.
	WriteTimeout time.Duration
}

func (l Limits) orDefault() Limits {
	if l.MaxFrame <= 0 {
		l.MaxFrame = DefaultMaxFrame
	}
	if l.MaxMessage <= 0 {
		l.MaxMessage = DefaultMaxMessage
	}
	return l
}

// CloseError is the peer's close: its code and reason, or the protocol
// error this side closed for.
type CloseError struct {
	Code   int
	Reason string
	// Local is set when this side closed the connection for the error.
	Local bool
}

func (e *CloseError) Error() string {
	who := "the peer"
	if e.Local {
		who = "this side"
	}
	if e.Reason == "" {
		return fmt.Sprintf("websocket closed by %s (%d)", who, e.Code)
	}
	return fmt.Sprintf("websocket closed by %s (%d): %s", who, e.Code, e.Reason)
}

// IsClose reports whether err is a close of the connection, with code
// CloseNormal or CloseGoingAway when codes is empty.
func IsClose(err error, codes ...int) bool {
	var ce *CloseError
	if !errors.As(err, &ce) {
		return false
	}
	if len(codes) == 0 {
		codes = []int{CloseNormal, CloseGoingAway, CloseNoStatus}
	}
	for _, c := range codes {
		if ce.Code == c {
			return true
		}
	}
	return false
}

// ErrClosed is a write after the connection was closed.
var ErrClosed = errors.New("websocket: the connection is closed")

// frame is one frame as read off the wire, unmasked.
type frame struct {
	fin     bool
	op      byte
	payload []byte
}

// protocolError is a frame this side refuses, with the close code to send.
type protocolError struct {
	code int
	msg  string
}

func (e *protocolError) Error() string { return "websocket: " + e.msg }

func perr(code int, format string, args ...any) error {
	return &protocolError{code: code, msg: fmt.Sprintf(format, args...)}
}

// readFrame reads one frame from r. masked is whether frames in this
// direction must be masked (client to server) or must not be.
func readFrame(r io.Reader, masked bool, maxFrame int64) (frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return frame{}, err
	}
	f := frame{fin: h[0]&0x80 != 0, op: h[0] & 0x0F}
	if h[0]&0x70 != 0 {
		return frame{}, perr(CloseProtocolError, "reserved bits are set, and no extension was agreed")
	}
	switch f.op {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
	default:
		return frame{}, perr(CloseProtocolError, "unknown opcode %#x", f.op)
	}
	if (h[1]&0x80 != 0) != masked {
		if masked {
			return frame{}, perr(CloseProtocolError, "a client frame was not masked")
		}
		return frame{}, perr(CloseProtocolError, "a server frame was masked")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, unexpected(err)
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
		if n < 126 {
			return frame{}, perr(CloseProtocolError, "a frame length was not minimally encoded")
		}
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, unexpected(err)
		}
		n = binary.BigEndian.Uint64(b[:])
		if n>>63 != 0 {
			return frame{}, perr(CloseProtocolError, "a frame length has its top bit set")
		}
		if n <= 0xFFFF {
			return frame{}, perr(CloseProtocolError, "a frame length was not minimally encoded")
		}
	}
	if f.op >= opClose {
		if !f.fin {
			return frame{}, perr(CloseProtocolError, "a control frame was fragmented")
		}
		if n > maxControl {
			return frame{}, perr(CloseProtocolError, "a control frame is longer than %d bytes", maxControl)
		}
	}
	if n > uint64(maxFrame) {
		return frame{}, perr(CloseTooBig, "a frame is larger than %d bytes", maxFrame)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return frame{}, unexpected(err)
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame{}, unexpected(err)
	}
	if masked {
		for i := range f.payload {
			f.payload[i] ^= mask[i&3]
		}
	}
	return f, nil
}

// unexpected turns an EOF inside a frame into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// appendFrame appends one frame with payload p to b, masked with key when
// key is not nil.
func appendFrame(b []byte, fin bool, op byte, p []byte, key *[4]byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	b = append(b, b0)
	m := byte(0)
	if key != nil {
		m = 0x80
	}
	n := len(p)
	switch {
	case n < 126:
		b = append(b, m|byte(n))
	case n <= 0xFFFF:
		b = append(b, m|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n)) // #nosec G115 -- n <= 0xFFFF here
	default:
		b = append(b, m|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n)) // #nosec G115 -- a length is never negative
	}
	if key == nil {
		return append(b, p...)
	}
	b = append(b, key[:]...)
	start := len(b)
	b = append(b, p...)
	for i := range p {
		b[start+i] ^= key[i&3]
	}
	return b
}

// checkClose validates a close frame's payload and returns its code and reason.
func checkClose(p []byte) (int, string, error) {
	switch {
	case len(p) == 0:
		return CloseNoStatus, "", nil
	case len(p) == 1:
		return 0, "", perr(CloseProtocolError, "a close frame carried one byte")
	}
	code := int(binary.BigEndian.Uint16(p))
	if !validCloseCode(code) {
		return 0, "", perr(CloseProtocolError, "close code %d may not be sent", code)
	}
	if !utf8.Valid(p[2:]) {
		return 0, "", perr(CloseInvalidData, "a close reason is not UTF-8")
	}
	return code, string(p[2:]), nil
}

// validCloseCode reports a code a peer may send in a close frame.
func validCloseCode(c int) bool {
	switch {
	case c >= 1000 && c <= 1003, c >= 1007 && c <= 1011:
		return true
	case c >= 3000 && c <= 4999:
		return true
	}
	return false
}

// Conn is one WebSocket connection. One goroutine may read while others
// write; writes are serialised.
type Conn struct {
	conn   net.Conn
	r      *bufio.Reader
	client bool
	limits Limits
	// Subprotocol is the protocol the two sides agreed, "" for none.
	Subprotocol string

	rmu sync.Mutex

	wmu       sync.Mutex
	sentClose bool
	closed    atomic.Bool
	gotClose  atomic.Bool
	lastRead  atomic.Int64
	// peerClose is the peer's close once it arrived.
	peerClose atomic.Pointer[CloseError]

	closeOnce sync.Once
}

func newConn(c net.Conn, r *bufio.Reader, client bool, l Limits, proto string) *Conn {
	if r == nil {
		r = bufio.NewReaderSize(c, 32<<10)
	}
	ws := &Conn{conn: c, r: r, client: client, limits: l.orDefault(), Subprotocol: proto}
	ws.lastRead.Store(time.Now().UnixNano())
	return ws
}

// LastRead is when a frame last arrived, of any kind.
func (c *Conn) LastRead() time.Time { return time.Unix(0, c.lastRead.Load()) }

// LocalAddr and RemoteAddr are the underlying connection's.
func (c *Conn) LocalAddr() net.Addr  { return c.conn.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetReadDeadline and SetWriteDeadline set the underlying connection's.
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// ReadMessage returns the next data message, answering pings on the way.
// A close from the peer is answered and returned as a *CloseError; a
// protocol error closes the connection with its code and is returned.
func (c *Conn) ReadMessage() (MessageType, []byte, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.closed.Load() {
		return 0, nil, c.ended()
	}
	var typ MessageType
	var msg []byte
	started := false
	for {
		f, err := readFrame(c.r, !c.client, c.limits.MaxFrame)
		if err != nil {
			return 0, nil, c.fail(err)
		}
		c.lastRead.Store(time.Now().UnixNano())
		switch f.op {
		case opPing:
			if err := c.write(opPong, f.payload); err != nil && !errors.Is(err, ErrClosed) {
				return 0, nil, c.fail(err)
			}
			continue
		case opPong:
			continue
		case opClose:
			code, reason, err := checkClose(f.payload)
			if err != nil {
				return 0, nil, c.fail(err)
			}
			c.gotClose.Store(true)
			ce := &CloseError{Code: code, Reason: reason}
			c.peerClose.Store(ce)
			// Echo the code, as RFC 6455 asks, unless this side closed first.
			echo := code
			if echo == CloseNoStatus {
				echo = CloseNormal
			}
			_ = c.sendClose(echo, "")
			c.finish()
			return 0, nil, ce
		case opText, opBinary:
			if started {
				return 0, nil, c.fail(perr(CloseProtocolError, "a message began inside another"))
			}
			started, typ = true, MessageType(f.op)
		case opContinuation:
			if !started {
				return 0, nil, c.fail(perr(CloseProtocolError, "a continuation frame began no message"))
			}
		}
		if int64(len(msg))+int64(len(f.payload)) > c.limits.MaxMessage {
			return 0, nil, c.fail(perr(CloseTooBig, "a message is larger than %d bytes", c.limits.MaxMessage))
		}
		msg = append(msg, f.payload...)
		if !f.fin {
			continue
		}
		if typ == Text && !utf8.Valid(msg) {
			return 0, nil, c.fail(perr(CloseInvalidData, "a text message is not UTF-8"))
		}
		if msg == nil {
			msg = []byte{}
		}
		return typ, msg, nil
	}
}

// fail closes the connection for err: with err's code for a protocol
// error, and at once for a broken connection.
func (c *Conn) fail(err error) error {
	var pe *protocolError
	if errors.As(err, &pe) {
		_ = c.sendClose(pe.code, pe.msg)
		c.finish()
		return &CloseError{Code: pe.code, Reason: pe.msg, Local: true}
	}
	if c.closed.Load() {
		return c.ended()
	}
	c.finish()
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ended is what a read returns once the connection is closed.
func (c *Conn) ended() error {
	if pc := c.peerClose.Load(); pc != nil {
		return pc
	}
	return ErrClosed
}

// WriteMessage sends one unfragmented message.
func (c *Conn) WriteMessage(t MessageType, p []byte) error {
	if t != Text && t != Binary {
		return fmt.Errorf("websocket: message type %d", t)
	}
	if t == Text && !utf8.Valid(p) {
		return errors.New("websocket: a text message must be UTF-8")
	}
	return c.write(byte(t), p)
}

// Ping sends a ping; the peer's pong only refreshes LastRead.
func (c *Conn) Ping() error { return c.write(opPing, nil) }

func (c *Conn) write(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.sentClose || c.closed.Load() {
		return ErrClosed
	}
	return c.writeLocked(op, p, c.limits.WriteTimeout)
}

// writeLocked writes one frame within wait, when wait is set.
func (c *Conn) writeLocked(op byte, p []byte, wait time.Duration) error {
	var key *[4]byte
	if c.client {
		key = new([4]byte)
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
	}
	buf := appendFrame(make([]byte, 0, len(p)+14), true, op, p, key)
	if wait > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(wait))
		defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	}
	_, err := c.conn.Write(buf)
	if ne := net.Error(nil); errors.As(err, &ne) && ne.Timeout() {
		// The peer stopped reading, and part of a frame may have gone.
		c.finish()
	}
	return err
}

// sendClose sends a close frame once.
func (c *Conn) sendClose(code int, reason string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.sentClose || c.closed.Load() {
		return nil
	}
	c.sentClose = true
	if len(reason) > maxControl-2 {
		reason = trimUTF8(reason, maxControl-2)
	}
	p := binary.BigEndian.AppendUint16(nil, uint16(code)) // #nosec G115 -- close codes fit in 16 bits
	return c.writeLocked(opClose, append(p, reason...), closeWait)
}

// trimUTF8 cuts s to at most n bytes without splitting a character.
func trimUTF8(s string, n int) string {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	if n < len(s) {
		return s[:n]
	}
	return s
}

// finish closes the underlying connection once.
func (c *Conn) finish() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		_ = c.conn.Close()
	})
}

// CloseWith starts the close handshake with code and reason and closes
// the connection once the peer answers, or after a short wait. A reader
// still running sees the peer's close as a *CloseError.
func (c *Conn) CloseWith(code int, reason string) error {
	if c.closed.Load() {
		return nil
	}
	if err := c.sendClose(code, reason); err != nil {
		c.finish()
		return err
	}
	if c.gotClose.Load() {
		c.finish()
		return nil
	}
	// Wait for the peer's close unless a reader is already waiting for it.
	if c.rmu.TryLock() {
		go func() {
			defer c.rmu.Unlock()
			_ = c.conn.SetReadDeadline(time.Now().Add(closeWait))
			for {
				f, err := readFrame(c.r, !c.client, c.limits.MaxFrame)
				if err != nil {
					break
				}
				if f.op == opClose {
					if code, reason, err := checkClose(f.payload); err == nil {
						c.gotClose.Store(true)
						c.peerClose.Store(&CloseError{Code: code, Reason: reason})
					}
					break
				}
			}
			c.finish()
		}()
		return nil
	}
	time.AfterFunc(closeWait, c.finish)
	return nil
}

// Close closes the connection normally.
func (c *Conn) Close() error { return c.CloseWith(CloseNormal, "") }

// Abort closes the underlying connection at once, without a handshake.
func (c *Conn) Abort() { c.finish() }
