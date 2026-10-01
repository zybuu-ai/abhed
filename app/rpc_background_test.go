package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// rpcConv drives rpcCmd one request at a time, reading what it writes.
type rpcConv struct {
	t     *testing.T
	in    *os.File
	lines chan string
}

func startRPCConv(t *testing.T, ws string) *rpcConv {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	c := &rpcConv{t: t, in: inW, lines: make(chan string, 1024)}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	done := make(chan struct{})
	go func() { defer close(done); rpcCmd(ws, ""); _ = outW.Close() }()
	t.Cleanup(func() {
		_ = inW.Close()
		<-done
		os.Stdin, os.Stdout = oldIn, oldOut
	})
	return c
}

// send writes a request and returns the line answering it, by id.
func (c *rpcConv) send(id, req string) map[string]any {
	c.t.Helper()
	fmt.Fprintln(c.in, req)
	return c.until(func(m map[string]any) bool { return m["id"] == id })
}

func (c *rpcConv) until(match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				c.t.Fatal("rpc closed")
			}
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil && match(m) {
				return m
			}
		case <-deadline:
			c.t.Fatal("no matching line from rpc")
		}
	}
}

// rpc lists, cancels and wakes for background tasks when started in notify.
func TestRPCTasksCancelWake(t *testing.T) {
	m := &bgModelServer{childDelay: time.Minute}
	ws := bgWorkspace(t, m.start(t), "")
	c := startRPCConv(t, ws)
	if r := c.send("1", `{"id":"1","method":"start","wake":"notify"}`); r["type"] != "ready" {
		t.Fatalf("start: %v", r)
	}
	if r := c.send("2", `{"id":"2","method":"prompt","prompt":"go"}`); r["type"] != "answer" {
		t.Fatalf("prompt: %v", r)
	}
	r := c.send("3", `{"id":"3","method":"tasks"}`)
	tasks, _ := r["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks: %v", r)
	}
	task := tasks[0].(map[string]any)
	if task["status"] != "running" {
		t.Fatalf("task: %v", task)
	}
	if r := c.send("4", `{"id":"4","method":"cancel_task","task_id":"`+task["task_id"].(string)+`"}`); r["type"] != "cancelled" {
		t.Fatalf("cancel: %v", r)
	}
	// The result arrives as an event line, then a wake runs the agent on it.
	c.until(func(m map[string]any) bool {
		ev, _ := m["event"].(map[string]any)
		return ev != nil && ev["type"] == "subagent.notice"
	})
	if r := c.send("5", `{"id":"5","method":"wake"}`); r["type"] != "answer" || !strings.Contains(fmt.Sprint(r["answer"]), "noted") {
		t.Fatalf("wake: %v", r)
	}
	if r := c.send("6", `{"id":"6","method":"wake"}`); r["type"] != "error" || !strings.Contains(fmt.Sprint(r["error"]), "nothing to wake") {
		t.Fatalf("a second wake: %v", r)
	}
}
