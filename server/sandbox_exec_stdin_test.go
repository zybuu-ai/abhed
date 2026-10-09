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
	streamQuiet = 200 * time.Millisecond
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
