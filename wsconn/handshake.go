package wsconn

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6455 fixes SHA-1 for the accept key; it protects nothing
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// HandshakeError is an upgrade refused: on the server, before anything was
// written, with the status to answer; on the client, the server's answer.
type HandshakeError struct {
	Status int
	Msg    string
	// Body is the start of the server's answer, on the client.
	Body []byte
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("websocket handshake: %d %s", e.Status, e.Msg)
}

func herr(status int, format string, args ...any) *HandshakeError {
	return &HandshakeError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// AcceptKey is the Sec-WebSocket-Accept answer to key.
func AcceptKey(key string) string {
	h := sha1.Sum([]byte(key + guid)) // #nosec G401 -- fixed by RFC 6455
	return base64.StdEncoding.EncodeToString(h[:])
}

// HeaderHas reports whether a comma-separated header holds token, ignoring case.
func HeaderHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// IsUpgrade reports whether r asks to become a WebSocket.
func IsUpgrade(r *http.Request) bool {
	return HeaderHas(r.Header, "Connection", "upgrade") && HeaderHas(r.Header, "Upgrade", "websocket")
}

// UpgradeOptions say what a server accepts.
type UpgradeOptions struct {
	// Protocols are the subprotocols served, in order of preference. When
	// set, a client must offer one of them.
	Protocols []string
	// CheckOrigin decides a request that carries an Origin header. When
	// nil, only an origin of the request's own host is accepted. A request
	// with no Origin (not from a browser) is never refused for it.
	CheckOrigin func(r *http.Request) bool
	Limits      Limits
}

// Check validates r as an upgrade without answering it, and returns the
// subprotocol it would agree. A refusal says what status to answer with.
func Check(r *http.Request, o UpgradeOptions) (string, error) {
	if r.Method != http.MethodGet {
		return "", herr(http.StatusMethodNotAllowed, "a WebSocket upgrade is a GET")
	}
	if !r.ProtoAtLeast(1, 1) || r.ProtoMajor != 1 {
		return "", herr(http.StatusHTTPVersionNotSupported, "a WebSocket upgrade needs HTTP/1.1")
	}
	if !IsUpgrade(r) {
		return "", herr(http.StatusUpgradeRequired, "this resource is a WebSocket; send Connection: Upgrade and Upgrade: websocket")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return "", herr(http.StatusUpgradeRequired, "Sec-WebSocket-Version must be 13")
	}
	key, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
	if err != nil || len(key) != 16 {
		return "", herr(http.StatusBadRequest, "Sec-WebSocket-Key must be 16 bytes in base64")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		ok := false
		if o.CheckOrigin != nil {
			ok = o.CheckOrigin(r)
		} else {
			ok = SameHost(origin, r.Host)
		}
		if !ok {
			return "", herr(http.StatusForbidden, "cross-origin WebSocket refused")
		}
	}
	if len(o.Protocols) == 0 {
		return "", nil
	}
	for _, want := range o.Protocols {
		if HeaderHas(r.Header, "Sec-WebSocket-Protocol", want) {
			return want, nil
		}
	}
	return "", herr(http.StatusBadRequest, "offer the subprotocol %s in Sec-WebSocket-Protocol", strings.Join(o.Protocols, " or "))
}

// SameHost reports whether origin names host, over http or https.
func SameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" && u.Path != "/" {
		return false
	}
	return host != "" && strings.EqualFold(u.Host, host)
}

// Accept answers a request Check passed with 101 and returns the
// connection, taken over from the HTTP server: its deadlines are cleared,
// and the caller owns it from here.
func Accept(w http.ResponseWriter, r *http.Request, proto string, l Limits) (*Conn, error) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("websocket: taking over the connection: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + AcceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n")
	if proto != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + proto + "\r\n")
	}
	b.WriteString("\r\n")
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(b.String())); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	// Bytes the client sent after its request are already in the reader.
	return newConn(conn, brw.Reader, false, l, proto), nil
}

// Upgrade is Check then Accept. On a *HandshakeError nothing was written,
// and the caller answers with its Status.
func Upgrade(w http.ResponseWriter, r *http.Request, o UpgradeOptions) (*Conn, error) {
	proto, err := Check(r, o)
	if err != nil {
		return nil, err
	}
	return Accept(w, r, proto, o.Limits)
}

// DialOptions say how a client connects.
type DialOptions struct {
	// Header is sent with the upgrade, such as Authorization.
	Header http.Header
	// Protocols are offered in order of preference; the server must choose one.
	Protocols []string
	TLSConfig *tls.Config
	// NetDial, when set, opens the TCP connection.
	NetDial func(ctx context.Context, network, addr string) (net.Conn, error)
	Limits  Limits
}

// maxHandshakeBody bounds what is kept of a refusal's body.
const maxHandshakeBody = 64 << 10

// Dial opens a WebSocket to rawURL (ws, wss, http or https). A refused
// upgrade is a *HandshakeError carrying the server's status and answer.
func Dial(ctx context.Context, rawURL string, o DialOptions) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	secure := false
	switch u.Scheme {
	case "ws", "http":
	case "wss", "https":
		secure = true
	default:
		return nil, fmt.Errorf("websocket: %q is not a ws, wss, http or https URL", rawURL)
	}
	if u.User != nil {
		return nil, errors.New("websocket: credentials in the URL are not sent; use a header")
	}
	host := u.Host
	if u.Port() == "" {
		port := "80"
		if secure {
			port = "443"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	dial := o.NetDial
	if dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	raw, err := dial(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	conn := raw
	// Until the upgrade is done the caller's context bounds every read and write.
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Now()) })
	ws, err := func() (*Conn, error) {
		if secure {
			cfg := &tls.Config{MinVersion: tls.VersionTLS12}
			if o.TLSConfig != nil {
				cfg = o.TLSConfig.Clone()
			}
			// The upgrade is HTTP/1.1; HTTP/2 has no Upgrade.
			cfg.NextProtos = []string{"http/1.1"}
			if cfg.ServerName == "" {
				cfg.ServerName = u.Hostname()
			}
			tc := tls.Client(raw, cfg)
			if err := tc.HandshakeContext(ctx); err != nil {
				return nil, err
			}
			conn = tc
		}
		return clientHandshake(conn, u, o)
	}()
	if !stop() {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		_ = raw.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return ws, nil
}

func clientHandshake(conn net.Conn, u *url.URL, o DialOptions) (*Conn, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	hu := *u
	hu.Scheme = "http"
	if u.Scheme == "wss" || u.Scheme == "https" {
		hu.Scheme = "https"
	}
	req, err := http.NewRequest(http.MethodGet, hu.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range o.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	if len(o.Protocols) > 0 {
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(o.Protocols, ", "))
	}
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(conn, 32<<10)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("websocket: the server's answer: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHandshakeBody))
		_ = resp.Body.Close()
		return nil, &HandshakeError{Status: resp.StatusCode, Msg: http.StatusText(resp.StatusCode), Body: body}
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || !HeaderHas(resp.Header, "Connection", "upgrade") {
		return nil, errors.New("websocket: the server did not upgrade the connection")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != AcceptKey(key) {
		return nil, errors.New("websocket: the server's accept key is wrong")
	}
	proto := resp.Header.Get("Sec-WebSocket-Protocol")
	if len(o.Protocols) > 0 || proto != "" {
		ok := false
		for _, p := range o.Protocols {
			ok = ok || p == proto
		}
		if !ok {
			return nil, fmt.Errorf("websocket: the server chose subprotocol %q, which was not offered", proto)
		}
	}
	return newConn(conn, br, true, o.Limits, proto), nil
}
