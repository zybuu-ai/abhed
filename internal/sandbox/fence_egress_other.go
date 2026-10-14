//go:build !linux

package sandbox

// fenceEgress holds nothing off Linux, where nothing is fenced.
type fenceEgress struct{}

func (e *fenceEgress) close() error { return nil }
