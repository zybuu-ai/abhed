package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// StartRefusal answers as POST /v1/sessions does, so an edition's routes
// that start sessions refuse alike; an unknown cause is never shown.
func TestStartRefusalStatuses(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		retry  bool
	}{
		{fmt.Errorf("x: %w", errDraining), http.StatusServiceUnavailable, true},
		{errHoldFailed, http.StatusServiceUnavailable, true},
		{errBadMode, http.StatusForbidden, false},
		{errNotSoleOwner, http.StatusForbidden, false},
		{errors.New("provider: no such provider"), http.StatusBadRequest, false},
		{errors.New("pq: relation sessions at 10.0.0.9"), http.StatusInternalServerError, false},
	} {
		status, msg, retry := StartRefusal(tc.err)
		if status != tc.status || retry != tc.retry {
			t.Errorf("%v: %d %v, want %d %v", tc.err, status, retry, tc.status, tc.retry)
		}
		if strings.Contains(msg, "10.0.0.9") || strings.HasPrefix(msg, "provider:") {
			t.Errorf("%v: shows %q", tc.err, msg)
		}
	}
}
