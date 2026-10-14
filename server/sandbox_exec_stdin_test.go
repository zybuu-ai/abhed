package server

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/wsconn"
)

// While the command is not reading its stdin, the stream still answers the
// client's pings; once it reads, it gets every byte, in order, none dropped.
func TestSandboxExecStdinDoesNotBlockPings(t *testing.T) {
	quiet := streamQuiet
	// Short, so the stream would end in the waits below were a full queue
	// taken for a quiet client; long enough for an emulated client to mask
	// and send a 1 MiB message between reads.
	streamQuiet = 500 * time.Millisecond
	t.Cleanup(func() { streamQuiet = quiet })
	r := newWSRig(t, nil, nil, func(o *Options) { o.Config.Permissions.Deny, o.Config.Permissions.Ask = nil, nil })
	sb := r.ready(t, "alice")
	reading := make(chan struct{})
	r.fake.ExecFunc = func(ctx context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		select {
		case <-reading:
		case <-ctx.Done():
			return driver.ExecResult{}, ctx.Err()
		}
		b, _ := io.ReadAll(req.Stdin)
		sum := 0
		for i, c := range b {
			if c != byte(i%251) {
				_, _ = io.WriteString(req.Stdout, "out of order at "+strconv.Itoa(i))
				return driver.ExecResult{ExitCode: 1}, nil
			}
			sum++
		}
		_, _ = io.WriteString(req.Stdout, strconv.Itoa(sum))
		return driver.ExecResult{}, nil
	}
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"command":"cat"}`))
	if c, _ := control(t, ws); c.Type != "started" {
		t.Fatalf("first %+v", c)
	}
	data := make([]byte, 3<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	msgs := make(chan []byte, 64)
	go func() {
		defer close(msgs)
		for {
			typ, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			msgs <- append([]byte{byte(typ)}, msg...)
		}
	}()
	// The command takes nothing yet: the stream must outlive the quiet bound,
	// first with stdin queued, then with the queue full.
	_ = ws.WriteMessage(wsconn.Binary, data[:64<<10])
	time.Sleep(3 * streamQuiet)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for off := 64 << 10; off < len(data); off += 1 << 20 {
			_ = ws.WriteMessage(wsconn.Binary, data[off:min(off+1<<20, len(data))])
		}
	}()
	time.Sleep(3 * streamQuiet)
	select {
	case m := <-msgs:
		t.Fatalf("the stream ended while the command was not reading: %s", m[1:])
	default:
	}
	close(reading)
	<-sent
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"type":"eof"}`))
	var out []byte
	for m := range msgs {
		if m[0] == byte(wsconn.Binary) {
			out = append(out, m[2:]...)
			continue
		}
		if !bytes.Contains(m, []byte(`"exit"`)) {
			t.Fatalf("end %s (output %q)", m[1:], out)
		}
		break
	}
	if string(out) != strconv.Itoa(len(data)) {
		t.Fatalf("the command read %q, want %d bytes", out, len(data))
	}
}

// A message the command is slow to take keeps the reader busy until all of
// it is queued, not just until one chunk fits, and the client's silence
// counts from when the reader is back.
func TestStdinQueueBusyForTheWholeMessage(t *testing.T) {
	pr, pw := io.Pipe()
	q := newStdinQueue(pw)
	t.Cleanup(func() { q.end(); _ = pr.Close() })
	msg := make([]byte, (stdinQueued+8)*stdinChunk)
	go q.put(context.Background(), msg)
	for !q.full() {
		time.Sleep(time.Millisecond)
	}
	before := time.Now()
	drained := make(chan int64)
	go func() {
		n, _ := io.CopyN(io.Discard, pr, int64(len(msg)))
		drained <- n
	}()
	// resumed is stored before full clears, so full is never false first.
	for seen := 0; q.since().IsZero(); seen++ {
		if !q.full() && q.since().IsZero() {
			t.Fatalf("not busy after %d looks while the message is still being queued", seen)
		}
	}
	if n := <-drained; n != int64(len(msg)) {
		t.Fatalf("drained %d of %d bytes", n, len(msg))
	}
	if q.full() || q.since().Before(before) {
		t.Fatalf("after the message: full %v, resumed %v (want after %v)", q.full(), q.since(), before)
	}
}

func TestClientQuietCountsFromResumed(t *testing.T) {
	now := time.Now()
	old, recent := now.Add(-time.Minute), now.Add(-time.Second)
	busy, idle := func() bool { return true }, func() bool { return false }
	at := func(t time.Time) func() time.Time { return func() time.Time { return t } }
	for _, c := range []struct {
		name string
		b    streamBounds
		last time.Time
		want bool
	}{
		{"quiet, no bounds", streamBounds{}, old, true},
		{"heard lately", streamBounds{}, recent, false},
		{"server busy", streamBounds{busy: busy}, old, false},
		{"server just back from busy", streamBounds{busy: idle, resumed: at(recent)}, old, false},
		{"quiet since the server came back", streamBounds{busy: idle, resumed: at(old)}, old, true},
		{"never busy", streamBounds{busy: idle, resumed: at(time.Time{})}, old, true},
	} {
		if got := c.b.clientQuiet(c.last, 10*time.Second, now); got != c.want {
			t.Errorf("%s: quiet %v, want %v", c.name, got, c.want)
		}
	}
}

// Stdin sent after {"type":"eof"} is dropped: the command gets what came
// before, and the server neither panics nor ends the stream for it.
func TestSandboxExecStdinAfterEOFIsDropped(t *testing.T) {
	r := newWSRig(t, nil, nil, func(o *Options) { o.Config.Permissions.Deny, o.Config.Permissions.Ask = nil, nil })
	sb := r.ready(t, "alice")
	r.fake.ExecFunc = func(_ context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		b, _ := io.ReadAll(req.Stdin)
		_, _ = req.Stdout.Write(b)
		return driver.ExecResult{}, nil
	}
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"command":"cat"}`))
	if c, _ := control(t, ws); c.Type != "started" {
		t.Fatalf("first %+v", c)
	}
	_ = ws.WriteMessage(wsconn.Binary, []byte("before"))
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"type":"eof"}`))
	_ = ws.WriteMessage(wsconn.Binary, []byte("after"))
	_ = ws.WriteMessage(wsconn.Binary, make([]byte, 1<<20))
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"type":"eof"}`))
	var out []byte
	for {
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("the stream ended without an exit: %v (output %q)", err, out)
		}
		if typ == wsconn.Binary {
			out = append(out, msg[1:]...)
			continue
		}
		if !bytes.Contains(msg, []byte(`"exit"`)) {
			t.Fatalf("end %s (output %q)", msg, out)
		}
		break
	}
	if string(out) != "before" {
		t.Fatalf("the command read %q, want only what came before eof", out)
	}
}

func TestStdinQueuePutAfterEnd(t *testing.T) {
	pr, pw := io.Pipe()
	q := newStdinQueue(pw)
	if !q.put(context.Background(), []byte("kept")) {
		t.Fatal("put before end refused")
	}
	q.end()
	q.end()
	// More than the queue holds, so an unsafe put would also block.
	if !q.put(context.Background(), make([]byte, (stdinQueued+2)*stdinChunk)) {
		t.Fatal("put after end reported the context ended")
	}
	b, err := io.ReadAll(pr)
	if err != nil || string(b) != "kept" {
		t.Fatalf("read %q, %v; want only what was put before end", b, err)
	}
}
