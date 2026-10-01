package agent

import (
	"sync"
	"testing"
	"time"
)

// slowOddStore takes longer over odd seqs, so a later seq would overtake an
// earlier one unless the recorder holds each write until it is appended.
type slowOddStore struct {
	*MemStore
	mu    sync.Mutex
	order []int64
}

func (s *slowOddStore) Append(ev Event) error {
	if ev.Seq%2 == 1 {
		time.Sleep(3 * time.Millisecond)
	}
	s.mu.Lock()
	s.order = append(s.order, ev.Seq)
	s.mu.Unlock()
	return s.MemStore.Append(ev)
}

// Concurrent records reach the store in seq order, so a reader never sees a
// gap that a slower, earlier event fills later.
func TestRecordAppendsInSeqOrder(t *testing.T) {
	st := &slowOddStore{MemStore: NewMemStore()}
	r := NewRecorder(st, "s-order", "")
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: "x"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, seq := range st.order {
		if seq != int64(i+1) {
			t.Fatalf("appended in order %v", st.order)
		}
	}
}
