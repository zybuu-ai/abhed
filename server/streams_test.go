package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// muxClient reads a shared stream's frames as a page would.
type muxClient struct {
	t      *testing.T
	srv    *httptest.Server
	header http.Header
	id     string
	frames chan muxFrame
	resp   *http.Response
}

func openMux(t *testing.T, h http.Handler, header http.Header) *muxClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/streams", nil)
	req.Header = header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shared stream: %d", resp.StatusCode)
	}
	c := &muxClient{t: t, srv: srv, header: header, frames: make(chan muxFrame, 1024), resp: resp}
	named := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "mux":
				var m struct{ ID string }
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m)
				named <- m.ID
			case strings.HasPrefix(line, "data: ") && event == "c":
				var f muxFrame
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f)
				c.frames <- f
			}
		}
		close(c.frames)
	}()
	select {
	case c.id = <-named:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared stream was not named")
	}
	return c
}

// ask opens channel ch for path, answering the POST's status.
func (c *muxClient) ask(ch, path string, header http.Header) int {
	c.t.Helper()
	body, _ := json.Marshal(muxOpenRequest{Path: path})
	req, _ := http.NewRequest("POST", c.srv.URL+"/v1/streams/"+c.id+"/"+ch, strings.NewReader(string(body)))
	req.Header = header.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// until reads frames until done says so, or fails after a while.
func (c *muxClient) until(what string, done func(muxFrame) bool) {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	var last []muxFrame
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				c.t.Fatalf("the shared stream ended waiting for %s", what)
			}
			if done(f) {
				return
			}
			last = append(last, f)
		case <-deadline:
			c.t.Fatalf("no %s on the shared stream; frames: %+v", what, last)
		}
	}
}

// Eight shells and the session's events ride one connection, each as its
// own channel with its own output.
func TestSharedStreamCarriesManyShells(t *testing.T) {
	wb := shellBench(t, nil)
	h := http.Header{"X-Abhed-Tenant": {"acme"}}
	c := openMux(t, wb.h, h)
	if code := c.ask("ev", "/v1/sessions/"+wb.session+"/events", h); code != http.StatusAccepted {
		t.Fatalf("events channel: %d", code)
	}
	c.until("the events channel's open", func(f muxFrame) bool { return f.Ch == "ev" && f.Type == "open" })
	const shells = 8
	ids := map[string]string{}
	for i := range shells {
		start := wb.startShell()
		ch := fmt.Sprintf("p%d", i)
		ids[ch] = start.ID
		if code := c.ask(ch, "/v1/sessions/"+wb.session+"/pty/"+start.ID, h); code != http.StatusAccepted {
			t.Fatalf("shell %d: %d", i, code)
		}
	}
	for ch, id := range ids {
		req, _ := http.NewRequest("POST", c.srv.URL+"/v1/sessions/"+wb.session+"/pty/"+id+"/input", strings.NewReader("echo mark-$((40+2))-"+ch+"\r"))
		req.Header = h.Clone()
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	seen := map[string]bool{}
	got := map[string]string{}
	c.until("every shell's output", func(f muxFrame) bool {
		if f.Type == "ev" && f.Event == "out" {
			b, _ := base64.StdEncoding.DecodeString(f.Data)
			got[f.Ch] += string(b)
			if strings.Contains(plainText([]byte(got[f.Ch])), "mark-42-"+f.Ch) {
				seen[f.Ch] = true
			}
		}
		return len(seen) == shells
	})
}

// A channel is its route's own request: one for a session the caller does
// not own is refused with the route's status, and another account cannot
// put a channel on someone else's shared stream.
func TestSharedStreamChannelsAreAuthorisedEach(t *testing.T) {
	r := newPerSessionRig(t)
	id, _ := r.open(t, "alice")
	alice, bob := http.Header{"Authorization": {"Bearer alice"}}, http.Header{"Authorization": {"Bearer bob"}}
	c := openMux(t, r.h, bob)
	if code := c.ask("x", "/v1/sessions/"+id+"/events", bob); code != http.StatusAccepted {
		t.Fatalf("bob's ask: %d", code)
	}
	c.until("the refusal", func(f muxFrame) bool {
		if f.Ch == "x" && (f.Type == "open" || f.Type == "ev") {
			t.Fatalf("bob was given alice's events: %+v", f)
		}
		return f.Ch == "x" && f.Type == "fail" && f.Status == http.StatusNotFound
	})
	if code := c.ask("y", "/v1/sessions/"+id+"/events", alice); code != http.StatusNotFound {
		t.Fatalf("alice put a channel on bob's shared stream: %d", code)
	}
	for _, path := range []string{"/v1/sessions/" + id + "/file", "/v1/overview", "https://elsewhere.example/v1/sessions/" + id + "/events", "/v1/sessions/" + id + "/events/../file"} {
		if code := c.ask("z", path, bob); code != http.StatusBadRequest {
			t.Errorf("%s: %d", path, code)
		}
	}
	ca := openMux(t, r.h, alice)
	if code := ca.ask("e", "/v1/sessions/"+id+"/events", alice); code != http.StatusAccepted {
		t.Fatalf("alice's ask: %d", code)
	}
	ca.until("alice's events", func(f muxFrame) bool { return f.Ch == "e" && f.Type == "open" })
}

// An open channel's id is not reused: a second ask is refused, so neither the
// cap nor a later close loses track of it; once it has ended, the id is free.
func TestSharedStreamRefusesAnOpenChannelID(t *testing.T) {
	wb := shellBench(t, nil)
	h := http.Header{"X-Abhed-Tenant": {"acme"}}
	c := openMux(t, wb.h, h)
	path := "/v1/sessions/" + wb.session + "/events"
	if code := c.ask("ev", path, h); code != http.StatusAccepted {
		t.Fatalf("events channel: %d", code)
	}
	c.until("the channel's open", func(f muxFrame) bool { return f.Ch == "ev" && f.Type == "open" })
	if code := c.ask("ev", path, h); code != http.StatusConflict {
		t.Fatalf("an open channel's id was taken again: %d", code)
	}
	req, _ := http.NewRequest("DELETE", c.srv.URL+"/v1/streams/"+c.id+"/ev", nil)
	req.Header = h.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("close: %d", resp.StatusCode)
	}
	c.until("the channel's end", func(f muxFrame) bool { return f.Ch == "ev" && f.Type == "end" })
	code := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if code = c.ask("ev", path, h); code != http.StatusConflict {
			break
		}
	}
	if code != http.StatusAccepted {
		t.Fatalf("the ended channel's id is not free: %d", code)
	}
	c.until("the new channel's open", func(f muxFrame) bool { return f.Ch == "ev" && f.Type == "open" })
}
