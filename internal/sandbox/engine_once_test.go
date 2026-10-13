//go:build unix

package sandbox

// resetEngineProbes forgets every engine's answer.
func resetEngineProbes() {
	probesMu.Lock()
	defer probesMu.Unlock()
	probes = map[string]*probed{}
}
