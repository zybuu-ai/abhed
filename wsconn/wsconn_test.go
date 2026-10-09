package wsconn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// pipeConn is a Conn reading data and writing to out, as role says.
func pipeConn(data []byte, client bool, l Limits) (*Conn, *syncBuffer) {
	a, b := net.Pipe()
	out := &syncBuffer{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := b.Read(buf)
			out.write(buf[:n])
			if err != nil {
				return
			}
		}
	}()
	c := newConn(a, bufio.NewReader(bytes.NewReader(data)), client, l, "")
	return c, out
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.b.Bytes())
}

func clientFrame(fin bool, op byte, p []byte) []byte {
	return appendFrame(nil, fin, op, p, &[4]byte{1, 2, 3, 4})
}

func serverFrame(op byte, p []byte) []byte { return appendFrame(nil, true, op, p, nil) }

func closeCode(t *testing.T, err error) int {
	t.Helper()
	var ce *CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("not a close: %v", err)
	}
	return ce.Code
}

func TestFrameRules(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 200)
	cases := []struct {
		name string
		data []byte
		code int
	}{
		{"unmasked from a client", serverFrame(opBinary, []byte("x")), CloseProtocolError},
		{"reserved bits", append([]byte{0xC2}, clientFrame(true, opBinary, []byte("x"))[1:]...), CloseProtocolError},
		{"unknown opcode", clientFrame(true, 0x3, nil), CloseProtocolError},
		{"fragmented ping", clientFrame(false, opPing, nil), CloseProtocolError},
		{"long ping", clientFrame(true, opPing, big), CloseProtocolError},
		{"continuation first", clientFrame(true, opContinuation, []byte("x")), CloseProtocolError},
		{"message inside a message", append(clientFrame(false, opText, []byte("a")), clientFrame(true, opText, []byte("b"))...), CloseProtocolError},
		{"invalid UTF-8", clientFrame(true, opText, []byte{0xff, 0xfe}), CloseInvalidData},
		{"one-byte close", clientFrame(true, opClose, []byte{3}), CloseProtocolError},
		{"forbidden close code", clientFrame(true, opClose, []byte{0x03, 0xED}), CloseProtocolError},
		{"frame over the limit", clientFrame(true, opBinary, bytes.Repeat([]byte("a"), 2000)), CloseTooBig},
		{"message over the limit", append(clientFrame(false, opBinary, big), append(clientFrame(false, opContinuation, big),
			clientFrame(true, opContinuation, big)...)...), CloseTooBig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws, _ := pipeConn(c.data, false, Limits{MaxFrame: 1024, MaxMessage: 500})
			_, _, err := ws.ReadMessage()
			if got := closeCode(t, err); got != c.code {
				t.Fatalf("code %d, want %d (%v)", got, c.code, err)
			}
		})
	}
	// A server's frames must not be masked.
	ws, _ := pipeConn(clientFrame(true, opBinary, []byte("x")), true, Limits{})
	if _, _, err := ws.ReadMessage(); closeCode(t, err) != CloseProtocolError {
		t.Fatalf("masked server frame: %v", err)
	}
	// A length not minimally encoded is refused.
	nonMinimal := []byte{0x82, 0x80 | 126, 0, 5, 1, 2, 3, 4, 'a', 'b', 'c', 'd', 'e'}
	ws, _ = pipeConn(nonMinimal, false, Limits{})
	if _, _, err := ws.ReadMessage(); closeCode(t, err) != CloseProtocolError {
		t.Fatalf("non-minimal length: %v", err)
	}
}

func TestFragmentsPingAndClose(t *testing.T) {
	var data []byte
	data = append(data, clientFrame(false, opText, []byte("hel"))...)
	data = append(data, clientFrame(true, opPing, []byte("p"))...)
	data = append(data, clientFrame(true, opContinuation, []byte("lo"))...)
	data = append(data, clientFrame(true, opClose, append([]byte{0x03, 0xE8}, "bye"...))...)
	ws, out := pipeConn(data, false, Limits{})
	typ, msg, err := ws.ReadMessage()
	if err != nil || typ != Text || string(msg) != "hello" {
		t.Fatalf("%v %q %v", typ, msg, err)
	}
	_, _, err = ws.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseNormal || ce.Reason != "bye" || ce.Local || !IsClose(err) {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	// The ping was answered with a pong carrying its payload, and the close echoed.
	got := out.Bytes()
	if !bytes.HasPrefix(got, serverFrame(opPong, []byte("p"))) || !bytes.Contains(got, serverFrame(opClose, []byte{0x03, 0xE8})) {
		t.Fatalf("answers %x", got)
	}
	if err := ws.WriteMessage(Binary, []byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

// FuzzReadMessage feeds arbitrary bytes to both sides' readers: nothing
// panics, nothing over the limits is returned, and text is UTF-8.
func FuzzReadMessage(f *testing.F) {
	f.Add(clientFrame(true, opText, []byte("hi")), false)
	f.Add(serverFrame(opBinary, []byte{0, 1, 2}), true)
	f.Add(append(clientFrame(false, opBinary, []byte("a")), clientFrame(true, opContinuation, []byte("b"))...), false)
	f.Add(clientFrame(true, opClose, []byte{0x03, 0xE8, 'x'}), false)
	f.Add([]byte{0x82, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, false)
	f.Fuzz(func(t *testing.T, data []byte, client bool) {
		l := Limits{MaxFrame: 256, MaxMessage: 512}
		ws, _ := pipeConn(data, client, l)
		for range 64 {
			typ, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if int64(len(msg)) > l.MaxMessage {
				t.Fatalf("a message of %d bytes", len(msg))
			}
			if typ == Text && !utf8.Valid(msg) {
				t.Fatal("text that is not UTF-8")
			}
			if typ != Text && typ != Binary {
				t.Fatalf("type %d", typ)
			}
		}
	})
}

// FuzzReadFrame checks the frame parser alone against what it accepts.
func FuzzReadFrame(f *testing.F) {
	f.Add(clientFrame(true, opPing, []byte("x")), true)
	f.Add(serverFrame(opBinary, bytes.Repeat([]byte("z"), 300)), false)
	f.Fuzz(func(t *testing.T, data []byte, masked bool) {
		fr, err := readFrame(bytes.NewReader(data), masked, 1024)
		if err != nil {
			return
		}
		if len(fr.payload) > 1024 || fr.op >= opClose && (!fr.fin || len(fr.payload) > maxControl) {
			t.Fatalf("accepted %+v", fr)
		}
		// What was accepted encodes back to the same bytes, unmasked.
		var key *[4]byte
		if masked {
			off := 2
			switch data[1] & 0x7F {
			case 126:
				off += 2
			case 127:
				off += 8
			}
			key = (*[4]byte)(data[off : off+4])
		}
		consumed := appendFrame(nil, fr.fin, fr.op, fr.payload, key)
		if !bytes.HasPrefix(data, consumed) {
			t.Fatalf("round trip %x vs %x", consumed, data)
		}
	})
}

func echoServer(t *testing.T, o UpgradeOptions) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r, o)
		var he *HandshakeError
		if errors.As(err, &he) {
			http.Error(w, he.Msg, he.Status)
			return
		}
		if err != nil {
			return
		}
		for {
			typ, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if err := ws.WriteMessage(typ, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDialRoundTripAndClose(t *testing.T) {
	srv := echoServer(t, UpgradeOptions{Protocols: []string{"abhed.test.v1"}})
	ctx := context.Background()
	ws, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), DialOptions{Protocols: []string{"other", "abhed.test.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Subprotocol != "abhed.test.v1" {
		t.Fatalf("subprotocol %q", ws.Subprotocol)
	}
	big := bytes.Repeat([]byte("0123456789"), 10_000)
	for _, m := range [][]byte{[]byte("hi"), big, {}} {
		if err := ws.WriteMessage(Binary, m); err != nil {
			t.Fatal(err)
		}
		typ, got, err := ws.ReadMessage()
		if err != nil || typ != Binary || !bytes.Equal(got, m) {
			t.Fatalf("echo of %d bytes: %d bytes, %v", len(m), len(got), err)
		}
	}
	if err := ws.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(Text, []byte{0xff}); err == nil {
		t.Fatal("wrote invalid UTF-8 as text")
	}
	done := make(chan error, 1)
	go func() { _, _, err := ws.ReadMessage(); done <- err }()
	if err := ws.CloseWith(CloseNormal, "done"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !IsClose(err) {
			t.Fatalf("close handshake: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the close handshake did not finish")
	}
}

func TestUpgradeRefusals(t *testing.T) {
	srv := echoServer(t, UpgradeOptions{Protocols: []string{"p1"}})
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx := context.Background()
	// No subprotocol offered.
	_, err := Dial(ctx, url, DialOptions{})
	var he *HandshakeError
	if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
		t.Fatalf("no protocol: %v", err)
	}
	// Another site's page.
	_, err = Dial(ctx, url, DialOptions{Protocols: []string{"p1"}, Header: http.Header{"Origin": {"https://evil.example"}}})
	if !errors.As(err, &he) || he.Status != http.StatusForbidden {
		t.Fatalf("cross origin: %v", err)
	}
	// The server's own origin is accepted.
	ws, err := Dial(ctx, url, DialOptions{Protocols: []string{"p1"}, Header: http.Header{"Origin": {srv.URL}}})
	if err != nil {
		t.Fatalf("same origin: %v", err)
	}
	_ = ws.Close()
	// A plain GET, and a bad key.
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "short")
	req.Header.Set("Sec-WebSocket-Protocol", "p1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}
}

func TestSameHost(t *testing.T) {
	for origin, want := range map[string]bool{
		"https://a.example:8443": true, "http://a.example:8443": true, "https://a.example": false,
		"https://evil.example:8443": false, "null": false, "file://a.example:8443": false, "https://a.example:8443/x": false,
	} {
		if got := SameHost(origin, "a.example:8443"); got != want {
			t.Errorf("%s: %v", origin, got)
		}
	}
}

func TestStream(t *testing.T) {
	srv := echoServer(t, UpgradeOptions{})
	ws, err := Dial(context.Background(), srv.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := NewStream(ws)
	s.Chunk = 3
	if _, err := s.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 8)
	if _, err := io.ReadFull(s, got); err != nil || string(got) != "abcdefgh" {
		t.Fatalf("%q %v", got, err)
	}
	// A text message on a binary stream ends it.
	if err := ws.WriteMessage(Text, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(got); err == nil {
		t.Fatal("a text message was read as bytes")
	}
}

func TestCloseCodeRange(t *testing.T) {
	for code, ok := range map[int]bool{999: false, 1000: true, 1004: false, 1005: false, 1006: false, 1011: true, 1015: false, 2999: false, 3000: true, 4999: true, 5000: false} {
		if validCloseCode(code) != ok {
			t.Errorf("%d", code)
		}
	}
	p := binary.BigEndian.AppendUint16(nil, 1000)
	if c, r, err := checkClose(append(p, "ok"...)); c != 1000 || r != "ok" || err != nil {
		t.Fatal(c, r, err)
	}
	if trimUTF8("aé", 2) != "a" {
		t.Fatal("trim split a character")
	}
}

// A peer that stops reading holds a write only until WriteTimeout; then the
// connection is closed, so other writers and a reader are not held behind it.
func TestWriteTimeoutAborts(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newConn(a, nil, false, Limits{WriteTimeout: 50 * time.Millisecond}, "")
	start := time.Now()
	if err := c.WriteMessage(Binary, []byte("nobody reads this")); err == nil {
		t.Fatal("a write no one read succeeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the write took %s", time.Since(start))
	}
	if err := c.WriteMessage(Binary, []byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("after the timeout: %v", err)
	}
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("a read on an aborted connection")
	}
}
