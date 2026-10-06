package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitleRunes bounds a session title: a label for the list, not a note.
const MaxTitleRunes = 120

// CleanTitle trims a session title and checks it is one line of visible
// text, at most MaxTitleRunes long. It returns the title, or why it is
// refused. Every way a title reaches a list goes through it.
func CleanTitle(raw string) (string, string) {
	t := strings.TrimSpace(raw)
	if !utf8.ValidString(t) {
		return "", "the title is not valid text"
	}
	if utf8.RuneCountInString(t) > MaxTitleRunes {
		return "", "the title is longer than 120 characters"
	}
	if strings.IndexFunc(t, unicode.IsControl) >= 0 {
		return "", "the title holds a control character, such as a line break"
	}
	if hiddenFormat(t) {
		return "", "the title holds an invisible format character, such as a direction override"
	}
	return t, ""
}

// hiddenFormat reports a format character in t, other than a zero-width
// joiner (U+200D) between two emoji, which joins them into one, such as
// woman, joiner, laptop for a woman technologist, and hides nothing.
func hiddenFormat(t string) bool {
	runes := []rune(t)
	emoji := func(r rune) bool { return unicode.Is(unicode.So, r) }
	for i, r := range runes {
		if !unicode.Is(unicode.Cf, r) {
			continue
		}
		joined := r == '\u200d' && i > 0 && i+1 < len(runes) && emoji(runes[i+1]) &&
			(emoji(runes[i-1]) || runes[i-1] == '\ufe0f' || runes[i-1] >= 0x1f3fb && runes[i-1] <= 0x1f3ff)
		if !joined {
			return true
		}
	}
	return false
}
