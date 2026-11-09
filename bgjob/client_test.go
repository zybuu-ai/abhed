package bgjob

import (
	"net/http"
	"strings"
	"testing"
)

// A refusal keeps the server's message in either form, and a server with no
// job routes says so instead of a bare 404.
func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		path   string
		status int
		body   string
		want   string
	}{
		{PathJobs, http.StatusTooManyRequests, `{"error":"you have 2 running"}`, "you have 2 running (429)"},
		{PathJobs, http.StatusForbidden, `{"error":{"code":"x","message":"not yours"}}`, "not yours (403)"},
		{PathJobs, http.StatusNotFound, "404 page not found\n", "this server runs no background jobs"},
		{"/v1/sessions/s/approve", http.StatusNotFound, "404 page not found", "the server answered 404 Not Found"},
		{PathJobs, http.StatusBadGateway, "<html>", "the server answered 502 Bad Gateway"},
	} {
		err := refused(tc.path, tc.status, []byte(tc.body))
		if StatusOf(err) != tc.status || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %d %q: %v (status %d), want %q", tc.path, tc.status, tc.body, err, StatusOf(err), tc.want)
		}
	}
}

func TestStateFinal(t *testing.T) {
	for s, want := range map[State]bool{StateRunning: false, StateWaiting: false, StateDone: true,
		StateFailed: true, StateCancelled: true, StateTimedOut: true} {
		if s.Final() != want {
			t.Errorf("%s.Final() = %v", s, !want)
		}
	}
}
