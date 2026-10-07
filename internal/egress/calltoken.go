package egress

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
)

// maxEnded bounds the ended calls whose credentials are still recognised, so
// their late traffic is recorded under the call rather than as a stranger's.
const maxEnded = 1024

var (
	errNoCredential = errors.New("missing or wrong proxy credentials")
	errCallEnded    = errors.New("the call this credential was issued to has ended")
)

// credential is one call's token as the proxy holds it: the call it names,
// whether that call has ended, and the connections made with it.
type credential struct {
	callID string
	ended  bool
	conns  map[net.Conn]struct{}
}

// Call is the credential a proxy issued to one call: the only proof of the
// call id the proxy accepts. It is valid until End.
type Call struct {
	p     *Proxy
	id    string
	token string
	once  sync.Once
}

// Issue gives callID a credential of its own. The call id the proxy records
// is the one issued here, never one a client sends; End revokes it.
func (p *Proxy) Issue(callID string) (*Call, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	c := &Call{p: p, id: callID, token: hex.EncodeToString(b[:])}
	p.mu.Lock()
	if p.creds == nil {
		p.creds = map[[32]byte]*credential{}
	}
	p.creds[tokenKey(c.token)] = &credential{callID: callID, conns: map[net.Conn]struct{}{}}
	p.mu.Unlock()
	return c, nil
}

// ID is the call id the credential was issued for.
func (c *Call) ID() string { return c.id }

// URL is the proxy URL for the call: its id as the user name, for a reader of
// the environment only, and its token as the password.
func (c *Call) URL() string {
	user := c.id
	if user == "" {
		user = "abhed"
	}
	u := url.URL{Scheme: "http", User: url.UserPassword(user, c.token), Host: c.p.addr.String()}
	return u.String()
}

// Env is the environment that sends the call's HTTP clients through the
// proxy, in both cases since clients read one or the other. Loopback is left
// direct: on the sandbox tiers it is the sandbox's own.
func (c *Call) Env() []string {
	u := c.URL()
	const noProxy = "localhost,127.0.0.1,::1"
	return []string{
		"HTTP_PROXY=" + u, "http_proxy=" + u,
		"HTTPS_PROXY=" + u, "https_proxy=" + u,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

// End revokes the credential and closes the connections made with it; a
// process left running after its call ended is refused from then on.
func (c *Call) End() {
	c.once.Do(func() {
		p := c.p
		key := tokenKey(c.token)
		p.mu.Lock()
		cr := p.creds[key]
		var open []net.Conn
		if cr != nil {
			cr.ended = true
			for k := range cr.conns {
				open = append(open, k)
			}
			cr.conns = nil
			p.ended = append(p.ended, key)
			if len(p.ended) > maxEnded {
				delete(p.creds, p.ended[0])
				p.ended = p.ended[1:]
			}
		}
		p.mu.Unlock()
		for _, k := range open {
			_ = k.Close()
		}
	})
}

// tokenKey hashes a token, so the map lookup never compares the secret itself.
func tokenKey(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// authorize reads the Basic credentials and returns the call id they were
// issued for, whatever user name the client sent, and the token's key. An
// ended call's credential names its call and errCallEnded.
func (p *Proxy) authorize(h string) (string, [32]byte, error) {
	scheme, enc, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", [32]byte{}, errNoCredential
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", [32]byte{}, errNoCredential
	}
	_, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", [32]byte{}, errNoCredential
	}
	key := tokenKey(pass)
	p.mu.Lock()
	defer p.mu.Unlock()
	cr := p.creds[key]
	switch {
	case cr == nil:
		return "", key, errNoCredential
	case cr.ended:
		return cr.callID, key, errCallEnded
	}
	return cr.callID, key, nil
}

// bind counts c against the credential key, so ending the call closes it;
// false if the call ended since it was authorised.
func (p *Proxy) bind(key [32]byte, c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cr := p.creds[key]
	if cr == nil || cr.ended {
		return false
	}
	cr.conns[c] = struct{}{}
	return true
}

func (p *Proxy) unbind(key [32]byte, c net.Conn) {
	p.mu.Lock()
	if cr := p.creds[key]; cr != nil && cr.conns != nil {
		delete(cr.conns, c)
	}
	p.mu.Unlock()
}
