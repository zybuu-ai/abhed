package model

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A request that cannot connect reports where it went without the query or
// user information a base_url may carry a key in.
func TestConnectionErrorDropsTheKeyInTheURL(t *testing.T) {
	build := func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "http://user:pw-SECRET1@127.0.0.1:1/v1/chat?api_key=SECRET2#frag", nil)
	}
	resp, err := send(context.Background(), &http.Client{}, RetryPolicy{Attempts: 1}, build, nil, nil)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error from a closed port")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("the key reached the error: %v", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) || !strings.Contains(ue.URL, "127.0.0.1:1/v1/chat") {
		t.Fatalf("the error lost the endpoint: %v", err)
	}
	resp, err = send(context.Background(), &http.Client{}, RetryPolicy{Attempts: 1}, func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "http://h\x7f/?api_key=SECRET3", nil)
	}, nil, nil)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("an unparsable URL leaked its key: %v", err)
	}
}
