package recordship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// fakeOrg is an org server in memory: it checks each batch as the
// Enterprise server does and keeps the lines.
type fakeOrg struct {
	mu      sync.Mutex
	lines   map[string][][]byte
	events  []Event
	offline bool
	// loseAck keeps a batch but answers as if the network failed.
	loseAck bool
	calls   int
}

func newFakeOrg() *fakeOrg { return &fakeOrg{lines: map[string][][]byte{}} }

func (f *fakeOrg) head(id string) (int64, string) {
	ls := f.lines[id]
	if len(ls) == 0 {
		return 0, local.Genesis
	}
	segs, _ := local.CheckSegment(ls[len(ls)-1:], id, prevOf(ls[len(ls)-1]))
	return int64(len(ls)), segs[0].Hash
}

func prevOf(raw []byte) string {
	var l struct{ Prev string }
	_ = json.Unmarshal(raw, &l)
	return l.Prev
}

func (f *fakeOrg) do(_ context.Context, method, path, ctype string, body []byte) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.offline {
		return 0, nil, errors.New("connection refused")
	}
	if path == EventsPath {
		var in struct{ Events []Event }
		_ = json.Unmarshal(body, &in)
		f.events = append(f.events, in.Events...)
		return http.StatusOK, nil, nil
	}
	if method != http.MethodPost || ctype != ContentType || !strings.HasSuffix(path, "/lines") {
		return http.StatusNotFound, nil, nil
	}
	h, raws, err := DecodeBatch(bytes.NewReader(body), MaxBatchBytes)
	if err != nil {
		return http.StatusBadRequest, mustJSON(Ack{Error: err.Error()}), nil
	}
	n, hash := f.head(h.Session)
	if h.From != n || h.Before != hash {
		return http.StatusConflict, mustJSON(Ack{Lines: n, Hash: hash, Error: "not where the server is"}), nil
	}
	segs, err := local.CheckSegment(raws, h.Session, h.Before)
	if err != nil {
		return http.StatusUnprocessableEntity, mustJSON(Ack{Lines: n, Hash: hash, Error: err.Error()}), nil
	}
	f.lines[h.Session] = append(f.lines[h.Session], raws...)
	if f.loseAck {
		return 0, nil, errors.New("connection reset")
	}
	return http.StatusOK, mustJSON(Ack{Lines: n + int64(len(raws)), Hash: segs[len(segs)-1].Hash}), nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type redactor struct{}

func (redactor) Redact(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("SECRET-VALUE"), []byte("[redacted]"))
}
func (redactor) Span() int { return 12 }

func openStore(t *testing.T) *local.Store {
	t.Helper()
	s, err := local.Open(local.Options{Dir: t.TempDir(), User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func session(t *testing.T, s *local.Store, id string, texts ...string) *agent.Recorder {
	t.Helper()
	if err := s.CreateSession(context.Background(), store.SessionRecord{ID: id, User: "alice", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecorder(s, id, "")
	for _, text := range texts {
		if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Sync(id)
	return rec
}

func newShipper(t *testing.T, s *local.Store, org *fakeOrg, statePath string, mut ...func(*Options)) *Shipper {
	t.Helper()
	o := Options{Store: s, Do: org.do, URL: "https://org.example", StatePath: statePath, Redact: redactor{}, Host: "laptop"}
	for _, m := range mut {
		m(&o)
	}
	sh, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func shippedEqualsLocal(t *testing.T, s *local.Store, org *fakeOrg, id string) {
	t.Helper()
	lines, _, err := s.LinesFrom(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	org.mu.Lock()
	got := org.lines[id]
	org.mu.Unlock()
	if len(got) != len(lines) {
		t.Fatalf("session %s: the server holds %d lines, the record %d", id, len(got), len(lines))
	}
	for i := range lines {
		if !bytes.Equal(got[i], lines[i].Raw) {
			t.Fatalf("session %s line %d differs", id, i+1)
		}
	}
	// What the server holds verifies as an export does.
	var b bytes.Buffer
	if _, err := local.WriteExport(&b, id, got); err != nil {
		t.Fatal(err)
	}
}

func TestShipsEachSessionAndGoesOnFromWhereItStopped(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	state := filepath.Join(t.TempDir(), "ship.json")
	sh := newShipper(t, s, org, state)
	rec := session(t, s, "s-1", "one", "two")
	session(t, s, "s-2", "three")
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	shippedEqualsLocal(t, s, org, "s-1")
	shippedEqualsLocal(t, s, org, "s-2")
	// More is written; a pass sends only what is new.
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "four"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Sync("s-1")
	before := org.calls
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	shippedEqualsLocal(t, s, org, "s-1")
	if org.calls-before != 1 {
		t.Errorf("a pass with one session grown made %d calls", org.calls-before)
	}
	// A new process goes on from the state file.
	sh2 := newShipper(t, s, org, state)
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "five"}); err != nil {
		t.Fatal(err)
	}
	if err := sh2.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	shippedEqualsLocal(t, s, org, "s-1")
}

func TestOfflineThenResumes(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	var events []Event
	sh := newShipper(t, s, org, filepath.Join(t.TempDir(), "ship.json"), func(o *Options) {
		o.OnEvent = func(e Event) { events = append(events, e) }
	})
	session(t, s, "s-1", "one", "two")
	org.offline = true
	if err := sh.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be reached") {
		t.Fatalf("offline pass: %v", err)
	}
	if st := sh.Status(); st.Backlog == 0 || st.LastError == "" {
		t.Errorf("status while offline: %+v", st)
	}
	org.offline = false
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	shippedEqualsLocal(t, s, org, "s-1")
	if len(events) == 0 || events[len(events)-1].Kind != KindResumed {
		t.Errorf("no resumed event: %+v", events)
	}
	if len(org.events) == 0 || org.events[len(org.events)-1].Kind != KindResumed {
		t.Errorf("the server was not told: %+v", org.events)
	}
	if st := sh.Status(); st.Backlog != 0 || st.LastError != "" {
		t.Errorf("status after: %+v", st)
	}
}

// An answer lost on the way back: the server holds the batch, the client
// does not know; the next pass is told where the server is and goes on.
func TestLostAckIsResolvedByTheServersHead(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	sh := newShipper(t, s, org, filepath.Join(t.TempDir(), "ship.json"))
	rec := session(t, s, "s-1", "one", "two")
	org.loseAck = true
	_ = sh.Pass(context.Background())
	org.loseAck = false
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "three"}); err != nil {
		t.Fatal(err)
	}
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	shippedEqualsLocal(t, s, org, "s-1")
}

// A line the secrets store still redacts is not sent, nor anything after it.
func TestLineHoldingASecretIsNotShipped(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	var events []Event
	sh := newShipper(t, s, org, filepath.Join(t.TempDir(), "ship.json"), func(o *Options) {
		o.OnEvent = func(e Event) { events = append(events, e) }
	})
	session(t, s, "s-1", "one", "the key is SECRET-VALUE", "three")
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	org.mu.Lock()
	got := org.lines["s-1"]
	org.mu.Unlock()
	for _, l := range got {
		if bytes.Contains(l, []byte("SECRET-VALUE")) {
			t.Fatal("a secret reached the server")
		}
	}
	lines, _, _ := s.LinesFrom("s-1", 0)
	if len(got) >= len(lines) {
		t.Fatalf("the lines from the secret on were shipped (%d of %d)", len(got), len(lines))
	}
	if st := sh.Status(); !strings.Contains(st.Sessions["s-1"].Blocked, "redacts") {
		t.Errorf("not stopped: %+v", st.Sessions["s-1"])
	}
	if len(events) == 0 || events[0].Kind != KindBlocked {
		t.Errorf("no blocked event: %+v", events)
	}
}

func TestSessionsFromBeforeShippingBeganAreNotSent(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	session(t, s, "old", "personal")
	sh := newShipper(t, s, org, filepath.Join(t.TempDir(), "ship.json"), func(o *Options) {
		o.Clock = func() time.Time { return time.Now().Add(time.Hour) }
	})
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(org.lines["old"]) != 0 {
		t.Error("a session from before shipping began was sent")
	}
}

func TestServerRefusalStopsTheSession(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	sh := newShipper(t, s, org, filepath.Join(t.TempDir(), "ship.json"))
	session(t, s, "s-1", "one")
	refuse := func(context.Context, string, string, string, []byte) (int, []byte, error) {
		return http.StatusUnprocessableEntity, mustJSON(Ack{Error: "line 1 of the segment: its hash does not match"}), nil
	}
	sh.o.Do = refuse
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := sh.Status().Sessions["s-1"]; !strings.Contains(c.Blocked, "refused") {
		t.Fatalf("cursor %+v", c)
	}
}

// The local record rewritten under lines already shipped is reported, and
// the session is not shipped further.
func TestDivergedRecordIsReported(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	state := filepath.Join(t.TempDir(), "ship.json")
	sh := newShipper(t, s, org, state)
	session(t, s, "s-1", "one", "two")
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := sh.Status()
	c := st.Sessions["s-1"]
	c.Hash, c.Offset = strings.Repeat("a", 64), 1 // as if the file under it changed
	st.Sessions["s-1"] = c
	if err := SaveState(state, &st); err != nil {
		t.Fatal(err)
	}
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := sh.Status().Sessions["s-1"]; c.Blocked == "" {
		t.Fatal("a diverged record went on shipping")
	}
	if !hasKind(org.events, KindDiverged) {
		t.Errorf("the server was not told: %+v", org.events)
	}
}

func TestBacklogAndGapAreReported(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	sh := newShipper(t, s, org, filepath.Join(t.TempDir(), "ship.json"), func(o *Options) { o.MaxBacklog = 10 })
	rec := session(t, s, "s-1", "one", "two")
	org.offline = true
	_ = sh.Pass(context.Background())
	if !hasLogged(sh.Status().Events, KindBacklog) {
		t.Fatalf("no backlog event: %+v", sh.Status().Events)
	}
	org.offline = false
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// More is written while offline, then the session is pruned before it goes.
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "three"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Sync("s-1")
	org.offline = true
	_ = sh.Pass(context.Background())
	org.offline = false
	_ = s.Release("s-1")
	if _, err := s.Prune("s-1", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !hasKind(org.events, KindGap) {
		t.Errorf("no gap reported: %+v", org.events)
	}
}

func hasKind(es []Event, kind string) bool {
	for _, e := range es {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func hasLogged(es []Logged, kind string) bool {
	for _, e := range es {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestBatchRoundTripAndLimits(t *testing.T) {
	body, err := EncodeBatch(Header{Session: "s", From: 2, Before: local.Genesis}, [][]byte{[]byte(`{"a":1}`), []byte(`{"b":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	h, raws, err := DecodeBatch(bytes.NewReader(body), MaxBatchBytes)
	if err != nil || h.Session != "s" || h.From != 2 || len(raws) != 2 {
		t.Fatalf("round trip: %v %+v %d", err, h, len(raws))
	}
	if _, err := EncodeBatch(Header{}, [][]byte{[]byte("a\nb")}); err == nil {
		t.Error("a line with a newline was encoded")
	}
	for name, b := range map[string]string{
		"empty":       "",
		"no header":   `{"a":1}` + "\n",
		"old version": `{"abhed_ship":9,"session":"s"}` + "\n",
		"extra field": `{"abhed_ship":1,"session":"s","x":1}` + "\n",
		"blank line":  `{"abhed_ship":1,"session":"s"}` + "\n\n",
	} {
		if _, _, err := DecodeBatch(strings.NewReader(b), MaxBatchBytes); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	big := `{"abhed_ship":1,"session":"s"}` + "\n" + strings.Repeat("x", 200) + "\n"
	if _, _, err := DecodeBatch(strings.NewReader(big), 100); err == nil {
		t.Error("an oversized batch was accepted")
	}
}

func TestOversizeLineStopsTheSessionAndConflictsAreBounded(t *testing.T) {
	s := openStore(t)
	org := newFakeOrg()
	sh := newShipper(t, s, org, filepath.Join(t.TempDir(), "ship.json"))
	session(t, s, "s-big", strings.Repeat("x", MaxLineBytes+10))
	if err := sh.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := sh.Status().Sessions["s-big"]; !strings.Contains(c.Blocked, "over the") {
		t.Fatalf("an oversize line: %+v", c)
	}
	// A server that keeps naming a new place is given up on for the pass.
	session(t, s, "s-409", "one", "two")
	ls, _, _ := s.LinesFrom("s-409", 0)
	places := []Ack{{Lines: 1, Hash: ls[0].Hash}, {Lines: 0, Hash: local.Genesis}}
	n := 0
	sh.o.Do = func(_ context.Context, _, path, _ string, _ []byte) (int, []byte, error) {
		if !strings.Contains(path, "s-409") {
			return http.StatusOK, nil, nil
		}
		n++
		return http.StatusConflict, mustJSON(places[(n-1)%2]), nil
	}
	done := make(chan error, 1)
	go func() { done <- sh.Pass(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a moving server was not reported")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shipper spun on 409s")
	}
}
