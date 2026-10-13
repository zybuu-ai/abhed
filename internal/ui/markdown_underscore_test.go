package ui

import (
	"strings"
	"testing"
)

// Underscores inside a word, as in a Python file name, are literal; between
// words they still mark emphasis.
func TestIntrawordUnderscoresAreLiteral(t *testing.T) {
	plain := func(segs []seg) (text string, styled bool) {
		var b strings.Builder
		for _, s := range segs {
			b.WriteString(s.text)
			styled = styled || s.bold || s.italic
		}
		return b.String(), styled
	}
	for in, want := range map[string]string{
		"__init__.py":                  "__init__.py",
		"edit pkg/__init__.py now":     "edit pkg/__init__.py now",
		"see snake_case_name here":     "see snake_case_name here",
		"run __main__.py and my_mod_x": "run __main__.py and my_mod_x",
		"a__b__c":                      "a__b__c",
	} {
		got, styled := plain(parseInline(in, false))
		if got != want || styled {
			t.Errorf("%q: %q styled=%v", in, got, styled)
		}
	}
	for _, in := range []string{"this is __bold__ text", "this is __bold__.", "an _italic_ word", "_italic_, then"} {
		if _, styled := plain(parseInline(in, false)); !styled {
			t.Errorf("%q: no emphasis", in)
		}
	}
}
