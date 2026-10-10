package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncWriter is a buffer the replying goroutines share.
type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// A burst of requests followed at once by the end of stdin is answered in
// full: a slow request, one waiting on an agent→client request no one can
// answer now, and the quick ones. acp used to exit with replies unsent.
func TestACPAnswersEveryRequestReadBeforeEOF(t *testing.T) {
	release := make(chan struct{})
	handle(map[string]func(*acpConn, rpcMessage){
		"test/slow": func(c *acpConn, m rpcMessage) {
			<-release
			c.reply(m.ID, map[string]any{"slow": true}, nil)
		},
		"test/ask": func(c *acpConn, m rpcMessage) {
			if _, err := c.call(c.root(), "test/question", nil); err != nil {
				c.reply(m.ID, nil, refusal(errParams, "%v", err))
				return
			}
			c.reply(m.ID, map[string]any{}, nil)
		},
	})
	t.Cleanup(func() {
		delete(acpMethods, "test/slow")
		delete(acpMethods, "test/ask")
	})

	var in strings.Builder
	want := map[string]bool{}
	add := func(id int, method string) {
		fmt.Fprintf(&in, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":{"protocolVersion":1}}`+"\n", id, method)
		want[fmt.Sprint(id)] = true
	}
	add(1, "test/slow")
	add(2, "test/ask")
	for i := 3; i < 40; i++ {
		add(i, "initialize")
	}
	add(40, "no/such/method")

	out := &syncWriter{}
	c := &acpConn{out: out, version: "test", base: t.TempDir(), sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	// The slow request is still running when stdin has ended.
	time.AfterFunc(100*time.Millisecond, func() { close(release) })
	if err := c.run(strings.NewReader(in.String())); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var m rpcMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Method != "" {
			continue
		}
		delete(want, string(m.ID))
	}
	if len(want) > 0 {
		t.Fatalf("no reply to ids %v:\n%s", want, out.String())
	}
}
