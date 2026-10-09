package wsconn

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// Stream is a byte stream over a connection's binary messages, as a
// net.Conn. A text message goes to OnText; with none set, it closes the
// stream as unsupported data. The peer's normal close reads as io.EOF.
type Stream struct {
	c *Conn
	// OnText, when set, takes each text message; an error from it ends the stream.
	OnText func([]byte) error
	// Chunk bounds the bytes one write puts in a message.
	Chunk int

	mu  sync.Mutex
	buf []byte
	err error
}

// NewStream is a stream over c.
func NewStream(c *Conn) *Stream { return &Stream{c: c, Chunk: 32 << 10} }

// Conn is the connection the stream runs over.
func (s *Stream) Conn() *Conn { return s.c }

func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		typ, msg, err := s.c.ReadMessage()
		switch {
		case err != nil:
			if IsClose(err) {
				err = io.EOF
			}
			s.err = err
			continue
		case typ == Text:
			if s.OnText == nil {
				_ = s.c.CloseWith(CloseUnsupported, "this stream carries binary messages only")
				s.err = errors.New("websocket: a text message on a binary stream")
				continue
			}
			if err := s.OnText(msg); err != nil {
				s.err = err
			}
			continue
		}
		s.buf = msg
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *Stream) Write(p []byte) (int, error) {
	chunk := s.Chunk
	if chunk <= 0 {
		chunk = 32 << 10
	}
	written := 0
	for written < len(p) {
		end := min(written+chunk, len(p))
		if err := s.c.WriteMessage(Binary, p[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

// Close closes the connection normally.
func (s *Stream) Close() error { return s.c.Close() }

func (s *Stream) LocalAddr() net.Addr  { return s.c.LocalAddr() }
func (s *Stream) RemoteAddr() net.Addr { return s.c.RemoteAddr() }
func (s *Stream) SetDeadline(t time.Time) error {
	return errors.Join(s.c.SetReadDeadline(t), s.c.SetWriteDeadline(t))
}
func (s *Stream) SetReadDeadline(t time.Time) error  { return s.c.SetReadDeadline(t) }
func (s *Stream) SetWriteDeadline(t time.Time) error { return s.c.SetWriteDeadline(t) }

var _ net.Conn = (*Stream)(nil)
