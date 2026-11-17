package plugin

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Policy is what the managed configuration and the signed org policy say
// about plugins. The zero Policy leaves the decision to the person.
type Policy struct {
	Disabled         bool
	RequireSignature bool
	// Lists are allow lists, one per layer that set any (the machine's
	// managed file, the org policy). A bundle must pass every one: a layer
	// only ever narrows what the others admit.
	Lists   []AllowList
	Sources []Source
}

// AllowList is one layer's allowed publishers and bundle digests. A bundle
// passes it when a listed publisher signed it or its digest is listed.
type AllowList struct {
	Layer      string
	Publishers []Publisher
	Bundles    []string
	// Grants says passing this list is enough to trust a bundle: the
	// machine's managed file, or an org policy whose keys the machine pins.
	// A list that does not grant only narrows; the trust must still come
	// from the person (a publisher of theirs, or a pin) or a granting layer.
	Grants bool
}

func (l AllowList) passes(e Evidence) bool {
	if slices.Contains(l.Bundles, e.Digest) {
		return true
	}
	return e.Signer != nil && slices.ContainsFunc(l.Publishers, func(p Publisher) bool { return p.Matches(*e.Signer) })
}

// Source is an index of bundles an organisation serves.
type Source struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Trusted says a bundle the index lists is trusted for that alone: a
	// source the machine's managed file names, or the org server of an org
	// policy whose keys the machine pins.
	Trusted bool `json:"-"`
}

// Basis values: why a bundle is trusted.
const (
	BasisSignature = "signature"  // a keyless signature from an allowed publisher
	BasisPin       = "pin"        // the person pinned its digest at install
	BasisOrgPin    = "org-pin"    // a granting allow list names its digest
	BasisSource    = "org-source" // a trusted source's index listed its digest
)

// ErrDisabled is a managed configuration that turns plugins off.
var ErrDisabled = errors.New("plugins are turned off by the managed configuration (plugins.disabled)")

// Evidence is what is known about one bundle when it is judged.
type Evidence struct {
	Digest string
	// Signer is the verified signer, nil when unsigned or not verified now.
	Signer *Signer
	// Pin is the digest the person approved by, "" for none.
	Pin string
	// Source names the source whose index listed Digest, "" for none.
	Source string
}

// Admit decides whether a bundle may be installed or loaded, and on what
// basis. mine are the person's own trusted publishers.
//
// Every allow list must pass. Then the trust itself comes from, in order:
// a granting list, the person's publishers, a trusted source, or the
// person's pin (never under require_signature).
func (p Policy) Admit(e Evidence, mine []Publisher) (string, error) {
	if p.Disabled {
		return "", ErrDisabled
	}
	if e.Signer == nil && p.RequireSignature {
		return "", errors.New("the bundle has no signature that verifies, and the managed configuration requires one (plugins.require_signature)")
	}
	granted := false
	for _, l := range p.Lists {
		if !l.passes(e) {
			who := "unsigned"
			if e.Signer != nil {
				who = "signed by " + e.Signer.String()
			}
			return "", fmt.Errorf("the %s allow list (plugins.allowed_publishers, plugins.allowed_bundles) does not admit this bundle (%s, digest %s)", l.Layer, who, e.Digest)
		}
		granted = granted || l.Grants
	}
	switch {
	case granted && e.Signer != nil:
		return BasisSignature, nil
	case granted:
		return BasisOrgPin, nil
	case e.Signer != nil && slices.ContainsFunc(mine, func(a Publisher) bool { return a.Matches(*e.Signer) }):
		return BasisSignature, nil
	case p.fromSource(e):
		return BasisSource, nil
	case e.Pin != "" && e.Pin == e.Digest && !p.RequireSignature:
		return BasisPin, nil
	case e.Pin != "" && e.Pin != e.Digest:
		return "", fmt.Errorf("the bundle's digest is %s, not the pinned %s", e.Digest, e.Pin)
	case e.Signer != nil:
		return "", fmt.Errorf("the signer %s is not a publisher you trust; add it with: abhed plugin publishers add -issuer %s %s",
			e.Signer, e.Signer.Issuer, e.Signer.Identity)
	}
	return "", fmt.Errorf("the bundle is not signed; to trust it anyway, check where it came from and pin its digest: -sha256 %s", e.Digest)
}

// fromSource reports a bundle a trusted source still configured listed.
func (p Policy) fromSource(e Evidence) bool {
	return e.Source != "" && slices.ContainsFunc(p.Sources, func(s Source) bool { return s.Name == e.Source && s.Trusted })
}

// SourceFor is the source a URL is under, "" when none. Under a source
// means the same scheme and host and a path below the index's folder.
func (p Policy) SourceFor(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	for _, s := range p.Sources {
		base, err := url.Parse(s.URL)
		if err != nil {
			continue
		}
		dir := base.Path[:strings.LastIndexByte(base.Path, '/')+1]
		if u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host) && strings.HasPrefix(u.Path, dir) && !strings.Contains(u.Path, "/../") {
			return s.Name
		}
	}
	return ""
}
