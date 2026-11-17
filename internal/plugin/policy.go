package plugin

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Policy is what the managed configuration (or the signed org policy) says
// about plugins. The zero Policy leaves the decision to the person.
type Policy struct {
	Disabled          bool
	RequireSignature  bool
	AllowedPublishers []Publisher
	AllowedBundles    []string
	Sources           []Source
}

// Source is an index of bundles an organisation serves.
type Source struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Basis values: why a bundle is trusted.
const (
	BasisSignature = "signature"  // a keyless signature from an allowed publisher
	BasisPin       = "pin"        // the person pinned its digest at install
	BasisOrgPin    = "org-pin"    // the managed allowed_bundles lists its digest
	BasisSource    = "org-source" // a managed source's index listed its digest
)

// ErrDisabled is a managed configuration that turns plugins off.
var ErrDisabled = errors.New("plugins are turned off by the managed configuration (plugins.disabled)")

// Evidence is what is known about one bundle when it is judged.
type Evidence struct {
	Digest string
	// Signer is the verified signer, nil when unsigned.
	Signer *Signer
	// Pin is the digest the person gave at install, "" for none.
	Pin string
	// Source names the managed source whose index listed Digest, "" for none.
	Source string
}

// Admit decides whether a bundle may be installed or loaded, and on what
// basis. mine are the person's own trusted publishers, used only when the
// managed configuration names none.
func (p Policy) Admit(e Evidence, mine []Publisher) (string, error) {
	if p.Disabled {
		return "", ErrDisabled
	}
	orgLists := len(p.AllowedPublishers) > 0 || len(p.AllowedBundles) > 0
	listed := slices.Contains(p.AllowedBundles, e.Digest)
	if e.Signer != nil {
		if orgLists {
			if slices.ContainsFunc(p.AllowedPublishers, func(a Publisher) bool { return a.Matches(*e.Signer) }) || listed {
				return BasisSignature, nil
			}
			return "", fmt.Errorf("the signer %s is not a publisher the managed configuration allows (plugins.allowed_publishers)", e.Signer)
		}
		if slices.ContainsFunc(mine, func(a Publisher) bool { return a.Matches(*e.Signer) }) {
			return BasisSignature, nil
		}
		if e.Pin != "" && e.Pin == e.Digest && !p.RequireSignature {
			return BasisPin, nil
		}
		return "", fmt.Errorf("the signer %s is not a publisher you trust; add it with: abhed plugin publishers add -issuer %s %s",
			e.Signer, e.Signer.Issuer, e.Signer.Identity)
	}
	if p.RequireSignature {
		return "", errors.New("the bundle is not signed, and the managed configuration requires a signature (plugins.require_signature)")
	}
	switch {
	case listed:
		return BasisOrgPin, nil
	case e.Source != "" && slices.ContainsFunc(p.Sources, func(s Source) bool { return s.Name == e.Source }):
		return BasisSource, nil
	case orgLists:
		return "", errors.New("the bundle is not signed by an allowed publisher and its digest is not in plugins.allowed_bundles")
	case e.Pin != "" && e.Pin == e.Digest:
		return BasisPin, nil
	case e.Pin != "":
		return "", fmt.Errorf("the bundle's digest is %s, not the pinned %s", e.Digest, e.Pin)
	}
	return "", fmt.Errorf("the bundle is not signed; to trust it anyway, check where it came from and pin its digest: -sha256 %s", e.Digest)
}

// SourceFor is the managed source a URL is under, "" when none. Under a
// source means the same scheme and host and a path below the index's folder.
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
