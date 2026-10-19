package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/store/chain"
)

func logsServer(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()
	st := agent.NewMemStore()
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for i, ev := range []struct {
		t agent.EventType
		p string
	}{
		{agent.EvSessionStarted, `{"provider":"openai","model":"m-1"}`},
		{agent.EvUserMessage, `{"text":"[REDACTED:api_key] is the key"}`},
		{agent.EvAgentDelta, `{"text":"do","n":0}`},
		{agent.EvModelCall, `{"model":"m-1","tokens_in":5,"tokens_out":2}`},
	} {
		_ = st.Append(agent.Event{ID: "e-" + string(rune('a'+i)), SessionID: "s1", Seq: int64(i + 1), Type: ev.t,
			Payload: json.RawMessage(ev.p), Actor: agent.ActorSystem, Trust: agent.Trusted, CreatedAt: at.Add(time.Duration(i) * time.Second)})
	}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Store: st,
		Registry: tools.NewRegistry(tools.Read{})})
	s.running["s1"] = &liveSession{ID: "s1", User: "anonymous", Tenant: "default", State: "running", Created: time.Now()}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// readSSE reads data lines with their ids until n have come.
func readSSE(t *testing.T, url string, header http.Header, n int) ([]int, []string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the defer below, after the reader goroutine
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d", url, resp.StatusCode)
	}
	var ids []int
	var data []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() && len(data) < n {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "id: "); ok {
				var id int
				_ = json.Unmarshal([]byte(v), &id)
				ids = append(ids, id)
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = append(data, v)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: %d of %d events", url, len(data), n)
	}
	return ids, data
}

// The logs stream is the record mapped: OCSF that validates, the record's
// seq as the SSE id so a client resumes where it stopped, fragments left
// out, and no more than the record holds.
func TestLogsStreamIsTheRecordAsOCSF(t *testing.T) {
	srv := logsServer(t, config.Default())
	ids, data := readSSE(t, srv.URL+"/v1/sessions/s1/logs", nil, 3)
	if want := []int{1, 2, 4}; len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("ids %v, want %v (the delta left out)", ids, want)
	}
	for _, d := range data {
		var ev map[string]any
		if err := json.Unmarshal([]byte(d), &ev); err != nil {
			t.Fatal(err)
		}
		if err := ocsf.Validate(ev); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(data[1], "[REDACTED:api_key]") {
		t.Fatalf("the message is not as the record holds it: %s", data[1])
	}

	// A client that saw seq 2 resumes after it; the model call still names
	// the provider session.started gave, read from before the cursor.
	h := http.Header{"Last-Event-ID": {"2"}}
	ids, data = readSSE(t, srv.URL+"/v1/sessions/s1/logs?version=1.5.0", h, 1)
	if ids[0] != 4 || !strings.Contains(data[0], `"version":"1.5.0"`) || !strings.Contains(data[0], `"ai_model"`) {
		t.Fatalf("resumed: %v %s", ids, data[0])
	}
	_, data = readSSE(t, srv.URL+"/v1/sessions/s1/logs?format=shorthand&after=3", nil, 1)
	if !strings.HasPrefix(data[0], "2026-10-19T12:00:03.000Z s1#4 API:Model call") {
		t.Fatalf("shorthand: %s", data[0])
	}
}

func TestLogsStreamRefusesWhatItCannotWrite(t *testing.T) {
	srv := logsServer(t, config.Default())
	for _, q := range []string{"?format=xml", "?version=1.4.0"} {
		resp, err := http.Get(srv.URL + "/v1/sessions/s1/logs" + q)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", q, resp.StatusCode)
		}
	}
}

// Another person's session is not found, as on /events.
func TestLogsStreamIsTheOwnersOnly(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	srv := logsServer(t, cfg)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/s1/logs", nil)
	req.Header.Set("X-Abhed-User", "mallory")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another person's session: %d", resp.StatusCode)
	}
}

// linkedMemory is a control-plane store whose audit rows are chained.
type linkedMemory struct{ *controlplane.Memory }

func (linkedMemory) AuditLinks(_ context.Context, seqs []int64) (map[int64]chain.Link, error) {
	out := map[int64]chain.Link{}
	for _, q := range seqs {
		out[q] = chain.Link{Pos: q + 100, Prev: fmt.Sprintf("prev-%d", q), Hash: fmt.Sprintf("hash-%d", q)}
	}
	return out, nil
}
func (linkedMemory) AuditChainName() string { return "test/audit" }

// A page of a chained store's rows carries each row's chain links.
func TestSandboxEventsOCSFCarriesChainLinks(t *testing.T) {
	mem := linkedMemory{controlplane.NewMemory()}
	st, _ := controlplane.NewSettings(nil, nil)
	svc, err := controlplane.New(controlplane.Config{Store: mem, Audit: mem, Drivers: driver.NewRegistry(), Settings: st,
		Workspace: func(string) (string, error) { return t.TempDir(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{}, ControlPlane: svc,
		Registry: tools.NewRegistry(tools.Read{})})
	rec := httptest.NewRecorder()
	items := []controlplane.AuditEvent{{Seq: 7, At: time.Now(), Principal: "ana", Action: "create", SandboxID: "sb", Outcome: "ok"}}
	s.sandboxEventsOCSF(rec, httptest.NewRequest("GET", "/v1/sandboxes/sb:events?format=ocsf", nil), items, 0)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("%v %s", err, rec.Body.String())
	}
	r := page.Items[0]["unmapped"].(map[string]any)["record"].(map[string]any)
	if r["chain"] != "test/audit" || r["pos"] != float64(107) || r["hash"] != "hash-7" || r["prev"] != "prev-7" {
		t.Fatalf("unmapped.record %v", r)
	}
}
