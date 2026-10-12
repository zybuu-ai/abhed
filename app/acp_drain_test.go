package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	abhed "github.com/zybuu-ai/abhed/sdk"
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

// blockingAgent runs until its context ends.
type blockingAgent struct{ started chan struct{} }

func (a *blockingAgent) Run(ctx context.Context, _ string) (string, error) {
	close(a.started)
	<-ctx.Done()
	return "", ctx.Err()
}
func (a *blockingAgent) Steer(string)                {}
func (a *blockingAgent) Flush(context.Context) error { return nil }
func (a *blockingAgent) CancelTasks() int            { return 0 }
func (a *blockingAgent) Close()                      {}

// A prompt still running when stdin closes is answered, as cancelled.
func TestACPPromptRunningAtEOFEndsCancelled(t *testing.T) {
	ag := &blockingAgent{started: make(chan struct{})}
	newACPAgent = func(context.Context, abhed.Options) (acpAgent, error) { return ag, nil }
	t.Cleanup(func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	})
	inR, inW := io.Pipe()
	out := &syncWriter{}
	c := &acpConn{out: out, version: "test", base: t.TempDir(), sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	done := make(chan error, 1)
	go func() { done <- c.run(inR) }()
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/ws"}}`)
	var sid string
	for end := time.Now().Add(10 * time.Second); sid == "" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if _, rest, ok := strings.Cut(out.String(), `"sessionId":"`); ok {
			sid, _, _ = strings.Cut(rest, `"`)
		}
	}
	if sid == "" {
		t.Fatalf("no session:\n%s", out.String())
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[{"type":"text","text":"go"}]}}`)
	select {
	case <-ag.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the prompt never started")
	}
	_ = inW.Close()
	select {
	case <-done:
	case <-time.After(drainWait + 5*time.Second):
		t.Fatal("acp did not exit")
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				StopReason string `json:"stopReason"`
			} `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && string(m.ID) == "3" {
			if m.Result.StopReason != "cancelled" {
				t.Fatalf("the prompt ended %q:\n%s", m.Result.StopReason, out.String())
			}
			return
		}
	}
	t.Fatalf("the prompt was not answered:\n%s", out.String())
}
