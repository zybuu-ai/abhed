package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store/chain"
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

	if _, begun, err := wa.Load(nil); err != nil || begun {
		t.Fatalf("a new sink: begun %v, %v", begun, err)
	}
	if err := wa.Save(nil); err != nil {
		t.Fatal(err)
	}
	if cur, begun, _ := wa.Load(nil); !begun || len(cur) != 0 {
		t.Fatalf("an empty save marks a begun sink with no cursors: %v %v", begun, cur)
	}
	if err := wa.Save(map[string]OCSFCursor{"s-1": {Seq: 5, Provider: "vllm", Primed: true}}); err != nil {
		t.Fatal(err)
	}
	if err := wa.Save(map[string]OCSFCursor{"s-1": {Seq: 3}}); err != nil {
		t.Fatal(err)
	}
	if cur, _, _ := wa.Load(nil); cur["s-1"].Seq != 5 {
		t.Fatalf("a cursor moved back: %+v", cur["s-1"])
	}
	if cur, begun, _ := wa.Load([]string{"s-1", "s-none"}); !begun || len(cur) != 1 || cur["s-1"].Seq != 5 {
		t.Fatalf("loading by name: %v %v", begun, cur)
	}
	if cur, begun, _ := wa.Load([]string{}); !begun || len(cur) != 0 {
		t.Fatalf("loading none: %v %v", begun, cur)
	}

	// The writer goes; the other takes over and reads the same cursors.
	wa.Close()
	unlock, err = wb.Lock()
	if err != nil {
		t.Fatalf("the lock was not handed on: %v", err)
	}
	unlock()
	if cur, _, _ := wb.Load(nil); cur["s-1"].Seq != 5 {
		t.Fatalf("the next writer reads %+v", cur["s-1"])
	}
	other := openStore(t, "globex")
	if cur, begun, _ := other.OCSFWriter(sink).Load(nil); begun || len(cur) != 0 {
		t.Fatalf("another tenant read the cursors: %v", cur)
	}
}

// A new OCSF reader starts at the last committed event, and the place it
// keeps round-trips.
func TestOCSFCommitPos(t *testing.T) {
	p := openStore(t, "acme")
	id := fmt.Sprintf("s-ends-%d", time.Now().UnixNano())
	newSession(t, p, id, "acme")
	for seq := int64(1); seq <= 3; seq++ {
		if err := p.Append(ev(id, seq, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "m"})); err != nil {
			t.Fatal(err)
		}
	}
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
	if _, begun, _ := w.Load(nil); !begun {
		t.Fatal("a saved place does not mark the sink begun")
	}
	if end.Session != id || end.Seq != 3 {
		t.Fatalf("the last committed event is %s/%d, want %s/3", end.Session, end.Seq, id)
	}
	// A writer that lost the sink and saves late cannot move the place back,
	// whichever part of the position is behind; a later place still moves it.
	old := p.OCSFWriter(w.sink)
	for _, back := range []CommitPos{{Tx: end.Tx - 1, At: end.At, Session: end.Session, Seq: end.Seq},
		{Tx: end.Tx, At: end.At, Session: end.Session, Seq: end.Seq - 1}, {}} {
		if err := old.SavePos(back.String()); err != nil {
			t.Fatal(err)
		}
		if got, _ := w.LoadPos(); got != end.String() {
			t.Fatalf("saving %q moved the place back to %q", back.String(), got)
		}
	}
	ahead := CommitPos{Tx: end.Tx, At: end.At, Session: end.Session, Seq: end.Seq + 1}
	if err := old.SavePos(ahead.String()); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.LoadPos(); got != ahead.String() {
		t.Fatalf("a later place was not saved: %q", got)
	}
	// A stored place that cannot be read counts as older: the next save replaces it.
	for _, bad := range []string{"garbage", "1/2", "99999999999999999999999/1/1/s", "99999999999999999999/1/1/s", "x/1/1/s"} {
		if _, err := p.pool.Exec(context.Background(), `UPDATE ocsf_cursors SET commit_pos = $2 WHERE sink = $1 AND session_id = ''`, w.sink, bad); err != nil {
			t.Fatal(err)
		}
		if err := w.SavePos(end.String()); err != nil {
			t.Fatalf("saving over %q: %v", bad, err)
		}
		if got, _ := w.LoadPos(); got != end.String() {
			t.Fatalf("an unreadable place %q was kept: %q", bad, got)
		}
	}
}

// The tenant's audit rows come back with their chain links, the last one's
// hash the head the chain reports, by row and by seq alike.
func TestAuditRowsAndLinks(t *testing.T) {
	tenant := fmt.Sprintf("audit-%d", time.Now().UnixNano())
	p := openStore(t, tenant)
	q, err := p.Sandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := q.Append(context.Background(), controlplane.AuditEvent{At: time.Now(), Principal: "alice",
			Action: "api:POST /v1/sessions", Outcome: "ok", Detail: map[string]string{"created.id": "s-1"}}); err != nil {
			t.Fatal(err)
		}
	}
	var seqs []int64
	var last chain.Link
	if err := p.AuditRows(context.Background(), func(a controlplane.AuditEvent, l chain.Link) error {
		if a.Detail["created.id"] != "s-1" {
			t.Errorf("row %d lost its detail: %v", a.Seq, a.Detail)
		}
		seqs, last = append(seqs, a.Seq), l
		return nil
	}); err != nil || len(seqs) != 3 {
		t.Fatalf("%d rows, %v", len(seqs), err)
	}
	head, err := p.AuditChain().Head(context.Background(), tenant)
	if err != nil || last.Pos != head.Pos || last.Hash != head.Hash {
		t.Fatalf("last %+v, head %+v, %v", last, head, err)
	}
	links, err := q.AuditLinks(context.Background(), seqs)
	if err != nil || links[seqs[2]].Hash != head.Hash || links[seqs[1]].Hash != links[seqs[2]].Prev {
		t.Fatalf("%v %v", links, err)
	}
}
