package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A browser opens at most six connections to one server over HTTP/1.1, and a
// workbench tab holds one stream for the session's events, one per shell and
// one per language server: a few shells starved every other request the page
// made. The page instead opens one stream, GET /v1/streams, and asks for each
// of its streams on it. Each is the route it always was, run as its own
// request with the asking request's credentials and origin, so each is
// authorised, checked and rechecked as before; only its events travel on the
// shared stream.

const (
	// maxStreamMuxes bounds the shared streams one account holds open.
	maxStreamMuxes = 16
	// maxStreamChannels bounds the streams one shared stream carries.
	maxStreamChannels = 64
	// muxFrameQueue is how many frames wait for the shared stream's writer
	// before a channel's writes wait in turn.
	muxFrameQueue = 256
	// muxKeepalive is how often an idle shared stream sends a comment.
	muxKeepalive = 20 * time.Second
	// maxStreamFail is how much of a refused stream's body is passed on.
	maxStreamFail = 4 << 10
)

// muxPaths are the routes a shared stream carries: the session's events,
// a shell's output and a language server's messages.
var muxPaths = regexp.MustCompile(`^/v1/sessions/[A-Za-z0-9_-]{1,128}/(events|pty/[A-Za-z0-9_-]{1,128}|lsp/[A-Za-z0-9_-]{1,128})$`)

// muxChannelID is a channel's name as the page gives it.
var muxChannelID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// streamMuxes are the open shared streams, by id.
type streamMuxes struct {
	mu sync.Mutex
	m  map[string]*streamMux
}

// streamMux is one page's shared stream.
type streamMux struct {
	owner, tenant string
	ctx           context.Context
	frames        chan []byte

	mu    sync.Mutex
	chans map[string]context.CancelFunc
}

// muxFrame is one message on the shared stream: a channel opened, an event
// on it, its reconnect delay, its end, or its refusal with the status.
type muxFrame struct {
	Ch      string `json:"ch"`
	Type    string `json:"t"`
	Event   string `json:"event,omitempty"`
	Data    string `json:"data,omitempty"`
	ID      string `json:"id,omitempty"`
	HasID   bool   `json:"has_id,omitempty"`
	Retry   int    `json:"retry,omitempty"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// send queues f for the shared stream, waiting while the queue is full; false
// once the shared stream has ended.
func (m *streamMux) send(f muxFrame) bool {
	b, err := json.Marshal(f)
	if err != nil {
		return false
	}
	select {
	case m.frames <- b:
		return true
	case <-m.ctx.Done():
		return false
	}
}

// openStreamMux is GET /v1/streams: the shared stream itself. Its first event
// names it; each later one is a frame of a channel the page asked for.
func (s *Server) openStreamMux(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	owner, tenant := UserOf(r.Context()), TenantOf(r.Context())
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	m := &streamMux{owner: owner, tenant: tenant, ctx: ctx, frames: make(chan []byte, muxFrameQueue), chans: map[string]context.CancelFunc{}}
	id := strings.TrimPrefix(newSessionID(), "s-")
	s.muxes.mu.Lock()
	held := 0
	for _, o := range s.muxes.m {
		if o.owner == owner && o.tenant == tenant {
			held++
		}
	}
	if held >= maxStreamMuxes {
		s.muxes.mu.Unlock()
		WriteError(w, http.StatusTooManyRequests, fmt.Sprintf("this account has %d shared streams open; close a tab", held))
		return
	}
	if s.muxes.m == nil {
		s.muxes.m = map[string]*streamMux{}
	}
	s.muxes.m[id] = m
	s.muxes.mu.Unlock()
	defer func() {
		s.muxes.mu.Lock()
		delete(s.muxes.m, id)
		s.muxes.mu.Unlock()
		m.mu.Lock()
		for _, stop := range m.chans {
			stop()
		}
		m.chans = nil
		m.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	b, _ := json.Marshal(map[string]string{"id": id})
	fmt.Fprintf(w, "event: mux\ndata: %s\n\n", b)
	flusher.Flush()
	tick := time.NewTicker(muxKeepalive)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-m.frames:
			if _, err := fmt.Fprintf(w, "event: c\ndata: %s\n\n", f); err != nil {
				return
			}
			// Frames already queued go out in the same write.
			for more := true; more; {
				select {
				case f := <-m.frames:
					if _, err := fmt.Fprintf(w, "event: c\ndata: %s\n\n", f); err != nil {
						return
					}
				default:
					more = false
				}
			}
			flusher.Flush()
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// muxFor is the caller's own shared stream id, or an error answered.
func (s *Server) muxFor(w http.ResponseWriter, r *http.Request) (*streamMux, string, bool) {
	ch := r.PathValue("ch")
	if !muxChannelID.MatchString(ch) {
		WriteError(w, http.StatusBadRequest, "invalid channel")
		return nil, "", false
	}
	s.muxes.mu.Lock()
	m := s.muxes.m[r.PathValue("mux")]
	s.muxes.mu.Unlock()
	// Another account's stream is answered as missing, so its frames never reach this caller's page.
	if m == nil || m.owner != UserOf(r.Context()) || m.tenant != TenantOf(r.Context()) {
		WriteError(w, http.StatusNotFound, "stream not found")
		return nil, "", false
	}
	return m, ch, true
}

// muxOpenRequest is a channel the page asks for.
type muxOpenRequest struct {
	Path        string `json:"path"`
	LastEventID string `json:"last_event_id,omitempty"`
}

// openStreamChannel is POST /v1/streams/{mux}/{ch}: run the route path names
// as a GET of its own, with this request's credentials, and carry its events
// on the shared stream as channel ch. Its status, refusal included, arrives
// there too.
func (s *Server) openStreamChannel(top http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ch, ok := s.muxFor(w, r)
		if !ok {
			return
		}
		capBody(w, r)
		var req muxOpenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badBody(w, err, "path is required")
			return
		}
		u, err := url.Parse(req.Path)
		if err != nil || u.IsAbs() || u.Host != "" || !muxPaths.MatchString(u.Path) || strings.ContainsAny(req.LastEventID, "\r\n") {
			WriteError(w, http.StatusBadRequest, "not a stream this route carries")
			return
		}
		// The channel's own request: this one's headers, so its credentials
		// and origin are checked again, and none of its identity.
		base := context.Background()
		if v := r.Context().Value(http.LocalAddrContextKey); v != nil {
			base = context.WithValue(base, http.LocalAddrContextKey, v)
		}
		ctx, cancel := context.WithCancel(base)
		stop := context.AfterFunc(m.ctx, cancel)
		inner := (&http.Request{
			Method: http.MethodGet, URL: &url.URL{Path: u.Path, RawQuery: u.RawQuery}, RequestURI: u.RequestURI(),
			Proto: r.Proto, ProtoMajor: r.ProtoMajor, ProtoMinor: r.ProtoMinor,
			Header: r.Header.Clone(), Host: r.Host, RemoteAddr: r.RemoteAddr, TLS: r.TLS, Body: http.NoBody,
		}).WithContext(ctx)
		for _, h := range []string{"Content-Type", "Content-Length", "Last-Event-ID"} {
			inner.Header.Del(h)
		}
		inner.Header.Set("Accept", "text/event-stream")
		if req.LastEventID != "" {
			inner.Header.Set("Last-Event-ID", req.LastEventID)
		}

		m.mu.Lock()
		switch {
		case m.chans == nil:
			m.mu.Unlock()
			stop()
			cancel()
			WriteError(w, http.StatusNotFound, "stream not found")
			return
		case len(m.chans) >= maxStreamChannels:
			m.mu.Unlock()
			stop()
			cancel()
			WriteError(w, http.StatusTooManyRequests, fmt.Sprintf("this page has %d streams open; close a terminal", maxStreamChannels))
			return
		}
		if prev := m.chans[ch]; prev != nil {
			prev()
		}
		var once sync.Once
		end := func() { once.Do(func() { stop(); cancel() }) }
		m.chans[ch] = end
		m.mu.Unlock()

		go func() {
			defer end()
			cw := &channelWriter{m: m, ch: ch, header: http.Header{}}
			top.ServeHTTP(cw, inner)
			cw.finish()
			m.mu.Lock()
			if m.chans != nil {
				delete(m.chans, ch)
			}
			m.mu.Unlock()
		}()
		w.WriteHeader(http.StatusAccepted)
	}
}

// closeStreamChannel is DELETE /v1/streams/{mux}/{ch}: the page is done with it.
func (s *Server) closeStreamChannel(w http.ResponseWriter, r *http.Request) {
	m, ch, ok := s.muxFor(w, r)
	if !ok {
		return
	}
	m.mu.Lock()
	if end := m.chans[ch]; end != nil {
		end()
	}
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// channelWriter is a channel's response: an event stream is read event by
// event onto the shared stream; any other answer becomes the channel's refusal.
type channelWriter struct {
	m      *streamMux
	ch     string
	header http.Header
	status int
	open   bool
	body   bytes.Buffer // a refusal's body, or the event stream not yet read
	event  string
	data   []string
	id     string
	hasID  bool
	gone   bool
}

func (c *channelWriter) Header() http.Header { return c.header }

func (c *channelWriter) WriteHeader(code int) {
	if c.status != 0 {
		return
	}
	c.status = code
	if code == http.StatusOK && strings.HasPrefix(c.header.Get("Content-Type"), "text/event-stream") {
		c.open = true
		c.gone = !c.m.send(muxFrame{Ch: c.ch, Type: "open"})
	}
}

func (c *channelWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if c.gone {
		return 0, context.Canceled
	}
	if !c.open {
		if room := maxStreamFail - c.body.Len(); room > 0 {
			c.body.Write(b[:min(len(b), room)])
		}
		return len(b), nil
	}
	c.body.Write(b)
	for {
		i := bytes.IndexByte(c.body.Bytes(), '\n')
		if i < 0 {
			// An unfinished line waits for the rest.
			return len(b), nil
		}
		line := string(c.body.Next(i + 1))
		if !c.line(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")) {
			c.gone = true
			return 0, context.Canceled
		}
	}
}

// line takes one line of the event stream; false once the shared stream has ended.
func (c *channelWriter) line(l string) bool {
	if l == "" {
		if len(c.data) == 0 && c.event == "" {
			return true
		}
		f := muxFrame{Ch: c.ch, Type: "ev", Event: c.event, Data: strings.Join(c.data, "\n"), ID: c.id, HasID: c.hasID}
		c.event, c.data, c.hasID = "", nil, false
		return c.m.send(f)
	}
	if strings.HasPrefix(l, ":") {
		return true
	}
	field, value, _ := strings.Cut(l, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		c.event = value
	case "data":
		c.data = append(c.data, value)
	case "id":
		c.id, c.hasID = value, true
	case "retry":
		var ms int
		if _, err := fmt.Sscanf(value, "%d", &ms); err == nil && ms > 0 {
			return c.m.send(muxFrame{Ch: c.ch, Type: "retry", Retry: ms})
		}
	}
	return true
}

// Flush has nothing to do: each event is sent as it is read.
func (c *channelWriter) Flush() {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
}

// finish says how the channel's request ended.
func (c *channelWriter) finish() {
	if c.gone {
		return
	}
	if c.open {
		c.m.send(muxFrame{Ch: c.ch, Type: "end"})
		return
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	msg := strings.TrimSpace(c.body.String())
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &e) == nil && e.Error != "" {
		msg = e.Error
	}
	c.m.send(muxFrame{Ch: c.ch, Type: "fail", Status: status, Message: msg})
}
