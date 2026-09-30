// Package abhed holds what the binary embeds from the root of the
// repository: the changelog, which /release-notes shows offline.
package abhed

import _ "embed"

// Changelog is CHANGELOG.md as built.
//
//go:embed CHANGELOG.md
var Changelog string
