package sandboxcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/profiles"
	"github.com/zybuu-ai/abhed/wsconn"
)

// client talks to one server as one profile, keeping the profile's tokens
// fresh in the profiles file.
type client struct {
	path string
	reg  *profiles.File
	name string
	p    *profiles.Profile
	hc   *http.Client
	// retryWait is the first pause before a retry; doubled each time.
	retryWait time.Duration
}

// openClient resolves the profile a command runs as: -profile, the profile
// whose server is -server, or the current one.
func openClient(path, name, server string) (*client, error) {
	reg, err := profiles.Load(path)
	if err != nil {
		return nil, err
	}
	if server != "" {
		u, err := profiles.NormalizeURL(server, true)
		if err != nil {
			return nil, usageError{err}
		}
		found := ""
		for _, n := range reg.Names() {
			if reg.Profiles[n].URL == u && (name == "" || n == name) {
				found = n
				break
			}
		}
		if found == "" {
			if name != "" {
				return nil, &apiError{status: http.StatusUnauthorized, code: "not_signed_in",
					msg: fmt.Sprintf("profile %q is not for %s", name, u)}
			}
			return nil, &apiError{status: http.StatusUnauthorized, code: "not_signed_in",
				msg: fmt.Sprintf("no profile for %s; run abhed login %s", u, u)}
		}
		name = found
	}
	n, p, err := reg.Get(name)
	if err != nil {
		return nil, &apiError{status: http.StatusUnauthorized, code: "not_signed_in", msg: err.Error()}
	}
	if !p.SignedIn(time.Now()) && p.RefreshToken == "" {
		return nil, &apiError{status: http.StatusUnauthorized, code: "not_signed_in",
			msg: fmt.Sprintf("the profile %s (%s) is not signed in; run abhed login %s", n, p.URL, p.URL)}
	}
	hc, err := p.HTTPClient()
	if err != nil {
		return nil, err
	}
	// Streams (exec output, copies) last as long as they last; each plain
	// call carries its own deadline instead.
	hc.Timeout = 0
	return &client{path: path, reg: reg, name: n, p: p, hc: hc, retryWait: 500 * time.Millisecond}, nil
}

// saved keeps a refreshed token.
func (c *client) saved(*profiles.Profile) error { return profiles.Save(c.path, c.reg) }

// apiError is a call the server refused, or one that could not be made.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string {
	if e.status == 0 {
		return e.msg
	}
	if e.code == "" {
		return fmt.Sprintf("%s (%d)", e.msg, e.status)
	}
	if e.msg == "" {
		return fmt.Sprintf("the server answered %d %s", e.status, http.StatusText(e.status))
	}
	return fmt.Sprintf("%s (%d %s)", e.msg, e.status, e.code)
}

// usageError is a mistake in how a command was called.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// readAPIError reads the contract's error body from a refused response.
func readAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return apiErrorFrom(resp.StatusCode, b)
}

func apiErrorFrom(status int, b []byte) error {
	var body struct {
		Error any `json:"error"`
	}
	e := &apiError{status: status}
	if json.Unmarshal(b, &body) == nil {
		switch v := body.Error.(type) {
		case string:
			e.msg = v
		case map[string]any:
			e.code, _ = v["code"].(string)
			e.msg, _ = v["message"].(string)
		}
	}
	if e.msg == "" {
		e.msg = strings.TrimSpace(string(b))
		if len(e.msg) > 200 || strings.ContainsAny(e.msg, "<\x00") {
			e.msg = ""
		}
	}
	return e
}

// newKey is a fresh Idempotency-Key.
func newKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("sandbox: system random source unavailable: " + err.Error())
	}
	return "cli-" + hex.EncodeToString(b)
}

// call is one request.
type call struct {
	method, path string
	body         []byte
	contentType  string
	header       http.Header
	// mutating calls carry an Idempotency-Key, the same one on every retry.
	mutating bool
	// timeout bounds the whole call; zero for a stream.
	timeout time.Duration
	// once is a call never sent twice: one that is not idempotent on the server.
	once bool
	// again marks a call sent before whose answer was lost: a 409 may be
	// the first try still running.
	again bool
	// stream, when set, is the body, read once; a call that would need it
	// again fails instead.
	stream io.Reader
}

// maxAttempts bounds how often a call is sent: once, then again after a
// failure that left the server's answer unknown, under the same key.
const maxAttempts = 4

// retryable reports whether a call may be sent again: it never reached an
// answer, or the server said to come back later. A mutating call is sent
// again only with its Idempotency-Key, so it acts once.
func retryable(status int, err error, attempt int) bool {
	switch {
	case err != nil:
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	case status == http.StatusBadGateway, status == http.StatusGatewayTimeout, status == http.StatusTooManyRequests:
		return true
	case status == http.StatusServiceUnavailable:
		return true
	case status == http.StatusConflict && attempt > 0:
		// The first try may still be running on the server: wait for its answer.
		return true
	}
	return false
}

// do sends c, refreshing the profile's token as needed and retrying what
// may be retried. The caller closes the response's body. A response that
// is not 2xx is returned as an apiError.
func (cl *client) do(ctx context.Context, c call) (*http.Response, error) {
	if c.mutating {
		if c.header == nil {
			c.header = http.Header{}
		}
		if c.header.Get("Idempotency-Key") == "" {
			c.header.Set("Idempotency-Key", newKey())
		}
	}
	cancel := func() {}
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	wait := cl.retryWait
	streamed := false
	if c.stream != nil {
		c.once = true
	}
	for attempt := 0; ; attempt++ {
		resp, err := cl.p.Do(ctx, cl.hc, func() (*http.Request, error) {
			var body io.Reader = bytes.NewReader(c.body)
			if c.stream != nil {
				if streamed {
					return nil, errors.New("the sign-in had to be renewed after the upload began; run the command again")
				}
				body, streamed = c.stream, true
			}
			req, err := cl.p.NewRequest(c.method, c.path, body)
			if err != nil {
				return nil, err
			}
			if c.body != nil || c.stream != nil {
				ct := c.contentType
				if ct == "" {
					ct = "application/json"
				}
				req.Header.Set("Content-Type", ct)
			}
			for k, vs := range c.header {
				req.Header[k] = vs
			}
			return req, nil
		}, cl.saved)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if err == nil && status < 300 {
			resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		if attempt+1 < maxAttempts && !c.once && retryable(status, err, attempt+boolInt(c.again)) {
			pause := wait
			if resp != nil {
				if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 {
					pause = min(time.Duration(s)*time.Second, 30*time.Second)
				}
				_ = resp.Body.Close()
			}
			select {
			case <-ctx.Done():
				cancel()
				return nil, ctx.Err()
			case <-time.After(pause):
			}
			wait *= 2
			continue
		}
		defer cancel()
		if err != nil {
			return nil, &apiError{msg: fmt.Sprintf("could not reach %s: %v", cl.p.URL, unwrapURLError(err))}
		}
		defer func() { _ = resp.Body.Close() }()
		return nil, readAPIError(resp)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// unwrapURLError drops the method and URL Go's client puts in front.
func unwrapURLError(err error) error {
	if ue := new(url.Error); errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

type cancelBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// json sends c and decodes a JSON answer into out, returning the ETag.
func (cl *client) json(ctx context.Context, c call, out any) (string, error) {
	if c.timeout == 0 {
		c.timeout = time.Minute
	}
	resp, err := cl.do(ctx, c)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
			return "", fmt.Errorf("the server's answer could not be read: %w", err)
		}
	}
	return resp.Header.Get("ETag"), nil
}

// authHeader carries the profile's token for a WebSocket.
func (cl *client) authHeader() http.Header {
	h := http.Header{}
	if cl.p.Token != "" {
		typ := cl.p.TokenType
		if typ == "" || strings.EqualFold(typ, "bearer") {
			typ = "Bearer"
		}
		h.Set("Authorization", typ+" "+cl.p.Token)
	}
	return h
}

// dial opens a stream, refreshing the token first when it is near its
// expiry and once more when the server answers 401.
func (cl *client) dial(ctx context.Context, path, proto string) (*wsconn.Conn, error) {
	tc, err := cl.p.TLSConfig()
	if err != nil {
		return nil, err
	}
	refreshed := false
	refresh := func() bool {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if profiles.Refresh(rctx, cl.hc, cl.p) != nil {
			return false
		}
		_ = cl.saved(cl.p)
		return true
	}
	if cl.p.RefreshToken != "" && cl.p.NearExpiry(time.Now()) {
		refreshed = refresh()
	}
	for {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ws, err := wsconn.Dial(dctx, cl.p.URL+path, wsconn.DialOptions{Header: cl.authHeader(),
			Protocols: []string{proto}, TLSConfig: tc})
		cancel()
		if err == nil {
			return ws, nil
		}
		var he *wsconn.HandshakeError
		if !errors.As(err, &he) {
			return nil, &apiError{msg: fmt.Sprintf("could not reach %s: %v", cl.p.URL, err)}
		}
		if he.Status == http.StatusUnauthorized && !refreshed && cl.p.RefreshToken != "" {
			refreshed = true
			if refresh() {
				continue
			}
		}
		return nil, apiErrorFrom(he.Status, he.Body)
	}
}
