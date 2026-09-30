package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// A reader leaving while events are appended must not crash the writer:
// Unsubscribe closes the channel, and a send racing that close panics.
func TestUnsubscribeWhileAppending(t *testing.T) {
	m := NewMemStore()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= 2000; i++ {
			_ = m.Append(Event{ID: fmt.Sprint(i), SessionID: "s", Seq: i, Type: EvAgentMessage,
				Payload: json.RawMessage(`{}`)})
		}
	}()
	for range 500 {
		ch := m.Subscribe("s")
		m.Unsubscribe("s", ch)
	}
	wg.Wait()
}
