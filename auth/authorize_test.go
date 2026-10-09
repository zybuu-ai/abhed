package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Authorize sees the request after the caller is known, so a credential good
// for some requests only is refused the rest, on a token and a session alike,
// and on a stream's recheck as on the first request.
func TestAuthorizeRefusesOutsideTheCredential(t *testing.T) {
	var seen []string
	mw := Middleware{
		Verifier:  fakeVerifier{},
		Providers: []Provider{fakeProvider{name: "fake", entry: "/fake/login", held: "s1"}},
		Authorize: func(r *http.Request, id *Identity) error {
			seen = append(seen, id.Subject+" "+r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/v1/admin") {
				return errors.New("this token is not for administration")
			}
			return nil
		},
	}
	h := mw.Wrap(echoSubject())
	do := func(path string, set func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		set(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer good") }
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "fake", Value: "s1"}) }

	if rec := do("/v1/sessions", bearer); rec.Code != http.StatusOK || rec.Body.String() != "user-42" {
		t.Fatalf("allowed request: %d %q", rec.Code, rec.Body.String())
	}
	rec := do("/v1/admin/users", bearer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not for administration") {
		t.Fatalf("refused token request: %d %q", rec.Code, rec.Body.String())
	}
	rec = do("/v1/admin/users", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("refused session request: %d", rec.Code)
	}
	// A refusal here is about the request, not the session: nothing is cleared.
	if c := rec.Header().Get("Set-Cookie"); c != "" {
		t.Fatalf("a request outside the credential ended the session: %q", c)
	}
	if len(seen) != 3 {
		t.Fatalf("Authorize ran %d times, want 3: %v", len(seen), seen)
	}
}

func TestAuthorizeRunsOnRecheck(t *testing.T) {
	allow := true
	mw := Middleware{Verifier: fakeVerifier{}, Authorize: func(*http.Request, *Identity) error {
		if !allow {
			return errors.New("no longer")
		}
		return nil
	}}
	var recheck error
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allow = false
		_, recheck = Recheck(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/x/events", nil)
	req.Header.Set("Authorization", "Bearer good")
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(context.Background()))
	if recheck == nil || !strings.Contains(recheck.Error(), "no longer") {
		t.Fatalf("recheck = %v, want the Authorize refusal", recheck)
	}
}
