package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// removedErr is an edition's refusal of a person whose access was removed.
type removedErr struct{}

func (removedErr) Error() string       { return "your access to this deployment was revoked" }
func (removedErr) AccessRemoved() bool { return true }

type refusingVerifier struct{ err error }

func (v refusingVerifier) Verify(context.Context, string) (*Identity, error) { return nil, v.err }

// A token refused because the person's access was removed is answered with
// "access_removed": true, wrapped or not; any other refusal is not, so a client never
// tells a person their access went when their token only lapsed.
func TestRefusalSaysWhenAccessWasRemoved(t *testing.T) {
	for _, c := range []struct {
		err     error
		removed bool
	}{
		{removedErr{}, true},
		{errors.Join(errors.New("checked"), removedErr{}), true},
		{errors.New("the token has expired; run abhed login again"), false},
	} {
		mw := Middleware{Verifier: refusingVerifier{c.err}}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/org/records/s/lines", nil)
		req.Header.Set("Authorization", "Bearer x")
		mw.Wrap(http.NotFoundHandler()).ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusUnauthorized || body["reason"] != c.err.Error() || (body["access_removed"] == true) != c.removed {
			t.Errorf("%v: %d %v", c.err, rec.Code, body)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%v: no challenge on the 401", c.err)
		}
	}
	// Turned away after it verified (Check), it is a 403 that says so too.
	mw := Middleware{Verifier: fakeVerifier{}, Check: func(context.Context, *Identity) error { return removedErr{} }}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer good")
	mw.Wrap(http.NotFoundHandler()).ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body["access_removed"] != true || body["refused"] != true {
		t.Errorf("refused by Check: %d %v", rec.Code, body)
	}
}
