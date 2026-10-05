package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A model that starts a reply and then hangs ends the turn on a retryable
// model error after the stall timeout, and is not asked again unseen.
func TestStalledModelCallEndsTheTurnRetryable(t *testing.T) {
	var n atomic.Int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": started\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(done); srv.Close() }()

	a := model.NewOpenAICompatible(srv.URL, "k", "m", model.Profile{ContextWindow: 100000})
	a.SetTimeouts(model.Timeouts{Stall: 200 * time.Millisecond})
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	l := NewLoop(a, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "s", ""), DefaultConfig())

	reason, err := l.Run(context.Background(), "hello")
	if reason != TermError || err == nil || !strings.Contains(err.Error(), "sent nothing") {
		t.Fatalf("reason %s, err %v", reason, err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("the model was asked %d times, want 1", got)
	}
	evs, _ := store.Events("s")
	found := false
	for _, e := range evs {
		var mc ModelCall
		if e.Type == EvModelCall && json.Unmarshal(e.Payload, &mc) == nil {
			found = mc.Retryable && strings.Contains(mc.Error, "can be retried")
		}
	}
	if !found {
		t.Fatal("no retryable model.call error was recorded")
	}
}
