package store

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// slowAt delays the append of one seq, as a slow commit would.
type slowAt struct {
	*Postgres
	seq   int64
	delay time.Duration
}

func (s slowAt) Append(ev agent.Event) error {
	if ev.Seq == s.seq {
		time.Sleep(s.delay)
	}
	return s.Postgres.Append(ev)
}

// follow reads a session as the server's event stream does: live events in
// order, and a read back from the record on a gap, skipping what it has sent.
func follow(p *Postgres, id string, want int64, done <-chan struct{}) (<-chan []int64, func()) {
	events := p.Subscribe(id)
	out := make(chan []int64, 1)
	go func() {
		var got []int64
		var last int64
		send := func(batch []agent.Event) {
			for _, e := range batch {
				if e.Seq <= last {
					continue
				}
				last = e.Seq
				got = append(got, e.Seq)
			}
		}
		for last < want {
			select {
			case e := <-events:
				if e.Seq > last+1 {
					missed, _ := p.Since(id, last)
					send(missed)
				}
				send([]agent.Event{e})
			case <-done:
				out <- got
				return
			}
		}
		out <- got
	}()
	return out, func() { p.Unsubscribe(id, events) }
}

func wantInOrder(t *testing.T, got []int64, n int64) {
	t.Helper()
	if int64(len(got)) != n {
		t.Fatalf("delivered %d of %d events: %v", len(got), n, got)
	}
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("event %d delivered as seq %d: %v", i+1, s, got)
		}
	}
}

// A slow commit of seq 2 cannot let seq 3 commit first: an open stream that
// reads the record on a gap would skip seq 2 once it lands.
func TestSlowAppendIsNotOvertaken(t *testing.T) {
	p := openStore(t, "t-seq")
	id := fmt.Sprintf("seq-order-%d", time.Now().UnixNano())
	newSession(t, p, id, "t-seq")
	rec := agent.NewRecorder(slowAt{Postgres: p, seq: 2, delay: 300 * time.Millisecond}, id, "")
	done := make(chan struct{})
	got, stop := follow(p, id, 3, done)
	defer stop()

	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, map[string]string{"n": "1"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, map[string]string{"n": "2"})
	}()
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond) // after seq 2 is taken
		_, _ = rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, map[string]string{"n": "3"})
	}()
	wg.Wait()
	time.AfterFunc(2*time.Second, func() { close(done) })
	wantInOrder(t, <-got, 3)
}

// Parallel appends to one session commit in seq order: the record never holds
// a seq whose predecessor is missing, and a stream delivers every one in order.
func TestParallelAppendsCommitInSeqOrder(t *testing.T) {
	p := openStore(t, "t-seq")
	id := fmt.Sprintf("seq-race-%d", time.Now().UnixNano())
	newSession(t, p, id, "t-seq")
	rec := agent.NewRecorder(p, id, "")
	const writers, each = 8, 20
	done := make(chan struct{})
	got, stop := follow(p, id, writers*each, done)
	defer stop()

	stopPoll := make(chan struct{})
	polled := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stopPoll:
				polled <- nil
				return
			default:
			}
			all, err := p.Events(id)
			if err != nil {
				polled <- err
				return
			}
			for i, e := range all {
				if e.Seq != int64(i+1) {
					polled <- fmt.Errorf("the record holds seq %d at position %d", e.Seq, i+1)
					return
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, map[string]int{"i": i}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stopPoll)
	if err := <-polled; err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(5*time.Second, func() { close(done) })
	wantInOrder(t, <-got, writers*each)
}
