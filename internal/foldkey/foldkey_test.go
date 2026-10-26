package foldkey

import (
	"strings"
	"testing"
)

// Key agrees with strings.EqualFold, Unicode folds included.
func TestKeyMatchesEqualFold(t *testing.T) {
	for _, pair := range [][2]string{{"arguments", "argumentſ"}, {"seq", "ſeq"}, {"kind", "Kind"}, {"Command", "COMMAND"},
		{"request_seq", "requeſt_seq"}, {"success", "succeſs"}, {"message", "meſſage"}, {"a", "b"}, {"ß", "ss"}} {
		if (Key(pair[0]) == Key(pair[1])) != strings.EqualFold(pair[0], pair[1]) {
			t.Errorf("%q %q: Key %q %q, EqualFold %v", pair[0], pair[1], Key(pair[0]), Key(pair[1]), strings.EqualFold(pair[0], pair[1]))
		}
	}
}
