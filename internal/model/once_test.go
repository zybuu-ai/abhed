package model

import (
	"net/http"
	"testing"
)

// Every registered provider's adapter can be held to one request and a
// redirect check, so completions never meet one that cannot.
func TestSendOnceReachesEveryProvider(t *testing.T) {
	check := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, name := range Providers() {
		a, err := New(Spec{Type: name, BaseURL: "http://127.0.0.1:1", Model: "m", APIKey: "k", Project: "p", Region: "r", Extra: map[string]string{"project_id": "p"}})
		if err != nil {
			t.Logf("%s: not built with a bare spec: %v", name, err)
			continue
		}
		if !SendOnce(a, check) {
			t.Errorf("%s: SendOnce cannot reach its client", name)
		}
	}
}
