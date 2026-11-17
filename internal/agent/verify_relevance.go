package agent

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Relevance outcomes, in answer.verdict's relevance.
const (
	RelevanceAddresses = "addresses"
	RelevanceOffTopic  = "off_topic"
)

// The relevance check asks whether an answer speaks to the question the
// person typed. It is cheap, needs no model call, and is only ever a note:
// it never changes the outcome, the exit code or the revision turn. It
// flags only the plain case: the question names things (a quoted phrase, a
// file, an identifier) and the answer mentions none of them, or the
// question has several content words and the answer shares none.

// quotedRe finds text the question quotes.
var quotedRe = regexp.MustCompile("\"([^\"]{3,120})\"|'([^']{3,120})'|`([^`]{2,120})`|“([^”]{3,120})”")

// anchorRe finds a word that names a thing: a path or file name, or an
// identifier in camelCase, snake_case or with a dot or double colon.
var anchorRe = regexp.MustCompile(`[A-Za-z0-9_./:-]*(?:[a-z][A-Z]|_[A-Za-z]|\.[A-Za-z]{1,5}\b|/[A-Za-z]|::)[A-Za-z0-9_./:-]*`)

// relevanceStop are words that carry no topic.
var relevanceStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`about above after again against also among another answer anything around because
		been before being below between both cannot could does doing done down during each either else every from
		have having here into just like make many more most much must need only other over please really same should
		show some such tell than that their them then there these they thing this those through under until upon very
		want what when where whether which while whom whose will with within without would your yours explain describe
		find give know look read file files code work works working does using used use help question`) {
		relevanceStop[w] = true
	}
}

// offTopic says why an answer seems not to address the question, or "".
func offTopic(question, answer string) string {
	q, a := strings.TrimSpace(question), strings.ToLower(answer)
	if q == "" || strings.TrimSpace(answer) == "" || strings.HasPrefix(q, "/") {
		return ""
	}
	var anchors []string
	for _, m := range quotedRe.FindAllStringSubmatch(q, -1) {
		for _, g := range m[1:] {
			if g = strings.TrimSpace(g); g != "" {
				anchors = append(anchors, g)
			}
		}
	}
	for _, m := range anchorRe.FindAllString(q, -1) {
		if m = strings.Trim(m, ".:/-"); len(m) >= 3 {
			anchors = append(anchors, m)
		}
	}
	if len(anchors) > 0 {
		for _, an := range anchors {
			if strings.Contains(a, strings.ToLower(an)) {
				return ""
			}
			// A path is met by its last part, as an answer often names the file alone.
			if i := strings.LastIndexAny(an, "/"); i >= 0 && len(an)-i > 3 && strings.Contains(a, strings.ToLower(an[i+1:])) {
				return ""
			}
		}
		shown := anchors
		if len(shown) > 3 {
			shown = shown[:3]
		}
		return fmt.Sprintf("the question names %s, and the answer mentions none of it", quoteList(shown))
	}
	words := topicWords(q)
	if len(words) < 3 {
		return ""
	}
	have := map[string]bool{}
	for _, w := range topicWords(answer) {
		have[w] = true
	}
	for _, w := range words {
		if have[w] {
			return ""
		}
	}
	return "the answer shares no topic word with the question"
}

// topicWords are a text's lower-case words of four letters or more, cut to
// their first five letters so plurals and tenses meet, without stop words.
func topicWords(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < 4 || relevanceStop[w] {
			continue
		}
		if r := []rune(w); len(r) > 5 {
			w = string(r[:5])
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", clipRunes(s, 60))
	}
	return strings.Join(q, ", ")
}
