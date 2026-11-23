package index

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

// An embedding counts as use of a local model server.
func TestEmbedCountsAsLocalUse(t *testing.T) {
	var started, ended atomic.Int32
	f := func(string) func() { started.Add(1); return func() { ended.Add(1) } }
	old := model.LocalUse.Swap(&f)
	t.Cleanup(func() { model.LocalUse.Store(old) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
	}))
	defer srv.Close()
	e := NewOpenAIEmbedder(srv.URL+"/v1", "", "nomic", 2)
	e.HTTP = srv.Client()
	_, _ = e.Embed(context.Background(), []string{"x"})
	if started.Load() != 1 || ended.Load() != 1 {
		t.Fatalf("started %d ended %d", started.Load(), ended.Load())
	}
}
