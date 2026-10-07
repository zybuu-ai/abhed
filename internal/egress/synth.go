package egress

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"time"
)

// SynthPrefix is the range the in-sandbox resolver answers from: benchmarking
// space, which is never routed, so an address from it means nothing but its name.
var SynthPrefix = netip.MustParsePrefix("198.18.0.0/15")

// Answer TTL and how long a mapping outlives its last answer.
const (
	// SynthTTL is the TTL of a synthetic answer.
	SynthTTL = 30 * time.Second
	// synthTTLSeconds is SynthTTL on the wire; a constant, so it cannot overflow.
	synthTTLSeconds = uint32(SynthTTL / time.Second)
	// synthHold keeps a mapping after its last answer for clients that cache past the TTL.
	synthHold = 5 * time.Minute
	// synthSize is how many addresses the pool hands out: the /15 less its first and last.
	synthSize = 1<<17 - 2
)

// synthPool maps allowed names to addresses from SynthPrefix and back, for one
// session; a mapping expires synthHold after the name was last answered.
type synthPool struct {
	mu     sync.Mutex
	byName map[string]*synthEntry
	byAddr map[netip.Addr]*synthEntry
	next   uint32 // offset of the next address to try, 1..size
	size   uint32
	hold   time.Duration
	now    func() time.Time
}

type synthEntry struct {
	name  string
	addr  netip.Addr
	until time.Time
}

func newSynthPool() *synthPool {
	return &synthPool{byName: map[string]*synthEntry{}, byAddr: map[netip.Addr]*synthEntry{},
		next: 1, size: synthSize, hold: synthHold, now: time.Now}
}

// assign is name's address, kept if it has one, or the next free one; fresh
// reports a new mapping. ok is false when every address is held.
func (p *synthPool) assign(name string) (addr netip.Addr, fresh, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if e := p.byName[name]; e != nil && now.Before(e.until) {
		e.until = now.Add(p.hold)
		return e.addr, false, true
	}
	if uint32(len(p.byAddr)) >= p.size { // #nosec G115 -- bounded by size
		p.sweep(now)
		if uint32(len(p.byAddr)) >= p.size { // #nosec G115 -- bounded by size
			return netip.Addr{}, false, false
		}
	}
	for range p.size {
		off := p.next
		p.next = p.next%p.size + 1
		a := offsetAddr(off)
		if e := p.byAddr[a]; e != nil {
			if now.Before(e.until) {
				continue
			}
			p.drop(e)
		}
		if old := p.byName[name]; old != nil {
			p.drop(old)
		}
		e := &synthEntry{name: name, addr: a, until: now.Add(p.hold)}
		p.byName[name], p.byAddr[a] = e, e
		return a, true, true
	}
	return netip.Addr{}, false, false
}

// lookup is the name a synthetic address was given for, while it is live.
func (p *synthPool) lookup(a netip.Addr) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byAddr[a.Unmap()]
	if e == nil {
		return "", false
	}
	if !p.now().Before(e.until) {
		p.drop(e)
		return "", false
	}
	return e.name, true
}

// sweep drops expired mappings; p.mu is held.
func (p *synthPool) sweep(now time.Time) {
	for _, e := range p.byAddr {
		if !now.Before(e.until) {
			p.drop(e)
		}
	}
}

func (p *synthPool) drop(e *synthEntry) {
	if p.byName[e.name] == e {
		delete(p.byName, e.name)
	}
	if p.byAddr[e.addr] == e {
		delete(p.byAddr, e.addr)
	}
}

// offsetAddr is the address off places after 198.18.0.0.
func offsetAddr(off uint32) netip.Addr {
	b := SynthPrefix.Addr().As4()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])+off)
	return netip.AddrFrom4(b)
}
