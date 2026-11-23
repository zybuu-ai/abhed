package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// hookLocalUse counts the local requests started and ended while the test runs.
func hookLocalUse(t *testing.T) (started, ended *atomic.Int32) {
	started, ended = new(atomic.Int32), new(atomic.Int32)
	f := func(string) func() {
		started.Add(1)
		return func() { ended.Add(1) }
	}
	old := LocalUse.Swap(&f)
	t.Cleanup(func() { LocalUse.Store(old) })
	return started, ended
}

// A completion, as Studio's inline completions, next edits and commit
// messages send, counts as use of a local model, from its start to its end.
func TestInfillCountsAsLocalUse(t *testing.T) {
	started, ended := hookLocalUse(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if started.Load() != 1 || ended.Load() != 0 {
			t.Errorf("not in use while the request runs: %d %d", started.Load(), ended.Load())
		}
		_, _ = io.WriteString(w, `{"choices":[{"text":"x"}]}`)
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL+"/v1", "", "m", Profile{})
	if _, err := a.Infill(context.Background(), InfillRequest{Prefix: "a"}); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 1 || ended.Load() != 1 {
		t.Fatalf("started %d ended %d", started.Load(), ended.Load())
	}
}

func TestChatCountsAsLocalUseUntilTheStreamEnds(t *testing.T) {
	started, ended := hookLocalUse(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := NewOpenAICompatible(srv.URL+"/v1", "", "m", Profile{SupportsStream: true})
	ch, err := a.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if started.Load() != 1 {
		t.Fatalf("started %d", started.Load())
	}
	for range ch {
	}
	for range 200 {
		if ended.Load() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the request never ended its use")
}

func TestRemoteRequestIsNotLocalUse(t *testing.T) {
	started, _ := hookLocalUse(t)
	end := LocalRequest("https://api.example.com/v1")
	end()
	if started.Load() != 0 {
		t.Fatal("a remote endpoint counted as local use")
	}
}
