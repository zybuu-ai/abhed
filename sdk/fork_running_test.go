package abhed_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// Fork refuses while a run is in progress and works once it has returned.
func TestForkRefusesDuringARun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	inTurn, release := make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if n.Add(1) == 1 {
			close(inTurn)
			<-release
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(),
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), "hi")
		done <- err
	}()
	select {
	case <-inTurn:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the model")
	}
	if err := a.Fork(0); !errors.Is(err, abhed.ErrForkDuringRun) {
		t.Errorf("Fork during a run: %v, want ErrForkDuringRun", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := a.Fork(0); err != nil {
		t.Fatalf("Fork after the run: %v", err)
	}
}
