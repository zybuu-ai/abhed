package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// What a session said is sealed with its own key, and deleting the session
// deletes the key: the rows stay, and nobody can read them again.
func TestDeleteErasesWhatTheSessionSaid(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	id := testID(t, "sess-erase-")
	if err := p.CreateSession(ctx, SessionRecord{ID: id, Tenant: "default", User: "tester", Workspace: "/w",
		Model: "m", Prompt: "the launch codes are 0000", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	said := "the launch codes are 0000"
	if err := p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, map[string]string{"text": said})); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveCheckpoint(ctx, id, 1, "/w/f", []byte(said)); err != nil {
		t.Fatal(err)
	}
	var raw, blob string
	if err := p.pool.QueryRow(ctx, `SELECT payload::text FROM events WHERE session_id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT encode(before, 'escape') FROM checkpoints WHERE session_id = $1`, id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "launch codes") || strings.Contains(blob, "launch codes") {
		t.Fatal("the payload is stored in the clear")
	}
	evs, err := p.Events(id)
	if err != nil || len(evs) != 1 || !strings.Contains(string(evs[0].Payload), said) {
		t.Fatalf("read back %v, %v", evs, err)
	}
	// Another replica, warmed by a write and a read before the delete.
	other := openStore(t, "default")
	if err := other.Append(ev(id, 2, agent.EvAgentMessage, agent.Trusted, map[string]string{"text": said})); err != nil {
		t.Fatal(err)
	}
	if evs, err := other.Events(id); err != nil || len(evs) != 2 || !strings.Contains(string(evs[0].Payload), said) {
		t.Fatalf("the other replica read back %v, %v", evs, err)
	}

	if err := p.DeleteSession(id); err != nil {
		t.Fatal(err)
	}
	var keys, rows int
	var prompt string
	_ = p.pool.QueryRow(ctx, `SELECT count(*) FROM session_keys WHERE session_id = $1`, id).Scan(&keys)
	_ = p.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1`, id).Scan(&rows)
	_ = p.pool.QueryRow(ctx, `SELECT prompt FROM sessions WHERE id = $1`, id).Scan(&prompt)
	if keys != 0 || rows != 2 || prompt != "" {
		t.Fatalf("after delete: %d keys, %d rows, prompt %q; want the key and label gone, the row kept", keys, rows, prompt)
	}
	// Neither a fresh store nor the warmed replica can open the payloads.
	for name, q := range map[string]*Postgres{"fresh": openStore(t, "default"), "warmed": other} {
		var seen []string
		if err := q.Scan(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), func(e agent.Event) error {
			if e.SessionID == id {
				seen = append(seen, string(e.Payload))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 2 || seen[0] != string(erasedPayload) || seen[1] != string(erasedPayload) {
			t.Fatalf("%s store: after delete the events read %q, want them erased", name, seen)
		}
		if evs, err := q.Events(id); err != nil || len(evs) != 0 {
			t.Fatalf("%s store: after delete Events gave %v, %v", name, evs, err)
		}
	}
	// The chain still covers the sealed rows.
	if r, err := other.VerifyChain(ctx, id); err != nil || !r.OK() || r.Rows != 2 {
		t.Fatalf("chain after erasure %+v, %v", r, err)
	}
}
