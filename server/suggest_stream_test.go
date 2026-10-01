package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
)

// gatedSuggest answers a turn at once and holds the suggestion call until
// its gate is closed.
type gatedSuggest struct{ gate chan struct{} }

func (*gatedSuggest) Name() string                           { return "gs" }
func (*gatedSuggest) Profile() model.Profile                 { return model.Profile{Name: "gs", ContextWindow: 32000} }
func (*gatedSuggest) CountTokens(model.Request) (int, error) { return 10, nil }
func (g *gatedSuggest) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	text := "done"
	if strings.Contains(req.Messages[0].Content, "is conversation data") {
		select {
		case <-g.gate:
			text = "Add a test for it"
		case <-ctx.Done():
			ch <- model.Chunk{Type: model.ChunkError, Err: ctx.Err()}
			close(ch)
			return ch, nil
		}
	}
	ch <- model.Chunk{Type: model.ChunkText, Text: text}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// The run ends without waiting for its suggestion, and the stream stays open
// past the end to carry it, then closes after the suggestion's model.call.
func TestStreamCarriesSuggestionAfterEnd(t *testing.T) {
	g := &gatedSuggest{gate: make(chan struct{})}
	b := newBGServerWith(t, nil, func(_ *config.Config, o *Options) { o.Adapter = g })
	id := b.start("hi", false)
	<-b.ended
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })

	srv := httptest.NewServer(b.h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+id+"/events", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", "alice")
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below, after the reader goroutine is done with it
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitLine := func(want string) {
		t.Helper()
		for deadline := time.After(10 * time.Second); ; {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the stream closed before %s", want)
				}
				if strings.Contains(l, want) {
					return
				}
			case <-deadline:
				t.Fatalf("no %s", want)
			}
		}
	}
	waitLine(`"suggesting":true`)
	select {
	case l, ok := <-lines:
		if !ok || strings.Contains(l, "suggestion.offered") {
			t.Fatalf("before the suggestion was made: %q, open %v", l, ok)
		}
	case <-time.After(200 * time.Millisecond):
	}
	close(g.gate)
	waitLine(`"type":"suggestion.offered"`)
	waitLine(`"purpose":"suggestion"`)
	for deadline := time.After(10 * time.Second); ; {
		select {
		case _, ok := <-lines:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the stream stayed open after the suggestion's call")
		}
	}
}
