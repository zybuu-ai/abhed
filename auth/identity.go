// Package auth establishes who is calling.
//
// Trusting a proxy-set header is fine when a trusted proxy is the only path
// in, but it fails open the moment anything else can reach the port. This
// package holds the identity a request carries, the middleware that
// establishes it, and the one mechanism every deployment has: accounts Abhed
// keeps itself. Verifying tokens against an identity provider is a Provider
// like any other, implemented elsewhere and handed to the middleware.
package auth

import (
	"net/mail"
	"strings"
)

// Identity is the verified caller.
type Identity struct {
	Subject  string   `json:"sub"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Tenant   string   `json:"tenant"`
	Groups   []string `json:"groups"`
	IssuedAt int64    `json:"iat"`
	Expires  int64    `json:"exp"`
	// Provider names what vouched for Subject: ProviderLocal, ProviderProxy,
	// or an edition's sign-in ("oidc", "github"). Empty when nothing did.
	Provider string `json:"provider,omitempty"`
	// EmailVerified is set only when Provider vouched for Email itself.
	// A local account's email is typed by the person and never is.
	EmailVerified bool `json:"email_verified,omitempty"`
}

// Providers this package sets on the identities it builds.
const (
	ProviderLocal = "local"
	ProviderProxy = "proxy"
)

// Anonymous is the owner of every request when authentication is off.
const Anonymous = "anonymous"

// Subagent is the owner a CLI subagent's session row is written under; the
// row's parent says whose it is.
const Subagent = "agent"

// UnclaimedPrefix begins the owner of a session no identity owns any more.
const UnclaimedPrefix = "unclaimed:"

// reservedOwners are the bare owners with a meaning of their own. Only the
// no-identity and subagent cases produce them; a subject equal to one is namespaced.
var reservedOwners = []string{Anonymous, Subagent}

// NobodyPrefix begins the owner of an identity that names no subject. It
// owns nothing: ownership checks refuse it rather than match it.
const NobodyPrefix = "nobody:"

// reservedPrefixes are the namespaces owners are built in. A subject from a
// proxy or an unnamed provider that starts with one is namespaced again.
var reservedPrefixes = []string{"local:", UnclaimedPrefix, "oidc:", "github:", "proxy:",
	"subject:", "schedule:", NobodyPrefix}

// Owner is the principal that owns what this identity creates: sessions,
// approvals it answers, and the rows its subagents write. It is the one
// definition; every ownership check compares these strings.
//
// It never comes from an email the person typed. A local account is owned by
// its username, namespaced so no other provider's principal can equal it.
// Another provider's identity is owned by its email only when that provider
// verified the address (the behaviour sessions have always had there), and
// otherwise by provider and subject. A trusted proxy has sole say over who
// is calling, so its headers keep their meaning.
func (id *Identity) Owner() string {
	if id == nil {
		return Anonymous
	}
	switch id.Provider {
	case "":
		if id.Subject == "" || id.Subject == Anonymous {
			return Anonymous
		}
		return external("subject", id.Subject)
	case ProviderLocal:
		if id.Subject == "" {
			return NobodyPrefix + ProviderLocal
		}
		return LocalOwner(id.Subject)
	case ProviderProxy:
		if id.EmailVerified && ownerEmail(id.Email) {
			return strings.ToLower(id.Email)
		}
		if id.Subject == "" {
			// A proxy vouched for someone; an email that cannot own is no one.
			return NobodyPrefix + ProviderProxy
		}
		return external(ProviderProxy, id.Subject)
	}
	if id.Subject == "" {
		return NobodyPrefix + id.Provider
	}
	if id.EmailVerified && ownerEmail(id.Email) {
		return strings.ToLower(id.Email)
	}
	return id.Provider + ":" + id.Subject
}

// external is a bare subject as an owner, moved under ns when it would
// otherwise read as another namespace's principal.
func external(ns, subject string) string {
	for _, w := range reservedOwners {
		if strings.EqualFold(strings.TrimSpace(subject), w) {
			return ns + ":" + subject
		}
	}
	for _, p := range reservedPrefixes {
		if len(subject) >= len(p) && strings.EqualFold(subject[:len(p)], p) {
			return ns + ":" + subject
		}
	}
	return FoldEmailOwner(subject)
}

// OwnsNothing reports whether owner is one no session may be matched to.
func OwnsNothing(owner string) bool { return strings.HasPrefix(owner, NobodyPrefix) }

// FoldEmailOwner lowercases an owner that is a plain email (an "@" and no
// ":"), so one address owns its sessions whatever case a provider sends.
func FoldEmailOwner(owner string) string {
	if strings.Contains(owner, "@") && !strings.Contains(owner, ":") {
		return strings.ToLower(owner)
	}
	return owner
}

// UnclaimedOwner is what owner's sessions become when owner is removed: a
// key no identity produces, which an administrator can move back by hand.
func UnclaimedOwner(owner string) string { return UnclaimedPrefix + owner }

// LocalOwner is the owner of a local account's sessions.
func LocalOwner(username string) string {
	return ProviderLocal + ":" + strings.ToLower(username)
}

// ownerEmail reports whether a verified email can stand as an owner: a bare
// address, which no namespaced principal (it has no "@") can equal.
func ownerEmail(e string) bool {
	return ValidEmail(e) == nil && !strings.Contains(e, ":")
}

// ValidEmail reports why e is not a plain email address. Display names,
// comments and anything net/mail would rewrite are refused.
func ValidEmail(e string) error {
	a, err := mail.ParseAddress(e)
	if err != nil || a.Name != "" || a.Address != e || strings.Count(e, "@") != 1 {
		return ErrBadEmail
	}
	return nil
}
