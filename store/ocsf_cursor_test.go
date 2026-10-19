package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// One server at a time holds an OCSF sink's writer lock; the cursors only
// move forward; and they are the tenant's alone.
func TestOCSFWriterLockAndCursors(t *testing.T) {
	a, b := openStore(t, "acme"), openStore(t, "acme")
	sink := fmt.Sprintf("sink-%d", time.Now().UnixNano())
	wa, wb := a.OCSFWriter(sink), b.OCSFWriter(sink)
	defer wa.Close()
	defer wb.Close()

	unlock, err := wa.Lock()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := wb.Lock(); !errors.Is(err, ErrOCSFStandby) {
		t.Fatalf("a second server took the writer's lock: %v", err)
	}
	// Held across calls, not only while one runs.
	if unlock, err := wa.Lock(); err != nil {
		t.Fatal(err)
	} else {
		unlock()
	}

	if _, begun, err := wa.Load(); err != nil || begun {
		t.Fatalf("a new sink: begun %v, %v", begun, err)
	}
	if err := wa.Save(nil); err != nil {
		t.Fatal(err)
	}
	if cur, begun, _ := wa.Load(); !begun || len(cur) != 0 {
		t.Fatalf("an empty save marks a begun sink with no cursors: %v %v", begun, cur)
	}
	if err := wa.Save(map[string]OCSFCursor{"s-1": {Seq: 5, Provider: "vllm", Primed: true}}); err != nil {
		t.Fatal(err)
	}
	if err := wa.Save(map[string]OCSFCursor{"s-1": {Seq: 3}}); err != nil {
		t.Fatal(err)
	}
	if cur, _, _ := wa.Load(); cur["s-1"].Seq != 5 {
		t.Fatalf("a cursor moved back: %+v", cur["s-1"])
	}

	// The writer goes; the other takes over and reads the same cursors.
	wa.Close()
	unlock, err = wb.Lock()
	if err != nil {
		t.Fatalf("the lock was not handed on: %v", err)
	}
	unlock()
	if cur, _, _ := wb.Load(); cur["s-1"].Seq != 5 {
		t.Fatalf("the next writer reads %+v", cur["s-1"])
	}
	other := openStore(t, "globex")
	if cur, begun, _ := other.OCSFWriter(sink).Load(); begun || len(cur) != 0 {
		t.Fatalf("another tenant read the cursors: %v", cur)
	}
}

// EventEnds lists each session's last seq, and lately written ones on asking.
func TestEventEnds(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-ends-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")
	for seq := int64(1); seq <= 3; seq++ {
		if err := p.Append(ev(id, seq, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "m"})); err != nil {
			t.Fatal(err)
		}
	}
	ends, err := p.EventEnds(context.Background(), nil)
	if err != nil || ends[id] != 3 {
		t.Fatalf("%d, %v", ends[id], err)
	}
	// A new reader starts at the last committed event; the place round-trips.
	end, err := p.CommitEnd(context.Background())
	if err != nil || end.Session == "" {
		t.Fatalf("commit end %+v, %v", end, err)
	}
	w := p.OCSFWriter(fmt.Sprintf("pos-%d", time.Now().UnixNano()))
	if err := w.SavePos(end.String()); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.LoadPos(); got != end.String() {
		t.Fatalf("saved %q, loaded %q", end.String(), got)
	}
	if _, begun, _ := w.Load(); !begun {
		t.Fatal("a saved place does not mark the sink begun")
	}
	// Up to the end, the ends are the same; up to before the session's first
	// event, it has none.
	if upTo, err := p.EventEnds(context.Background(), &end); err != nil || upTo[id] != 3 {
		t.Fatalf("up to the end: %d, %v", upTo[id], err)
	}
	if early, _ := p.EventEnds(context.Background(), &CommitPos{At: time.Unix(0, 0)}); early[id] != 0 {
		t.Fatalf("up to the start: %d", early[id])
	}
}
