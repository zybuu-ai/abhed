package egress

import (
	"fmt"
	"sync"
	"time"
)

// Bounds on what the record takes of repeated denials.
const (
	defaultBurst    = 10
	defaultInterval = time.Minute
	// maxPerInterval bounds the denials recorded one by one in an interval,
	// whatever their kinds, so varying the target does not lift the limit.
	maxPerInterval = 100
	// maxKinds bounds the kinds counted at once; the rest share one summary.
	maxKinds = 512
)

// limiter records allowed decisions as they come and rate-limits denials:
// the first burst of a kind in each interval one by one, the rest counted
// and recorded as one summary per kind when the interval ends.
type limiter struct {
	burst    int
	interval time.Duration
	emit     func(Event)

	mu     sync.Mutex
	kinds  map[string]*tally
	order  []string
	total  int
	done   chan struct{}
	ticked chan struct{} // tests wait on a flush
	once   sync.Once
	wg     sync.WaitGroup
}

type tally struct {
	recorded   int
	suppressed int64
	sample     Event
}

func newLimiter(burst int, interval time.Duration, emit func(Event)) *limiter {
	if burst <= 0 {
		burst = defaultBurst
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	l := &limiter{burst: burst, interval: interval, emit: emit, kinds: map[string]*tally{},
		done: make(chan struct{}), ticked: make(chan struct{}, 1)}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-l.done:
				return
			case <-t.C:
				l.flush()
				select {
				case l.ticked <- struct{}{}:
				default:
				}
			}
		}
	}()
	return l
}

// kindKey is what makes two denials the same kind: not the call id, path,
// method or reason, which a flood can vary at no cost.
func kindKey(e Event) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", e.Kind, e.Decision, e.Rule, e.Host, e.Port)
}

func (l *limiter) record(e Event) {
	if e.Decision == Allow {
		l.emit(e)
		return
	}
	key := kindKey(e)
	l.mu.Lock()
	t := l.kinds[key]
	if t == nil {
		if len(l.kinds) >= maxKinds {
			key = "overflow"
			t = l.kinds[key]
		}
		if t == nil {
			t = &tally{sample: e}
			if key == "overflow" {
				t.sample = Event{Kind: "summary", Decision: Deny, Rule: "rate", Reason: "denials of many kinds"}
				t.recorded = l.burst // never recorded one by one
			}
			l.kinds[key] = t
			l.order = append(l.order, key)
		}
	}
	now := t.recorded < l.burst && l.total < maxPerInterval
	if now {
		t.recorded++
		l.total++
	} else {
		t.suppressed++
	}
	l.mu.Unlock()
	if now {
		l.emit(e)
	}
}

// flush records a summary for each kind that had denials left out, and
// starts a new interval.
func (l *limiter) flush() {
	l.mu.Lock()
	var out []Event
	for _, k := range l.order {
		t := l.kinds[k]
		if t.suppressed == 0 {
			continue
		}
		s := t.sample
		s.CallID, s.Method, s.Path, s.IP, s.BytesIn, s.BytesOut = "", "", "", "", 0, 0
		s.Repeats = t.suppressed
		s.Reason = fmt.Sprintf("%d more like this within %s, not recorded one by one; the first: %s", t.suppressed, l.interval, t.sample.Reason)
		out = append(out, s)
	}
	l.kinds, l.order, l.total = map[string]*tally{}, nil, 0
	l.mu.Unlock()
	for _, e := range out {
		l.emit(e)
	}
}

// stop ends the ticker and records what is still counted.
func (l *limiter) stop() {
	l.once.Do(func() {
		close(l.done)
		l.wg.Wait()
		l.flush()
	})
}
