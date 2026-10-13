package sandbox

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// engineRetry is how long an engine that did not answer is taken as down
// before it is asked again, so a machine started later is found.
var engineRetry = 30 * time.Second

// probed is what one engine said of itself.
type probed struct {
	mu     sync.Mutex
	done   bool
	at     time.Time
	info   []byte
	err    error
	podman bool
}

var (
	probesMu sync.Mutex
	probes   = map[string]*probed{}
)

// resetEngineProbes forgets every engine's answer. For tests.
func resetEngineProbes() {
	probesMu.Lock()
	defer probesMu.Unlock()
	probes = map[string]*probed{}
}

// probeEngine is the engine at path's plain `info` and whether it is Podman,
// asked once per process: each call to a podman machine's CLI can cost the
// VM. A failure is asked again after engineRetry.
func probeEngine(path string) (info []byte, podman bool, err error) {
	probesMu.Lock()
	p := probes[path]
	if p == nil {
		p = &probed{}
		probes[path] = p
	}
	probesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done && (p.err == nil || time.Since(p.at) < engineRetry) {
		return p.info, p.podman, p.err
	}
	// Plain info: Podman's has no ServerVersion, so a format naming it failed there.
	p.info, p.err = engineProbe(path, "info")
	p.podman = p.err == nil && saysPodman(path)
	p.done, p.at = true, time.Now()
	return p.info, p.podman, p.err
}

// infoHasRunsc reports whether an engine's plain info names gVisor: Docker's
// Runtimes line, or Podman's OCI runtime.
func infoHasRunsc(info []byte) bool {
	lines := strings.Split(string(info), "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(t, "Runtimes:"); ok && slices.Contains(strings.Fields(rest), "runsc") {
			return true
		}
		if t != "ociRuntime:" {
			continue
		}
		for _, n := range lines[i+1:] {
			if v, ok := strings.CutPrefix(strings.TrimSpace(n), "name:"); ok {
				return strings.TrimSpace(v) == "runsc"
			}
		}
	}
	return false
}
