package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// HTTP transport for remote MCP servers.
//
// The stdio transport only reaches servers Abhed can spawn as a subprocess,
// which excludes every server that already runs somewhere else — a team's
// shared retrieval service, an internal tool gateway, anything in a cluster.
// Those speak MCP over HTTP.
//
// Two shapes exist in the wild and this handles both, because a deployment
// does not get to choose which one its vendor implemented:
//
//   - Streamable HTTP (the current spec): every request POSTs to one endpoint.
//     The reply is either a JSON object or an SSE stream, indicated by the
//     response Content-Type.
//   - HTTP+SSE (the older spec): a long-lived GET carries every server
//     message, and the server names a separate endpoint for the client to POST
//     to. Still widely deployed.
//
// The transport negotiates by trying the modern shape first and falling back,
// rather than making the operator declare which one their server speaks.

// HTTPTransport speaks MCP to a server over HTTP.
type HTTPTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	// incoming carries every message from the server, whichever shape it
	// arrived in, so Receive has one queue to read.
	incoming chan []byte
	errs     chan error

	// endpointReady is closed once the legacy handshake has named the POST
	// endpoint. Nil for the modern shape, where the URL is already known.
	//
	// Without this, the first Send races the endpoint event and POSTs to the
	// stream URL, which the server does not accept — the request appears to
	// succeed and the reply never comes.
	endpointReady chan struct{}
	readyOnce     sync.Once

	mu       sync.Mutex
	postURL  string // set by the legacy SSE handshake; otherwise == url
	sessionI string // Mcp-Session-Id, echoed back on later requests
	closed   bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// HTTPConfig configures a remote MCP connection.
type HTTPConfig struct {
	URL     string
	Headers map[string]string
	Timeout time.Duration
	Client  *http.Client
	// Egress is the tool set's egress guard for the default client; nil uses the request's.
	Egress *egress.Guard
}

// NewHTTPTransport connects to a remote MCP server.
func NewHTTPTransport(ctx context.Context, cfg HTTPConfig) (*HTTPTransport, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: url is required for an http server")
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("mcp: url must be http or https, got %q", cfg.URL)
	}
	client := cfg.Client
	if client == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			// Long, because a tool call may legitimately take a while. The
			// stream connection below uses no timeout at all.
			timeout = 120 * time.Second
		}
		// Judged and recorded by the egress guard under the allowlist.
		client = &http.Client{Timeout: timeout, Transport: &egress.Transport{Kind: egress.KindMCP, Guard: cfg.Egress}}
	}

	// Detached from ctx's end, but not its values: the stream keeps its set's guard.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t := &HTTPTransport{
		url: cfg.URL, headers: cfg.Headers, client: client,
		postURL:  cfg.URL,
		incoming: make(chan []byte, 64),
		errs:     make(chan error, 1),
		cancel:   cancel,
	}

	// Probe for the legacy HTTP+SSE shape: a GET that returns an event stream.
	// A server speaking the modern shape answers 405 or 404 here, which is not
	// an error — it just means POST-only.
	if err := t.tryLegacySSE(streamCtx); err != nil {
		cancel()
		return nil, err
	}
	return t, nil
}

// tryLegacySSE opens the long-lived GET used by the older spec. It is not an
// error for a server to refuse: that identifies it as the newer shape.
func (t *HTTPTransport) tryLegacySSE(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	t.applyHeaders(req)

	// No client timeout on the stream: it is meant to stay open.
	streamClient := &http.Client{Transport: t.client.Transport}
	resp, err := streamClient.Do(req) //nolint:bodyclose // the body is the event stream; the reader goroutine below closes it when the stream ends
	if err != nil {
		// Connection refused here means the server is unreachable, which the
		// first POST would hit anyway — report it now with a clearer message.
		return fmt.Errorf("mcp: cannot reach %s: %w", t.url, err)
	}
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		return nil // modern shape; every reply comes back on its own POST
	}

	t.endpointReady = make(chan struct{})
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() { _ = resp.Body.Close() }()
		t.readSSE(ctx, resp.Body, true)
		// If the stream ends without ever naming an endpoint, unblock Send so
		// it fails with a real error instead of hanging until the deadline.
		t.readyOnce.Do(func() { close(t.endpointReady) })
	}()
	return nil
}

// readSSE parses an event stream, forwarding data frames to Receive.
//
// learnEndpoint distinguishes the two shapes: on the legacy long-lived stream
// the server sends an "endpoint" event naming where to POST, and until that
// arrives the client has nowhere to send. On a per-response stream there is no
// endpoint event and none is expected.
func (t *HTTPTransport) readSSE(ctx context.Context, body io.Reader, learnEndpoint bool) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)

	var event, data strings.Builder
	flush := func() {
		payload := strings.TrimSpace(data.String())
		name := strings.TrimSpace(event.String())
		event.Reset()
		data.Reset()
		if payload == "" {
			return
		}
		if learnEndpoint && name == "endpoint" {
			t.mu.Lock()
			t.postURL = resolveEndpoint(t.url, payload)
			t.mu.Unlock()
			t.readyOnce.Do(func() { close(t.endpointReady) })
			return
		}
		select {
		case t.incoming <- []byte(payload):
		case <-ctx.Done():
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, ":"): // a comment, used as a keepalive
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	flush()
}

// resolveEndpoint turns the endpoint event's value into an absolute URL. It is
// commonly a path like "/messages?sessionId=abc".
//
// The result is pinned to the configured server's origin. The endpoint value
// comes from the server over the SSE stream, and the client then POSTs to it
// carrying the operator-configured auth headers (an API key, a bearer token).
// A hostile or compromised server that named an ABSOLUTE URL on another host
// would redirect those credentials to a destination it chose — an SSRF /
// egress redirect. So only the path and query the server asks for are honoured;
// the scheme, host and port always stay the operator's. A relative value is
// resolved against the base as before; an off-origin absolute value is
// neutralised by forcing the origin back to the base's.
func resolveEndpoint(base, value string) string {
	value = strings.TrimSpace(value)
	baseURL, err := url.Parse(base)
	if err != nil {
		return base
	}
	ref, err := url.Parse(value)
	if err != nil {
		return base
	}
	resolved := baseURL.ResolveReference(ref)
	resolved.Scheme = baseURL.Scheme
	resolved.Host = baseURL.Host
	resolved.User = baseURL.User
	return resolved.String()
}

func (t *HTTPTransport) applyHeaders(req *http.Request) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sid := t.sessionI
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
}

// Send POSTs one JSON-RPC message.
func (t *HTTPTransport) Send(ctx context.Context, payload []byte) error {
	// On the legacy shape the server names its POST endpoint on the stream,
	// so the first Send has to wait for it to arrive.
	if t.endpointReady != nil {
		select {
		case <-t.endpointReady:
		case <-ctx.Done():
			return fmt.Errorf("mcp: server never announced its message endpoint: %w", ctx.Err())
		}
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("mcp: transport is closed")
	}
	dest := t.postURL
	t.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Accept both, because the server chooses which to send per request.
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.applyHeaders(req)

	resp, err := t.client.Do(req) //nolint:bodyclose // every branch below closes the body, the streamed one from its reader goroutine
	if err != nil {
		return fmt.Errorf("mcp: request failed: %w", err)
	}

	// A server that assigns a session id expects it echoed on every later
	// request; losing it silently starts a new session mid-conversation.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionI = sid
		t.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusAccepted, resp.StatusCode == http.StatusNoContent:
		// Notification accepted; the reply, if any, arrives on the stream.
		_ = resp.Body.Close()
		return nil
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return fmt.Errorf("mcp: server returned %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Streamed reply: read it in the background so Send does not block on
		// a tool call that takes a while to produce its result.
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer resp.Body.Close()
			t.readSSE(context.Background(), resp.Body, false)
		}()
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("mcp: read response: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	select {
	case t.incoming <- body:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Receive returns the next message from the server.
func (t *HTTPTransport) Receive(ctx context.Context) ([]byte, error) {
	select {
	case msg := <-t.incoming:
		return msg, nil
	case err := <-t.errs:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	t.cancel()
	t.client.CloseIdleConnections()
	return nil
}
