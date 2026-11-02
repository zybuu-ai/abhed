package agent

// Ends is the last seq of each session the store holds.
func (m *MemStore) Ends() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int64, len(m.events))
	for id, evs := range m.events {
		// Appended in seq order, as the Recorder writes them.
		if len(evs) > 0 {
			out[id] = evs[len(evs)-1].Seq
		}
	}
	return out
}
