package profiles

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// Only a 401 or 403 the server marks access_removed is a removal; a
// success, an unmarked refusal or a body that does not read is not.
func TestRemovedFromReadsOnlyAMarkedRefusal(t *testing.T) {
	marked := []byte(`{"error":"unauthorized","reason":"revoked","access_removed":true}`)
	for _, c := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, string(marked)},
		{http.StatusInternalServerError, string(marked)},
		{http.StatusUnauthorized, `{"error":"unauthorized","reason":"expired"}`},
		{http.StatusUnauthorized, `{"access_removed":"yes"}`},
		{http.StatusUnauthorized, `{"access_removed":true`},
		{http.StatusForbidden, `not json`},
		{http.StatusForbidden, ``},
	} {
		if e := RemovedFrom("https://a.test", c.status, []byte(c.body)); e != nil {
			t.Errorf("%d %q read as a removal: %v", c.status, c.body, e)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		e := RemovedFrom("https://a.test", status, marked)
		if e == nil || e.URL != "https://a.test" || e.Reason != "revoked" {
			t.Errorf("%d: %+v", status, e)
		}
	}
}

// The server's reason is shown on one line, escaped, and cut at 300
// characters; nothing else of the body.
func TestRemovedReasonIsEscapedAndCut(t *testing.T) {
	long := strings.Repeat("é", 400)
	e := RemovedFrom("https://a.test", http.StatusUnauthorized,
		[]byte(`{"reason":"`+long+`","access_removed":true,"note":"deprovisioned through SCIM as bob"}`))
	if e == nil || utf8.RuneCountInString(e.Reason) != 301 || !strings.HasSuffix(e.Reason, "…") {
		t.Fatalf("the reason: %d runes %q", utf8.RuneCountInString(e.Reason), e.Reason)
	}
	if strings.Contains(e.Error(), "SCIM") {
		t.Fatalf("more than the reason was shown: %v", e)
	}
	e = RemovedFrom("https://a.test", http.StatusUnauthorized, []byte(`{"reason":"a\u001b[2J\nb","access_removed":true}`))
	if e == nil || strings.ContainsAny(e.Reason, "\x1b\n") {
		t.Fatalf("not escaped: %q", e.Reason)
	}
	// Exactly 300 is kept whole.
	if e := RemovedFrom("u", 401, []byte(`{"reason":"`+strings.Repeat("x", 300)+`","access_removed":true}`)); e.Reason != strings.Repeat("x", 300) {
		t.Fatalf("300 characters were cut: %q", e.Reason)
	}
}

// Only a token endpoint's access_revoked is a removal; any other error is
// returned as it is.
func TestGrantRemoved(t *testing.T) {
	err := GrantRemoved("https://a.test", &OAuthError{Code: "access_revoked", Description: "revoked on 16 Nov"})
	var removed *AccessRemovedError
	if !errors.As(err, &removed) || removed.Reason != "revoked on 16 Nov" ||
		!strings.Contains(err.Error(), "your organisation has removed your access to https://a.test") {
		t.Fatalf("access_revoked: %v", err)
	}
	for _, other := range []error{&OAuthError{Code: "access_denied", Description: "no"}, errors.New("network"), nil} {
		if got := GrantRemoved("https://a.test", other); got != other {
			t.Errorf("%v became %v", other, got)
		}
	}
}
