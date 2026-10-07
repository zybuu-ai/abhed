package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// query builds a DNS query for name and qtype.
func query(id uint16, name string, qtype uint16) []byte {
	m := binary.BigEndian.AppendUint16(nil, id)
	m = append(m, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0) // RD, one question
	for _, l := range strings.Split(name, ".") {
		m = append(m, byte(len(l)))
		m = append(m, l...)
	}
	m = append(m, 0)
	m = binary.BigEndian.AppendUint16(m, qtype)
	return binary.BigEndian.AppendUint16(m, dnsClassIN)
}

// reply is what a test reads back from an answer.
type reply struct {
	id      uint16
	rcode   int
	answers []netip.Addr
	ttls    []uint32
}

func parseReply(t *testing.T, m []byte) reply {
	t.Helper()
	if len(m) < 12 || m[2]&0x80 == 0 {
		t.Fatalf("not a reply: %x", m)
	}
	r := reply{id: binary.BigEndian.Uint16(m), rcode: int(m[3] & 0xF)}
	qd, an := binary.BigEndian.Uint16(m[4:]), binary.BigEndian.Uint16(m[6:])
	i := 12
	for range qd {
		for m[i] != 0 {
			i += int(m[i]) + 1
		}
		i += 5
	}
	for range an {
		i += 2 // the pointer
		typ := binary.BigEndian.Uint16(m[i:])
		ttl := binary.BigEndian.Uint32(m[i+4:])
		n := int(binary.BigEndian.Uint16(m[i+8:]))
		a, _ := netip.AddrFromSlice(m[i+10 : i+10+n])
		if (typ == dnsTypeA) != a.Is4() {
			t.Fatalf("type %d with %s", typ, a)
		}
		r.answers, r.ttls = append(r.answers, a), append(r.ttls, ttl)
		i += 10 + n
	}
	return r
}

func dnsProxy(t *testing.T, c Config, o Options) (*Proxy, *recorder, string) {
	t.Helper()
	rec := &recorder{}
	o.Policy, o.Record = mustCompile(t, c), rec.add
	if o.Resolve == nil {
		o.Resolve = fakeDNS
	}
	o.DialTimeout = 2 * time.Second
	p, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	sock := filepath.Join(shortTemp(t), "dns.sock")
	if err := p.ListenDNS(sock); err != nil {
		t.Fatal(err)
	}
	return p, rec, sock
}

// shortTemp is a folder whose path fits a unix socket's on macOS.
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "egdns")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// waitKind is the first event of kind for host, waiting for it a short while.
func (r *recorder) waitKind(t *testing.T, host, kind string) Event {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		r.mu.Lock()
		for _, e := range r.evs {
			if e.Host == host && e.Kind == kind {
				r.mu.Unlock()
				return e
			}
		}
		r.mu.Unlock()
	}
	t.Fatalf("no %s event for %s", kind, host)
	return Event{}
}

func (r *recorder) dns() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.evs {
		if e.Kind == "dns" {
			out = append(out, e)
		}
	}
	return out
}

// The resolver answers only names an allow rule could match, each with a
// synthetic address and a short TTL; every other name is NXDOMAIN, recorded.
func TestResolverAllowAndDeny(t *testing.T) {
	p, rec, _ := dnsProxy(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{8443}, Decision: "allow"},
		{Host: "*.wild.test", Decision: "allow"},
		{Host: "denied.test", Decision: "deny"},
	}}, Options{})
	ask := func(name string, qtype uint16) reply {
		t.Helper()
		return parseReply(t, p.answerDNS("call-d", query(7, name, qtype)))
	}
	r := ask("Allowed.Test", dnsTypeA)
	if r.id != 7 || r.rcode != rcodeOK || len(r.answers) != 1 || !SynthPrefix.Contains(r.answers[0]) {
		t.Fatalf("allowed.test: %+v", r)
	}
	if r.ttls[0] == 0 || r.ttls[0] > 30 {
		t.Fatalf("TTL %d, want 1 to 30", r.ttls[0])
	}
	if again := ask("allowed.test", dnsTypeA); again.answers[0] != r.answers[0] {
		t.Fatalf("allowed.test moved from %s to %s", r.answers[0], again.answers[0])
	}
	if r6 := ask("allowed.test", dnsTypeAAAA); r6.rcode != rcodeOK || len(r6.answers) != 0 {
		t.Fatalf("AAAA of an allowed name should be no data: %+v", r6)
	}
	if rw := ask("a.b.wild.test", dnsTypeA); rw.rcode != rcodeOK || len(rw.answers) != 1 {
		t.Fatalf("a.b.wild.test: %+v", rw)
	}
	for _, n := range []string{"other.test", "wild.test", "denied.test", "allowed.test.evil.net"} {
		if r := ask(n, dnsTypeA); r.rcode != rcodeNXDomain || len(r.answers) != 0 {
			t.Errorf("%s: %+v, want NXDOMAIN", n, r)
		}
	}
	if lo := ask("localhost", dnsTypeA); len(lo.answers) != 1 || !lo.answers[0].IsLoopback() {
		t.Fatalf("localhost: %+v", lo)
	}
	got := map[string]Event{}
	for _, e := range rec.dns() {
		got[e.Host] = e
		if e.CallID != "call-d" {
			t.Errorf("event without the call id: %+v", e)
		}
	}
	if e := got["other.test"]; e.Decision != Deny || e.Rule != "default" {
		t.Fatalf("other.test recorded as %+v", e)
	}
	if e := got["allowed.test"]; e.Decision != Allow || e.IP != r.answers[0].String() {
		t.Fatalf("allowed.test recorded as %+v", e)
	}
	if _, ok := got["localhost"]; ok {
		t.Fatal("localhost was recorded")
	}
	n := 0
	for _, e := range rec.dns() {
		if e.Host == "allowed.test" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("allowed.test recorded %d times; a mapping is recorded once", n)
	}
}

// Malformed and unusual queries are refused, never answered with an address.
func TestResolverRefusesOddQueries(t *testing.T) {
	p, _, _ := dnsProxy(t, Config{Default: "allow"}, Options{})
	ok := query(1, "a.test", dnsTypeA)
	cases := map[string]struct {
		m     []byte
		rcode int
	}{
		"opcode":   {func() []byte { m := append([]byte(nil), ok...); m[2] |= 0x10; return m }(), rcodeNotImp},
		"two qs":   {func() []byte { m := append([]byte(nil), ok...); m[5] = 2; return m }(), rcodeFormErr},
		"cut":      {ok[:len(ok)-3], rcodeFormErr},
		"pointer":  {append(append([]byte(nil), ok[:12]...), 0xC0, 12, 0, 1, 0, 1), rcodeFormErr},
		"class CH": {func() []byte { m := append([]byte(nil), ok...); m[len(m)-1] = 3; return m }(), rcodeRefused},
		"dot":      {append(append([]byte(nil), ok[:12]...), 3, 'a', '.', 'b', 0, 0, 1, 0, 1), rcodeNXDomain},
		"address":  {query(1, "10.0.0.1", dnsTypeA), rcodeNXDomain},
	}
	for name, c := range cases {
		r := parseReply(t, p.answerDNS("c", c.m))
		if r.rcode != c.rcode || len(r.answers) != 0 {
			t.Errorf("%s: rcode %d answers %v, want %d", name, r.rcode, r.answers, c.rcode)
		}
	}
	resp := append([]byte(nil), ok...)
	resp[2] |= 0x80
	if out := p.answerDNS("c", resp); out != nil {
		t.Fatal("a reply was answered")
	}
	if out := p.answerDNS("c", ok[:5]); out != nil {
		t.Fatal("a fragment was answered")
	}
	// Default allow answers any plain name.
	if r := parseReply(t, p.answerDNS("c", ok)); len(r.answers) != 1 {
		t.Fatalf("default allow: %+v", r)
	}
}

// Audit mode answers a name the rules deny and records it as would_deny.
func TestResolverAuditMode(t *testing.T) {
	p, rec, _ := dnsProxy(t, Config{Mode: "audit"}, Options{})
	if r := parseReply(t, p.answerDNS("c", query(1, "other.test", dnsTypeA))); len(r.answers) != 1 {
		t.Fatalf("audit: %+v", r)
	}
	if e := rec.dns(); len(e) != 1 || e[0].Decision != WouldDeny {
		t.Fatalf("audit records: %+v", e)
	}
}

// Denied lookups are rate-limited like other denials: a burst one by one,
// the rest in one summary when the proxy stops.
func TestResolverDenialsAreRateLimited(t *testing.T) {
	p, rec, _ := dnsProxy(t, Config{}, Options{Burst: 2, Interval: time.Hour})
	for range 10 {
		p.answerDNS("c", query(1, "loop.test", dnsTypeA))
	}
	// A loop varying the name is bounded by the interval's total.
	for i := range maxPerInterval + 50 {
		p.answerDNS("c", query(1, fmt.Sprintf("x%d.exfil.test", i), dnsTypeA))
	}
	_ = p.Close()
	one, total := 0, 0
	var sum Event
	for _, e := range rec.dns() {
		switch {
		case e.Host == "loop.test" && e.Repeats == 0:
			one++
		case e.Host == "loop.test":
			sum = e
		}
		if e.Repeats == 0 {
			total++
		}
	}
	if one != 2 || sum.Repeats != 8 {
		t.Fatalf("loop.test: %d one by one and a summary of %d, want 2 and 8", one, sum.Repeats)
	}
	if total > maxPerInterval {
		t.Fatalf("%d denials recorded one by one in an interval, over %d", total, maxPerInterval)
	}
}

// The resolver is reached over its socket, the call id travelling with the
// query, and the relay's UDP and TCP listeners pass queries on.
func TestResolverOverTheRelay(t *testing.T) {
	p, rec, sock := dnsProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Decision: "allow"}}}, Options{})
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udp.Close(); _ = tcp.Close() }()
	go ServeDNS(udp, tcp, sock, Credential(p.URL("call-r")))

	c, err := net.Dial("udp", udp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(query(9, "allowed.test", dnsTypeA)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if r := parseReply(t, buf[:n]); r.id != 9 || len(r.answers) != 1 {
		t.Fatalf("over UDP: %+v", r)
	}

	tc, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tc.Close() }()
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	for _, name := range []string{"allowed.test", "other.test"} {
		if err := writeFrame(tc, query(3, name, dnsTypeA)); err != nil {
			t.Fatal(err)
		}
		m, err := readFrame(tc, 0xFFFF)
		if err != nil {
			t.Fatal(err)
		}
		r := parseReply(t, m)
		if want := map[string]int{"allowed.test": rcodeOK, "other.test": rcodeNXDomain}[name]; r.rcode != want {
			t.Fatalf("%s over TCP: %+v", name, r)
		}
	}
	e := rec.wait(t, "other.test")
	if e.CallID != "call-r" || e.Kind != "dns" || e.Decision != Deny {
		t.Fatalf("recorded %+v", e)
	}
}

// synthOf asks p's resolver for name's synthetic address.
func synthOf(t *testing.T, p *Proxy, name string) netip.Addr {
	t.Helper()
	r := parseReply(t, p.answerDNS("c", query(1, name, dnsTypeA)))
	if len(r.answers) != 1 {
		t.Fatalf("%s not answered: %+v", name, r)
	}
	return r.answers[0]
}

// rawRequest sends one request line through the proxy and returns the status line.
func rawRequest(t *testing.T, p *Proxy, line string) string {
	t.Helper()
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("call-s:"+p.Token()))
	fmt.Fprintf(c, "%s\r\nHost: x\r\nProxy-Authorization: %s\r\n\r\n", line, auth)
	st, _ := bufio.NewReader(c).ReadString('\n')
	return st
}

// A CONNECT or plain request to a synthetic address is judged and dialled
// as the name the resolver gave it for; an address it never gave is refused.
func TestProxyMapsSyntheticAddresses(t *testing.T) {
	var sawHost atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHost.Store(r.Host)
		_, _ = io.WriteString(w, "via name")
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p, rec, _ := dnsProxy(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "denied.test", Ports: []int{1}, Decision: "allow"},
	}}, Options{})
	syn := synthOf(t, p, "allowed.test")

	c := client(t, p.URL("call-s"), nil)
	resp, err := c.Get(fmt.Sprintf("http://%s:%d/x", syn, port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "via name" {
		t.Fatalf("plain via %s: %d %q", syn, resp.StatusCode, body)
	}
	if h, _ := sawHost.Load().(string); h != fmt.Sprintf("allowed.test:%d", port) {
		t.Fatalf("the origin saw Host %q, want the name", h)
	}
	e := rec.waitKind(t, "allowed.test", "http")
	if e.Synthetic != syn.String() || e.IP != "127.0.0.1" {
		t.Fatalf("recorded %+v", e)
	}
	if st := rawRequest(t, p, fmt.Sprintf("CONNECT %s:%d HTTP/1.1", syn, port)); !strings.Contains(st, " 200 ") {
		t.Fatalf("CONNECT via %s: %s", syn, st)
	}
	// Judged on the name: denied.test is allowed only on port 1.
	den := synthOf(t, p, "denied.test")
	if st := rawRequest(t, p, fmt.Sprintf("CONNECT %s:%d HTTP/1.1", den, port)); !strings.Contains(st, " 403 ") {
		t.Fatalf("CONNECT to denied.test's address on another port: %s", st)
	}
	if st := rawRequest(t, p, fmt.Sprintf("CONNECT 198.19.0.77:%d HTTP/1.1", port)); !strings.Contains(st, " 403 ") {
		t.Fatalf("CONNECT to an address never given out: %s", st)
	}
	if e := rec.wait(t, "198.19.0.77"); e.Rule != "dns" || e.Decision != Deny {
		t.Fatalf("unmapped address recorded as %+v", e)
	}
}

// Without the resolver a synthetic address is only a reserved address.
func TestProxyWithoutResolverRefusesSyntheticRange(t *testing.T) {
	p, _ := startProxy(t, Config{Default: "allow"})
	if st := rawRequest(t, p, "CONNECT 198.18.0.1:443 HTTP/1.1"); !strings.Contains(st, " 403 ") {
		t.Fatalf("CONNECT 198.18.0.1 without the resolver: %s", st)
	}
}

// A synthetic address's name is resolved once per connection and the checked
// address dialled, so a name rebinding to an internal address is refused.
func TestSyntheticAddressIsPinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	var lookups atomic.Int32
	rebind := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "rebind.test" {
			return nil, fmt.Errorf("no such host %s", host)
		}
		if lookups.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
	}
	p, rec, _ := dnsProxy(t, Config{Rules: []Rule{
		{Host: "rebind.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}}, Options{Resolve: rebind})
	syn := synthOf(t, p, "rebind.test")
	if lookups.Load() != 0 {
		t.Fatal("the resolver looked the name up; only the proxy does, when it dials")
	}
	if st := rawRequest(t, p, fmt.Sprintf("CONNECT %s:%d HTTP/1.1", syn, port)); !strings.Contains(st, " 200 ") {
		t.Fatalf("first CONNECT: %s", st)
	}
	if n := lookups.Load(); n != 1 {
		t.Fatalf("%d lookups for one connection", n)
	}
	if st := rawRequest(t, p, fmt.Sprintf("CONNECT %s:%d HTTP/1.1", syn, port)); !strings.Contains(st, " 403 ") {
		t.Fatalf("CONNECT after the name rebound to 10.1.2.3: %s", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		found := false
		rec.mu.Lock()
		for _, e := range rec.evs {
			if e.Host == "rebind.test" && e.Decision == Deny && e.IP == "10.1.2.3" && e.Synthetic == syn.String() {
				found = true
			}
		}
		rec.mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no refusal recorded for the rebound address: %+v", rec.evs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A wildcard allow rule warns once, whatever compiles it; exact and deny rules do not.
func TestWildcardWarnsOnce(t *testing.T) {
	var warned []string
	old := warnWildcard
	warnWildcard = func(h string) { warned = append(warned, h) }
	t.Cleanup(func() { warnWildcard = old })
	c := Config{Rules: []Rule{
		{Host: "*.warn-once.test", Decision: "allow"},
		{Host: "exact.warn-once.test", Decision: "allow"},
		{Host: "*.deny-wild.test", Decision: "deny"},
	}}
	mustCompile(t, c)
	mustCompile(t, c)
	if len(warned) != 1 || warned[0] != "*.warn-once.test" {
		t.Fatalf("warned %v", warned)
	}
}

// A frame without the session's credential, as a command dialling dns.sock
// itself would send, is refused and recorded; the real one is answered.
func TestResolverSocketNeedsTheCredential(t *testing.T) {
	p, rec, sock := dnsProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Decision: "allow"}}}, Options{})
	forged := "Basic " + base64.StdEncoding.EncodeToString([]byte("call-x:not-the-token"))
	for _, cred := range []string{"call-x", forged, ""} {
		out, err := exchangeDNS(sock, cred, query(5, "allowed.test", dnsTypeA))
		if err != nil {
			t.Fatal(err)
		}
		if r := parseReply(t, out); r.rcode != rcodeRefused || len(r.answers) != 0 {
			t.Fatalf("credential %q answered: %+v", cred, r)
		}
	}
	if e := rec.wait(t, ""); e.Kind != "dns" || e.Rule != "auth" || e.Decision != Deny {
		t.Fatalf("forged frame recorded as %+v", e)
	}
	out, err := exchangeDNS(sock, Credential(p.URL("call-ok")), query(5, "allowed.test", dnsTypeA))
	if err != nil {
		t.Fatal(err)
	}
	if r := parseReply(t, out); len(r.answers) != 1 {
		t.Fatalf("the real credential: %+v", r)
	}
	if e := rec.wait(t, "allowed.test"); e.CallID != "call-ok" {
		t.Fatalf("recorded under %q", e.CallID)
	}
}

// Connections straight to dns.sock are bounded: past the bound one is closed unread and recorded.
func TestResolverSocketIsBounded(t *testing.T) {
	p, rec, sock := dnsProxy(t, Config{}, Options{})
	var held []net.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for range maxDNSInFlight {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	for deadline := time.Now().Add(5 * time.Second); len(p.dnsSlots) < maxDNSInFlight; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d held", len(p.dnsSlots), maxDNSInFlight)
		}
	}
	if _, err := exchangeDNS(sock, Credential(p.URL("c")), query(1, "a.test", dnsTypeA)); err == nil {
		t.Fatal("a lookup past the bound was answered")
	}
	if e := rec.wait(t, ""); e.Kind != "dns" || e.Rule != "cap" {
		t.Fatalf("recorded %+v", e)
	}
}

// A name is at most 255 bytes on the wire with its root: one byte more is malformed.
func TestResolverNameLength(t *testing.T) {
	p, _, _ := dnsProxy(t, Config{Default: "allow"}, Options{})
	l63 := strings.Repeat("a", 63)
	fits := strings.Join([]string{l63, l63, l63, strings.Repeat("b", 61)}, ".") // 255 on the wire
	over := strings.Join([]string{l63, l63, l63, strings.Repeat("b", 62)}, ".")
	if r := parseReply(t, p.answerDNS("c", query(1, fits, dnsTypeA))); r.rcode == rcodeFormErr {
		t.Fatalf("a %d-character name was malformed", len(fits))
	}
	if r := parseReply(t, p.answerDNS("c", query(1, over, dnsTypeA))); r.rcode != rcodeFormErr {
		t.Fatalf("a %d-character name: rcode %d, want FORMERR", len(over), r.rcode)
	}
}
