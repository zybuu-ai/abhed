package egress

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DNS message constants the resolver uses.
const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
	dnsTypeANY  = 255
	dnsClassIN  = 1

	rcodeOK       = 0
	rcodeFormErr  = 1
	rcodeServFail = 2
	rcodeNXDomain = 3
	rcodeNotImp   = 4
	rcodeRefused  = 5

	// maxDNSMessage bounds a query read from a command; real ones are far smaller.
	maxDNSMessage = 4096
	dnsTimeout    = 10 * time.Second
	// maxCredential bounds the credential frame: a call id and the token, encoded.
	maxCredential = 512
)

// dnsQuery is the one question of a query, with what a reply echoes.
type dnsQuery struct {
	id, flags     uint16
	name          string // lower case, no trailing dot; "" when a label is not plain
	qtype, qclass uint16
	question      []byte // the question section as sent
}

var errNotQuery = errors.New("not a DNS query")

// parseQuery reads a query's header and its one question. A message that is
// not a query is errNotQuery, and gets no reply; one that is malformed gets rcode.
func parseQuery(m []byte) (q dnsQuery, rcode int, err error) {
	if len(m) < 12 {
		return q, 0, errNotQuery
	}
	q.id, q.flags = binary.BigEndian.Uint16(m), binary.BigEndian.Uint16(m[2:])
	if q.flags&0x8000 != 0 {
		return q, 0, errNotQuery
	}
	if (q.flags>>11)&0xF != 0 {
		return q, rcodeNotImp, nil
	}
	if binary.BigEndian.Uint16(m[4:]) != 1 {
		return q, rcodeFormErr, nil
	}
	i, plain := 12, true
	var labels []string
	for {
		if i >= len(m) {
			return q, rcodeFormErr, nil
		}
		n := int(m[i])
		if n == 0 {
			i++
			break
		}
		// A compression pointer has no place in a question's name.
		// A name is at most 255 bytes on the wire, the root's zero byte included.
		if n > 63 || i+1+n > len(m) || i-12+1+n > 254 {
			return q, rcodeFormErr, nil
		}
		l := strings.ToLower(string(m[i+1 : i+1+n]))
		if strings.ContainsFunc(l, func(r rune) bool { return r <= 0x20 || r >= 0x7f || r == '.' }) {
			plain = false
		}
		labels = append(labels, l)
		i += 1 + n
	}
	if i+4 > len(m) {
		return q, rcodeFormErr, nil
	}
	q.qtype, q.qclass = binary.BigEndian.Uint16(m[i:]), binary.BigEndian.Uint16(m[i+2:])
	q.question = m[12 : i+4]
	if plain {
		q.name = strings.Join(labels, ".")
	}
	return q, rcodeOK, nil
}

// dnsReply is the reply to q with rcode and, for A or AAAA, the answers.
func dnsReply(q dnsQuery, rcode int, answers []netip.Addr, ttl uint32) []byte {
	flags := 0x8000 | q.flags&0x7900 | 0x0400 | 0x0080 | uint16(rcode&0xF) // #nosec G115 -- masked
	qd := uint16(0)
	if q.question != nil {
		qd = 1
	}
	out := binary.BigEndian.AppendUint16(nil, q.id)
	out = binary.BigEndian.AppendUint16(out, flags)
	out = binary.BigEndian.AppendUint16(out, qd)
	out = binary.BigEndian.AppendUint16(out, uint16(len(answers))) // #nosec G115 -- one or two
	out = append(out, 0, 0, 0, 0)
	out = append(out, q.question...)
	for _, a := range answers {
		typ := uint16(dnsTypeA)
		if a.Is6() {
			typ = dnsTypeAAAA
		}
		out = append(out, 0xC0, 12) // the name, as the question spells it
		out = binary.BigEndian.AppendUint16(out, typ)
		out = binary.BigEndian.AppendUint16(out, dnsClassIN)
		out = binary.BigEndian.AppendUint32(out, min(ttl, synthTTLSeconds))
		b := a.AsSlice()
		out = binary.BigEndian.AppendUint16(out, uint16(len(b))) // #nosec G115 -- 4 or 16
		out = append(out, b...)
	}
	return out
}

// answerDNS answers one query from a command of call callID, recording what
// it refuses; nil means no reply.
func (p *Proxy) answerDNS(callID string, m []byte) []byte {
	q, rc, err := parseQuery(m)
	if err != nil {
		return nil
	}
	if rc != rcodeOK {
		return dnsReply(q, rc, nil, 0)
	}
	ev := Event{CallID: callID, Kind: "dns", Host: q.name}
	refuse := func(rcode int, rule, why string) []byte {
		ev.Decision, ev.Rule, ev.Reason = Deny, rule, why
		p.record(ev)
		return dnsReply(q, rcode, nil, 0)
	}
	if q.name == "" {
		ev.Host = "(not a plain name)"
		return refuse(rcodeNXDomain, "dns", "the name has a label that is not plain")
	}
	if q.qclass != dnsClassIN {
		return refuse(rcodeRefused, "dns", "only class IN is answered")
	}
	// Loopback in the sandbox is its own, and no /etc/hosts is bound there.
	if q.name == "localhost" || strings.HasSuffix(q.name, ".localhost") {
		return dnsReply(q, rcodeOK, pick(q.qtype, netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()), synthTTLSeconds)
	}
	name, err := CanonicalHost(q.name)
	if err != nil || name != q.name {
		return refuse(rcodeNXDomain, "dns", "not a host name a rule could name")
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return refuse(rcodeNXDomain, "dns", "an address is not looked up")
	}
	v := p.opts.Policy.DecideName(name)
	ev.Decision, ev.Rule, ev.Reason = v.Decision, v.Rule, v.Reason
	if !v.Allowed() {
		return refuse(rcodeNXDomain, v.Rule, v.Reason)
	}
	pool := p.synth.Load()
	addr, fresh, ok := pool.assign(name)
	if !ok {
		return refuse(rcodeServFail, "dns", "every synthetic address of this session is in use")
	}
	if fresh || v.Decision != Allow {
		ev.IP = addr.String()
		p.record(ev)
	}
	return dnsReply(q, rcodeOK, pick(q.qtype, addr, netip.Addr{}), synthTTLSeconds)
}

// pick is the answer for qtype: v4 for A or ANY, v6 for AAAA, or none (no data).
func pick(qtype uint16, v4, v6 netip.Addr) []netip.Addr {
	switch {
	case qtype == dnsTypeA || qtype == dnsTypeANY:
		return []netip.Addr{v4}
	case qtype == dnsTypeAAAA && v6.IsValid():
		return []netip.Addr{v6}
	}
	return nil
}

// ListenDNS serves the session's resolver on a unix socket, for a relay
// inside a sandbox, and from then on maps its synthetic addresses back to names.
func (p *Proxy) ListenDNS(path string) error {
	p.synth.CompareAndSwap(nil, newSynthPool())
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	p.serve(ln, p.handleDNS)
	return nil
}

// handleDNS serves one exchange: the proxy credential, as Proxy-Authorization
// carries it, then a query, each length-prefixed; a wrong credential is refused.
func (p *Proxy) handleDNS(c net.Conn) {
	// The socket is reachable from the command too, so it gets its own bound.
	select {
	case p.dnsSlots <- struct{}{}:
		defer func() { <-p.dnsSlots }()
	default:
		p.record(Event{Kind: "dns", Decision: Deny, Rule: "cap",
			Reason: fmt.Sprintf("refused before it was read: %d lookups are in flight, the most one session's resolver serves", maxDNSInFlight)})
		return
	}
	_ = c.SetDeadline(time.Now().Add(dnsTimeout))
	cred, err := readFrame(c, maxCredential)
	if err != nil {
		return
	}
	m, err := readFrame(c, maxDNSMessage)
	if err != nil {
		return
	}
	callID, _, err := p.authorize(string(cred))
	if err != nil {
		p.record(Event{CallID: callID, Kind: "dns", Decision: Deny, Rule: "auth", Reason: err.Error()})
		if q, _, err := parseQuery(m); err == nil {
			_ = writeFrame(c, dnsReply(q, rcodeRefused, nil, 0))
		}
		return
	}
	if out := p.answerDNS(callID, m); out != nil {
		_ = writeFrame(c, out)
	}
}

// Credential reads the Proxy-Authorization value from a command's proxy
// variable, for the relay's lookups; "" if there is none.
func Credential(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.String()))
}

// readFrame reads a message with a two-byte length before it, as DNS over TCP sends.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(n[:]))
	if size > limit {
		return nil, fmt.Errorf("a %d-byte message is over %d", size, limit)
	}
	b := make([]byte, size)
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeFrame(w io.Writer, b []byte) error {
	if len(b) > 0xFFFF {
		return errors.New("message too long")
	}
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...)) // #nosec G115 -- checked
	return err
}

// exchangeDNS sends one query to the proxy's resolver socket and returns its reply.
func exchangeDNS(sock, cred string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("unix", sock, dnsTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(dnsTimeout))
	if err := writeFrame(c, []byte(cred)); err != nil {
		return nil, err
	}
	if err := writeFrame(c, q); err != nil {
		return nil, err
	}
	return readFrame(c, 0xFFFF)
}

// maxDNSInFlight bounds the queries the relay passes on at once.
const maxDNSInFlight = 64

// ServeDNS answers queries on udp and tcp inside a sandbox by passing each to
// the proxy's resolver socket, until both are closed.
func ServeDNS(udp net.PacketConn, tcp net.Listener, sock, cred string) {
	slots := make(chan struct{}, maxDNSInFlight)
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = c.Close()
				continue
			}
			go func() {
				defer func() { <-slots }()
				defer func() { _ = c.Close() }()
				for {
					_ = c.SetDeadline(time.Now().Add(dnsTimeout))
					q, err := readFrame(c, maxDNSMessage)
					if err != nil {
						return
					}
					out, err := exchangeDNS(sock, cred, q)
					if err != nil || writeFrame(c, out) != nil {
						return
					}
				}
			}()
		}
	}()
	buf := make([]byte, maxDNSMessage)
	for {
		n, from, err := udp.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		select {
		case slots <- struct{}{}:
		default:
			continue // over the bound: dropped, as a busy server would
		}
		go func() {
			defer func() { <-slots }()
			if out, err := exchangeDNS(sock, cred, q); err == nil {
				_, _ = udp.WriteTo(out, from)
			}
		}()
	}
}

// wildcardWarned keeps the wildcard warning to once per host per process.
var wildcardWarned sync.Map

// warnWildcard logs a wildcard allow rule's warning; tests replace it.
var warnWildcard = func(host string) {
	slog.Warn("egress: a wildcard allow rule lets a command put data in DNS labels under its domain, "+
		"which that domain's name servers see when the proxy resolves them", "host", host)
}

func noteWildcard(host string) {
	if _, seen := wildcardWarned.LoadOrStore(host, true); !seen {
		warnWildcard(host)
	}
}
