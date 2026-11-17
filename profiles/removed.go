package profiles

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zybuu-ai/abhed/internal/visible"
)

// AccessRemovedError is a server refusing this machine because the person's
// organisation removed their access (revoked, or deprovisioned by its
// identity provider), not because a sign-in lapsed: signing in again does
// not help, and the person is told whom to ask instead.
type AccessRemovedError struct {
	// URL is the server that refused.
	URL string
	// Reason is the server's own words for the person, escaped and cut.
	Reason string
}

func (e *AccessRemovedError) Error() string {
	msg := "your organisation has removed your access to " + e.URL
	if e.Reason != "" {
		msg += " (the server says: " + e.Reason + ")"
	}
	return msg + "; signing in again will not restore it. Ask your organisation's Abhed administrator, " +
		"who manages " + e.URL + ", to restore it"
}

// RemovedFrom is the refusal a server sent with status and body when it
// marks it "access_removed": true, or nil for any other answer. Only the
// server's reason for the person is kept, never more of the body.
func RemovedFrom(url string, status int, body []byte) *AccessRemovedError {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return nil
	}
	var why struct {
		Removed bool   `json:"access_removed"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(body, &why) != nil || !why.Removed {
		return nil
	}
	return &AccessRemovedError{URL: url, Reason: shown(why.Reason)}
}

// GrantRemoved is err as the person should read it when a token endpoint
// refused a sign-in with "access_revoked": the organisation removed their
// access. Any other error is returned as it is.
func GrantRemoved(url string, err error) error {
	var oe *OAuthError
	if errors.As(err, &oe) && oe.Code == "access_revoked" {
		return &AccessRemovedError{URL: url, Reason: shown(oe.Description)}
	}
	return err
}

// shown is a server's text on one line, hidden characters escaped, cut.
func shown(s string) string {
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return visible.Text(s, false, true)
}
