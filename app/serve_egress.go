package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/microvm"
	"github.com/zybuu-ai/abhed/driver/microvm/guest"
	"github.com/zybuu-ai/abhed/internal/egress"
)

// sandboxEgress is the control plane's egress proxies under the allowlist,
// one per sandbox, for the drivers that take theirs from the control plane
// rather than building it: the microvm driver. Each proxy serves the
// configured allowlist on a Unix socket in a folder of its own, and every
// decision goes to the control plane's record with the sandbox's id.
type sandboxEgress struct {
	policy *egress.Policy
	record driver.Recorder
	// start is egress.Start; tests replace it.
	start func(egress.Options) (*egress.Proxy, error)

	mu      sync.Mutex
	proxies map[string]*sandboxProxy
}

type sandboxProxy struct {
	proxy *egress.Proxy
	dir   string
	// dns is the sandbox's own credential for the proxy's resolver, which
	// the driver holds; the sandbox never sees it.
	dns *egress.Call
	// ca is the inspection CA bundle the guest is given, under inspect_tls.
	ca []byte
}

// newSandboxEgress returns the proxies for cfg, or nil when the allowlist
// is off: then a microvm sandbox gets no network path at all.
func newSandboxEgress(cfg config.Config, record driver.Recorder) (controlplane.Egress, error) {
	if cfg.Sandbox.Network != config.NetworkAllowlist {
		return nil, nil
	}
	pol, err := egress.Compile(cfg.Egress)
	if err != nil {
		return nil, fmt.Errorf("control_plane: the egress allowlist: %w", err)
	}
	return &sandboxEgress{policy: pol, record: record, start: egress.Start, proxies: map[string]*sandboxProxy{}}, nil
}

func (e *sandboxEgress) For(name string) bool { return name == microvm.Name }

func (e *sandboxEgress) Open(id string) (driver.Egress, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.proxies[id]; ok {
		return p.egress(), nil
	}
	route := func(ev egress.Event) {
		payload := ev.Payload()
		payload["sandbox_id"] = id
		if e.record == nil {
			slog.Warn("egress decision dropped: the control plane has no record", "sandbox_id", id, "host", ev.Host)
			return
		}
		_ = e.record(egress.EventName, payload)
	}
	px, err := e.start(egress.Options{Policy: e.policy, Record: route})
	if err != nil {
		return driver.Egress{}, err
	}
	// A short path: a Unix socket's name is limited to about a hundred bytes.
	dir, err := os.MkdirTemp("", "abhed-egress-")
	if err == nil {
		err = px.ListenUnix(filepath.Join(dir, "proxy.sock"))
	}
	if err == nil {
		// Lookups are judged by the same rules, under the sandbox's own credential.
		err = px.ListenDNS(filepath.Join(dir, "dns.sock"))
	}
	var dns *egress.Call
	if err == nil {
		dns, err = px.Issue(id + "/dns")
	}
	if err != nil {
		_ = px.Close()
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		return driver.Egress{}, err
	}
	p := &sandboxProxy{proxy: px, dir: dir, dns: dns}
	if px.Inspecting() {
		// The driver places the bundle in the guest, where the calls' variables point.
		px.SetCAPath(guest.CAFile)
		p.ca = px.CABundle()
	}
	e.proxies[id] = p
	return p.egress(), nil
}

func (p *sandboxProxy) egress() driver.Egress {
	return driver.Egress{Socket: filepath.Join(p.dir, "proxy.sock"), Port: p.proxy.Addr().Port(),
		DNSSocket: filepath.Join(p.dir, "dns.sock"), DNSCredential: egress.Credential(p.dns.URL()), CABundle: p.ca}
}

func (e *sandboxEgress) Call(id string) ([]string, func(), error) {
	e.mu.Lock()
	p, ok := e.proxies[id]
	e.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("sandbox %s has no egress proxy", id)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	c, err := p.proxy.Issue(id + "/" + hex.EncodeToString(b[:]))
	if err != nil {
		return nil, nil, err
	}
	return c.Env(), c.End, nil
}

func (e *sandboxEgress) Close(id string) error {
	e.mu.Lock()
	p, ok := e.proxies[id]
	delete(e.proxies, id)
	e.mu.Unlock()
	if !ok {
		return nil
	}
	return errors.Join(p.proxy.Close(), os.RemoveAll(p.dir))
}

// closeAll stops every proxy, as serve does on its way out.
func (e *sandboxEgress) closeAll() {
	e.mu.Lock()
	ids := make([]string, 0, len(e.proxies))
	for id := range e.proxies {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		_ = e.Close(id)
	}
}
