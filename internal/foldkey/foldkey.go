// Package foldkey gives a JSON key's case-folded form as Go's encoding/json
// matches keys: by Unicode simple folding, so "ſ" (U+017F) is "s" and "K"
// (U+212A) is "k". Two keys have the same Key exactly when strings.EqualFold
// says they are equal. The relays use it to refuse a message whose keys one
// reader would take as the same and another as different.
package foldkey

import (
	"strings"
	"unicode"
)

// Key is s with every rune replaced by the smallest rune of its simple
// folding orbit.
func Key(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < least {
				least = f
			}
		}
		b.WriteRune(least)
	}
	return b.String()
}
