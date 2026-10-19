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
	for _, within := range []time.Duration{0, time.Minute} {
		ends, err := p.EventEnds(context.Background(), within)
		if err != nil || ends[id] != 3 {
			t.Fatalf("within %v: %d, %v", within, ends[id], err)
		}
	}
}
