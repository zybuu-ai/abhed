package egress

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// UpstreamEnv is the environment a program Abhed starts for its own
// downloads (a Go module build, say) needs to go out through the upstream
// proxy in force to hosts, and a function that ends it; nil and a no-op with
// none. The program is given a loopback relay with a per-run token, never the
// proxy's credentials, which the relay adds itself; the relay reaches hosts
// alone.
func UpstreamEnv(hosts []string) (env []string, stop func(), err error) {
	proxy, np, stop, err := UpstreamRelay(hosts)
	if err != nil || proxy == "" {
		return nil, stop, err
	}
	return []string{"HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy, "https_proxy=" + proxy, "http_proxy=" + proxy,
		"NO_PROXY=" + np, "no_proxy=" + np}, stop, nil
}

// UpstreamRelay starts a loopback relay to the upstream proxy in force, for
// hosts alone (the ones the download needs), and returns its URL with a
// per-run token as its user, the no_proxy list for the program's own
// settings, and the function that ends it; "" and a no-op with no upstream
// proxy. A host alone is reached on 443 by CONNECT and 80 in absolute form;
// "host:port" on that port only. Any other target is refused before the
// proxy is asked.
func UpstreamRelay(hosts []string) (proxy, noProxy string, stop func(), err error) {
	u := CurrentUpstream()
	if u == nil {
		return "", "", func() {}, nil
	}
	r, err := startRelay(u, hosts)
	if err != nil {
		return "", "", func() {}, err
	}
	np := strings.Join(append([]string{"localhost", "127.0.0.1", "::1"}, u.NoProxy...), ",")
	return "http://abhed:" + r.token + "@" + r.ln.Addr().String(), np, r.close, nil
}

// relay is a loopback proxy that forwards to the upstream proxy, for the
// holder of its token alone.
type relay struct {
	u     *Upstream
	hosts map[string]map[string]bool // the only hosts it reaches, each on its ports ("" for the defaults)
	ln    net.Listener
	token string
	want  string // the Proxy-Authorization its clients send
	once  sync.Once
	wg    sync.WaitGroup

	mu     sync.Mutex
	conns  map[net.Conn]bool
	closed bool
}

func startRelay(u *Upstream, hosts []string) (*relay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		_ = ln.Close()
		return nil, err
	}
	tok := hex.EncodeToString(b)
	r := &relay{u: u, ln: ln, token: tok, want: "Basic " + base64.StdEncoding.EncodeToString([]byte("abhed:"+tok)),
		conns: map[net.Conn]bool{}, hosts: map[string]map[string]bool{}}
	for _, h := range hosts {
		port := ""
		if hh, p, err := net.SplitHostPort(h); err == nil {
			h, port = hh, p
		}
		if h = relayHost(h); h != "" {
			if r.hosts[h] == nil {
				r.hosts[h] = map[string]bool{}
			}
			r.hosts[h][port] = true
		}
	}
	r.wg.Add(1)
	go r.serve()
	return r, nil
}

// close stops the relay and ends every connection through it.
func (r *relay) close() {
	r.once.Do(func() {
		_ = r.ln.Close()
		r.mu.Lock()
		r.closed = true
		for c := range r.conns {
			_ = c.Close()
		}
		r.mu.Unlock()
	})
	r.wg.Wait()
}

// track keeps c until untrack, so close ends it; false once closed.
func (r *relay) track(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.conns[c] = true
	return true
}

func (r *relay) untrack(c net.Conn) {
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
}

func (r *relay) serve() {
	defer r.wg.Done()
	for {
		c, err := r.ln.Accept()
		if err != nil {
			return
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handle(c)
		}()
	}
}

func (r *relay) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	if !r.track(c) {
		return
	}
	defer r.untrack(c)
	_ = c.SetReadDeadline(time.Now().Add(headerTimeout))
	br := bufio.NewReader(io.LimitReader(c, maxHeader))
	req, err := http.ReadRequest(br)
	if err != nil {
		writeStatus(c, http.StatusBadRequest, "abhed relay: malformed request\n", nil)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if subtle.ConstantTimeCompare([]byte(req.Header.Get("Proxy-Authorization")), []byte(r.want)) != 1 {
		writeStatus(c, http.StatusProxyAuthRequired, "abhed relay: not this relay's client\n",
			http.Header{"Proxy-Authenticate": {`Basic realm="abhed"`}})
		return
	}
	target := req.Host
	if req.Method != http.MethodConnect && req.URL != nil {
		target = req.URL.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = target, ""
	}
	ports := r.hosts[relayHost(host)]
	if ports == nil {
		writeStatus(c, http.StatusForbidden, "abhed relay: "+host+" is not a host this download needs\n", nil)
		return
	}
	if !relayPortOK(ports, port, req.Method == http.MethodConnect) {
		writeStatus(c, http.StatusForbidden, "abhed relay: port "+port+" of "+host+" is not one this download needs\n", nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if req.Method == http.MethodConnect {
		up, err := r.u.DialTunnel(ctx, req.Host, 30*time.Second)
		if err != nil {
			writeStatus(c, http.StatusBadGateway, "abhed relay: "+relayError(err)+"\n", nil)
			return
		}
		defer func() { _ = up.Close() }()
		if !r.track(up) {
			return
		}
		defer r.untrack(up)
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		splice(c, br, up)
		return
	}
	if req.URL == nil || !req.URL.IsAbs() || !strings.EqualFold(req.URL.Scheme, "http") {
		writeStatus(c, http.StatusBadRequest, "abhed relay: only CONNECT and absolute http requests\n", nil)
		return
	}
	up, err := r.u.DialForward(ctx, 30*time.Second)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "abhed relay: "+relayError(err)+"\n", nil)
		return
	}
	defer func() { _ = up.Close() }()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	if auth, err := r.u.ForwardHeader(); err != nil {
		writeStatus(c, http.StatusBadGateway, "abhed relay: the upstream proxy's credentials could not be read\n", nil)
		return
	} else if auth != "" {
		req.Header.Set("Proxy-Authorization", auth)
	}
	req.RequestURI, req.Close = "", true
	if err := req.WriteProxy(up); err != nil {
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(up), req)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "abhed relay: the upstream response was malformed\n", nil)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	resp.Close = true
	_ = resp.Write(c)
}

// relayError is an upstream failure as the relay's client is told it: the
// proxy's status or that it could not be reached, never a header.
func relayError(err error) string {
	var r *UpstreamRefused
	if errors.As(err, &r) {
		return r.Error()
	}
	return "the upstream proxy could not be reached"
}

// splice copies both ways until either side ends, first what br holds.
func splice(c net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		if n := br.Buffered(); n > 0 {
			b, _ := br.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, c)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, up)
		closeWrite(c)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// relayHost is a host as the relay compares it.
func relayHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
}

// relayPortOK reports whether port is one the relay reaches for a host
// listed with ports: a port named with the host, or else 443 for CONNECT
// and 80 for absolute-form http.
func relayPortOK(ports map[string]bool, port string, connect bool) bool {
	if ports[port] && port != "" {
		return true
	}
	if !ports[""] {
		return false
	}
	if connect {
		return port == "443"
	}
	return port == "" || port == "80"
}
