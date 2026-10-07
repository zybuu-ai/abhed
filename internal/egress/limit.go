package egress

import (
	"fmt"
	"sync"
	"time"
)

// Bounds on what the record takes of repeated decisions.
const (
	defaultBurst    = 10
	defaultInterval = time.Minute
	// maxPerInterval bounds the denials recorded one by one in an interval,
	// whatever their kinds, so varying the target does not lift the limit.
	maxPerInterval = 100
	// defaultAllowBudget bounds the allowed decisions recorded one by one in an interval.
	defaultAllowBudget = 200
	// maxKinds bounds the kinds of denials, and apart those of allowed decisions, counted at once.
	maxKinds = 512
)

// limiter records the first decisions of each interval one by one and
// summarises the rest per kind, so a loop cannot grow the record without end.
type limiter struct {
	burst       int
	allowBudget int
	interval    time.Duration
	emit        func(Event)

	mu    sync.Mutex
	kinds map[string]*tally
	order []string
	// held counts kinds per side, so allowed traffic never takes a denial's room.
	heldAllow, heldDeny int
	total               int
	allowed             int
	done                chan struct{}
	ticked              chan struct{} // tests wait on a flush
	once                sync.Once
	wg                  sync.WaitGroup
}

type tally struct {
	recorded   int
	suppressed int64
	bytesIn    int64
	bytesOut   int64
	// sample is the kind's first decision, without what varies by call or
	// could be large (the path holds up to 64 KiB): the summary keeps none of it.
	sample Event
}

func newLimiter(burst, allowBudget int, interval time.Duration, emit func(Event)) *limiter {
	if burst <= 0 {
		burst = defaultBurst
	}
	if allowBudget <= 0 {
		allowBudget = defaultAllowBudget
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	l := &limiter{burst: burst, allowBudget: allowBudget, interval: interval, emit: emit, kinds: map[string]*tally{},
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

// kindKey is what makes two decisions the same kind: not the call id, path,
// method or reason, which a flood can vary at no cost.
func kindKey(e Event) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", e.Kind, e.Decision, e.Rule, e.Host, e.Port)
}

// tallyFor is e's kind's count, begun if need be; past maxKinds, a kind
// not yet counted shares its decision's overflow count. l.mu is held.
func (l *limiter) tallyFor(e Event) *tally {
	key := kindKey(e)
	if t := l.kinds[key]; t != nil {
		return t
	}
	s := e
	s.CallID, s.Method, s.Path, s.IP, s.BytesIn, s.BytesOut = "", "", "", "", 0, 0
	t := &tally{sample: s}
	held := &l.heldDeny
	if e.Decision == Allow {
		held = &l.heldAllow
	}
	if *held >= maxKinds {
		key = "overflow|" + string(e.Decision)
		if o := l.kinds[key]; o != nil {
			return o
		}
		what := "denials"
		if e.Decision == Allow {
			what = "allowed connections"
		}
		t.sample = Event{Kind: "summary", Decision: e.Decision, Rule: "rate", Reason: what + " of many kinds"}
		t.recorded = l.burst // never recorded one by one
	} else {
		*held++
	}
	l.kinds[key] = t
	l.order = append(l.order, key)
	return t
}

func (l *limiter) record(e Event) {
	l.mu.Lock()
	var t *tally
	var now bool
	if e.Decision == Allow {
		// Counted by kind only once over budget, in kinds of their own.
		if now = l.allowed < l.allowBudget; now {
			l.allowed++
		}
	} else {
		t = l.tallyFor(e)
		if now = t.recorded < l.burst && l.total < maxPerInterval; now {
			t.recorded++
			l.total++
		}
	}
	if !now {
		if t == nil {
			t = l.tallyFor(e)
		}
		t.suppressed++
		t.bytesIn += e.BytesIn
		t.bytesOut += e.BytesOut
	}
	l.mu.Unlock()
	if now {
		l.emit(e)
	}
}

// flush records a summary for each kind that had decisions left out, and
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
		s.Repeats, s.BytesIn, s.BytesOut = t.suppressed, t.bytesIn, t.bytesOut
		if s.Decision == Allow {
			s.Reason = fmt.Sprintf("%d more allowed within %s, over the %d recorded one by one", t.suppressed, l.interval, l.allowBudget)
		} else {
			s.Reason = fmt.Sprintf("%d more like this within %s, not recorded one by one; the first: %s", t.suppressed, l.interval, t.sample.Reason)
		}
		out = append(out, s)
	}
	l.kinds, l.order, l.total, l.allowed = map[string]*tally{}, nil, 0, 0
	l.heldAllow, l.heldDeny = 0, 0
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
