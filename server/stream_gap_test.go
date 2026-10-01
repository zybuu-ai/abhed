package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// gapStore records an event just after the stream has read its backlog, as a
// running session can at any moment.
type gapStore struct {
	*agent.MemStore
	once sync.Once
}

func (g *gapStore) Since(id string, seq int64) ([]agent.Event, error) {
	backlog, err := g.MemStore.Since(id, seq)
	g.once.Do(func() {
		_ = g.Append(agent.Event{ID: "e-gap", SessionID: id, Seq: 2,
			Type: agent.EvAgentMessage, Payload: json.RawMessage(`{}`), CreatedAt: time.Now()})
	})
	return backlog, err
}

// An event recorded between the stream's backlog and its subscription reached
// neither, so a quiet session never showed it. It must arrive.
func TestStreamKeepsAnEventRecordedWhileItOpens(t *testing.T) {
	st := &gapStore{MemStore: agent.NewMemStore()}
	_ = st.Append(agent.Event{ID: "e-1", SessionID: "s1", Seq: 1, Type: agent.EvAgentMessage,
		Payload: json.RawMessage(`{}`), CreatedAt: time.Now()})
	cfg := config.Default()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Store: st,
		Registry: tools.NewRegistry(tools.Read{})})
	s.running["s1"] = &liveSession{ID: "s1", User: "anonymous", Tenant: "default", State: "running", Created: time.Now()}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/sessions/s1/events") //nolint:bodyclose // closed by the defer below, once the stream has been read
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open stream: %d", resp.StatusCode)
	}
	lines := make(chan sseLine, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines <- sseLine{text: sc.Text()}
			}
		}
		lines <- sseLine{closed: true}
	}()
	waitFor(t, lines, seqLine(1))
	waitFor(t, lines, seqLine(2))
}
