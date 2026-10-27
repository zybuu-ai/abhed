package secretfiles

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// contentPattern is one shape of secret written into text; value is the
// submatch that is the secret itself (0 for the whole match). A literal
// pattern's value is not a secret when it is a placeholder.
type contentPattern struct {
	re      *regexp.Regexp
	value   int
	literal bool
	bare    bool // an unquoted value, which is not a secret when it is code
	// valid, when set, says whether a value is a secret by its own look,
	// for shapes that prose and fixtures share (an HTTP credential).
	valid func(string) bool
	// redactOnly patterns are hidden from what is shown, but are not taken
	// as a credential seen (HasKeyMaterial): their shape is too common.
	redactOnly bool
	// structured is a JSON or list value, a secret when one of its quoted
	// strings is a literal and not a placeholder.
	structured bool
}

// contentPatterns are the shapes of keys and tokens written into text, the
// one list both the monitor and redaction read: a private key block, tokens
// whose form says what they are, passwords in URLs, and assignments named
// as secrets. A prefix no word ends in needs no boundary before it, so a
// token run into other text is still found.
var contentPatterns = []contentPattern{
	{re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|\z)`)},
	{re: regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{re: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{40,}\b`)},
	{re: regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}\b`)},
	{re: regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{re: regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b`)},
	{re: regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{16,}\b`)},
	{re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}\b`)},
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{re: regexp.MustCompile(`https://hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9/_-]{20,}`)},
	// A URL's password, and a query parameter named as a secret.
	{re: regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://([^\s/:@"'` + "`" + `]*):([^\s/@"'` + "`" + `]+)@`), value: 2, literal: true},
	{re: regexp.MustCompile(`(?i)[?&][a-z0-9_]*(?:token|secret|password|passwd|api_?key|signature)[a-z0-9_]*=([^\s&#"'` + "`" + `]{8,})`), value: 1, literal: true},
	{re: regexp.MustCompile(`(?i)\baws_secret_access_key["']?\s*[=:]\s*["']?([^\s"']{20,})`), value: 1, literal: true},
	// An HTTP credential: a bearer token, as an Authorization header or a
	// --header argument carries it, and, for redaction alone, Basic
	// credentials.
	{re: regexp.MustCompile(`(?i)(?:^|[\s"'=:,(])Bearer[ \t]+([A-Za-z0-9._~+/=-]{16,})`), value: 1, literal: true, valid: bearerToken},
	{re: regexp.MustCompile(`(?i)(?:^|[\s"'=:,(])Basic[ \t]+([A-Za-z0-9+/]{12,}={0,2})`), value: 1, literal: true, valid: basicCredential, redactOnly: true},
	// An upper-case name with SECRET, TOKEN, PASSWORD or KEY as one of its
	// words, given a literal: quoted, or a bare run that is not code.
	{re: regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + secretName + `[ \t]*[=:][ \t]*"([^"\n]{8,})"`), value: 1, literal: true},
	{re: regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + secretName + `[ \t]*[=:][ \t]*'([^'\n]{8,})'`), value: 1, literal: true},
	{re: regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + secretName + `[ \t]*[=:][ \t]*([^\s"'` + "`" + `][^\s'` + "`" + `]{7,})`), value: 1, literal: true, bare: true},
	// A JSON or list value under such a name, plural too (APP_SECRETS), to
	// the end of the line: a secret when it holds a quoted literal.
	{re: regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + secretNames + `[ \t]*[=:][ \t]*['"]?([{\[].*?)['"]?[ \t]*$`), value: 1, literal: true, structured: true},
}

// secretName is an ALL_CAPS identifier one of whose words names a secret;
// secretNames allows the word plural, for a value that holds several.
const (
	secretName  = `(?:[A-Z0-9]+_)*(?:SECRET|TOKEN|PASSWORD|PASSWD|PASS|KEY|APIKEY|CREDENTIALS?)(?:_[A-Z0-9]+)*`
	secretNames = `(?:[A-Z0-9]+_)*(?:SECRET|TOKEN|PASSWORD|PASSWD|PASS|KEY|APIKEY|CREDENTIAL)S?(?:_[A-Z0-9]+)*`
)

// jsonString is one JSON string, read left to right so quotes pair up.
var jsonString = regexp.MustCompile(`"((?:[^"\\\n]|\\.)*)"`)

// placeholder is a value that stands for a secret rather than being one: a
// format verb, an expansion or template naming a variable (no default text),
// a bracketed name, a mask, or a stock word. Brackets hold only a name:
// letters, spaces, '_', '-' and '.', so a bracketed literal or JSON counts.
var placeholder = regexp.MustCompile(`^(?:` +
	`%[-+# 0-9.*]*[a-zA-Z]|\$\{[A-Za-z_][A-Za-z0-9_.]*\}|\$\([A-Za-z_ ./-]+\)|\$[A-Z_][A-Z0-9_]*|` +
	`\{\{[A-Za-z_ .-]+\}\}|\{[A-Za-z_ .-]+\}|<[A-Za-z_ .-]+>|\[[A-Za-z_ .-]+\]|` +
	`\*+|x+|X+|\.{3}|…|(?i:changeme|change_me|change-me|password|secret|redacted|example|placeholder)|` +
	`your[_-][a-z_-]+|YOUR[_-][A-Z_-]+)$`)

// identChain is a dotted chain of identifiers: code, not a value.
var identChain = regexp.MustCompile(`^[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)+$`)

// isPlaceholder reports a value that stands for a secret rather than being
// one, quoted or not.
func isPlaceholder(v string) bool {
	v = strings.TrimRight(v, ";,")
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return v == "" || placeholder.MatchString(v)
}

// counts reports whether one match of p is a secret: for a literal pattern,
// its value is not a placeholder, and in a URL not the user's own name.
func (p contentPattern) counts(text string, loc []int) bool {
	if !p.literal {
		return true
	}
	v := text[loc[2*p.value]:loc[2*p.value+1]]
	if isPlaceholder(v) {
		return false
	}
	if p.valid != nil && !p.valid(v) {
		return false
	}
	if p.structured {
		for _, m := range jsonString.FindAllStringSubmatch(v, -1) {
			if len(m[1]) >= 8 && !isPlaceholder(m[1]) {
				return true
			}
		}
		return false
	}
	if p.bare && (strings.Contains(v, "(") || strings.Contains(v[1:], "[") || identChain.MatchString(strings.TrimRight(v, ";,"))) {
		return false // a call, an index or a field such as settings.API_KEY
	}
	if p.value == 2 && loc[2] >= 0 && text[loc[2]:loc[3]] == v {
		return false // a stock default such as postgres:postgres
	}
	return true
}

// HasKeyMaterial reports whether text carries a key or token by its shape.
func HasKeyMaterial(text string) bool {
	for _, p := range contentPatterns {
		if p.redactOnly {
			continue
		}
		for _, l := range p.re.FindAllStringSubmatchIndex(text, -1) {
			if p.counts(text, l) {
				return true
			}
		}
	}
	return false
}

// RedactContent replaces each key or token in text with "[secret]" and
// reports whether it found one.
func RedactContent(text string) (string, bool) {
	found := false
	for _, p := range contentPatterns {
		locs := p.re.FindAllStringSubmatchIndex(text, -1)
		if len(locs) == 0 {
			continue
		}
		out := make([]byte, 0, len(text))
		last := 0
		for _, l := range locs {
			start, end := l[2*p.value], l[2*p.value+1]
			if start < 0 || !p.counts(text, l) {
				continue
			}
			found = true
			out = append(out, text[last:start]...)
			out = append(out, "[secret]"...)
			last = end
		}
		text = string(append(out, text[last:]...))
	}
	return text, found
}

// fixtureWords mark a value written as a stand-in: a test's, a doc's.
var fixtureWords = []string{"test", "fake", "dummy", "example", "mock", "sample", "placeholder", "changeme", "your_", "your-", "redacted"}

// lowerRun is seven lower-case letters in a row: a word, in a short value.
var lowerRun = regexp.MustCompile(`[a-z]{7}`)

// bearerToken reports whether a bearer value looks like a token rather than
// a fixture or prose: no stand-in word or mask, and, below 20 characters, a
// digit and no word in it; from 20 on, two kinds of character at least.
func bearerToken(v string) bool {
	low := strings.ToLower(v)
	for _, w := range fixtureWords {
		if strings.Contains(low, w) {
			return false
		}
	}
	if masked := strings.Trim(low, "0123456789"); masked == "" || strings.Trim(masked, string(masked[0])) == "" {
		return false // xxxxxxxx1, 00000000
	}
	var lower, upper, digit bool
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	if len(v) < 20 {
		return digit && (lower || upper) && !lowerRun.MatchString(v)
	}
	kinds := 0
	for _, k := range []bool{lower, upper, digit} {
		if k {
			kinds++
		}
	}
	return kinds >= 2
}

// basicCredential reports whether a Basic value is base64 of user:password.
func basicCredential(v string) bool {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(v, "="))
	}
	return err == nil && strings.Contains(string(b), ":") && !isPlaceholder(string(b))
}
