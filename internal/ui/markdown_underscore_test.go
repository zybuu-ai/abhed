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
		"__init__.py":                        "__init__.py",
		"edit pkg/__init__.py now":           "edit pkg/__init__.py now",
		"see snake_case_name here":           "see snake_case_name here",
		"run __main__.py and my_mod_x":       "run __main__.py and my_mod_x",
		"a__b__c":                            "a__b__c",
		"Edit __main__.py, then __init__.py": "Edit __main__.py, then __init__.py",
	} {
		got, styled := plain(parseInline(in, false))
		if got != want || styled {
			t.Errorf("%q: %q styled=%v", in, got, styled)
		}
	}
	// An opener whose nearest closer runs on into a name stays literal, and
	// is not paired with a closer further on.
	for in, literal := range map[string]string{
		"Created __init__.py and set the __version__ string.": "__init__.py",
		"_private_name.py and _other_":                        "_private_name.py",
		"see __init__.py or **bold** here":                    "__init__.py",
	} {
		var b strings.Builder
		for _, s := range parseInline(in, false) {
			if (s.bold || s.italic) && strings.Contains(s.text, " ") {
				t.Errorf("%q: emphasis spans words: %q", in, s.text)
			}
			b.WriteString(s.text)
		}
		if !strings.Contains(b.String(), literal) {
			t.Errorf("%q: %q is not shown as written: %q", in, literal, b.String())
		}
	}
	for _, in := range []string{"this is __bold__ text", "this is __bold__.", "an _italic_ word", "_italic_, then"} {
		if _, styled := plain(parseInline(in, false)); !styled {
			t.Errorf("%q: no emphasis", in)
		}
	}
}
