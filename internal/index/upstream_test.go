package index

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
)

// The code index's embeddings go out through the upstream proxy.
func TestEmbedderUsesTheUpstreamProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	corp := upstreamtest.Start(t)
	corp.Use(t, "", "")
	e := NewOpenAIEmbedder("http://embed.corp.test:"+u.Port()+"/v1", "", "m", 4)
	resp, err := e.HTTP.Get(e.BaseURL + "/embeddings")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !corp.Saw("embed.corp.test") {
		t.Fatal("the embeddings request did not go through the upstream proxy")
	}
}
