package secretfiles

import (
	"encoding/base64"
	"regexp"
	"strings"
	"unicode"
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
	// userPass is a URL's user:password, which is no secret when the two are the same.
	userPass bool
	// named, when set, says whether submatch 1, a key's name, names a secret.
	named func(string) bool
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
	{re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}\b`)},
	{re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}\b`)},
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{re: regexp.MustCompile(`https://hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9/_-]{20,}`)},
	// A URL's password, and a query parameter named as a secret.
	{re: regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://([^\s/:@"'` + "`" + `]*):([^\s/@"'` + "`" + `]+)@`), value: 2, literal: true, userPass: true},
	{re: regexp.MustCompile(`(?i)[?&][a-z0-9_]*(?:token|secret|password|passwd|api_?key|signature)[a-z0-9_]*=([^\s&#"'` + "`" + `]{8,})`), value: 1, literal: true},
	{re: regexp.MustCompile(`(?i)\baws_secret_access_key["']?\s*[=:]\s*["']?([^\s"']{20,})`), value: 1, literal: true},
	// An HTTP credential: a bearer token, as an Authorization header or a
	// --header argument carries it, and, for redaction alone, Basic
	// credentials.
	{re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])Bearer[ \t]+([A-Za-z0-9._~+/=-]{16,})`), value: 1, literal: true, valid: bearerToken},
	{re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])Basic[ \t]+([A-Za-z0-9+/]{12,}={0,2})`), value: 1, literal: true, valid: basicCredential, redactOnly: true},
	// A JSON or YAML key whose name says it holds a secret, in any case
	// ("internal_api_key", "clientSecret", "password:"), given a literal.
	{re: regexp.MustCompile(`"([A-Za-z0-9_.-]+)"[ \t]*:[ \t]*"([^"\n]*)"`), value: 2, literal: true, named: secretKeyName, redactOnly: true},
	{re: regexp.MustCompile(`(?m)^[ \t-]*([A-Za-z0-9_.-]+)[ \t]*:[ \t]+["']?([^"'\s#{}\[\]&*|>][^"'\n#]*?)["']?[ \t]*(?:#.*)?$`), value: 2, literal: true, named: secretKeyName, redactOnly: true, valid: yamlLiteral},
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
	if p.named != nil && (loc[2] < 0 || !p.named(text[loc[2]:loc[3]]) || len(v) < 6) {
		return false
	}
	if p.userPass && loc[2] >= 0 && text[loc[2]:loc[3]] == v {
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

// fixture reports a stand-in value (lower-cased): one that starts with a
// stand-in word, holds one as a whole part (between - _ . or the ends), or,
// below 24 characters, holds one anywhere. A long random token that
// happens to spell one inside a run is not one.
func fixture(low string) bool {
	for _, w := range fixtureWords {
		if strings.HasPrefix(low, w) || len(low) < 24 && strings.Contains(low, w) {
			return true
		}
		for i := strings.Index(low, w); i >= 0; {
			end := i + len(w)
			before := i == 0 || strings.ContainsRune("-_.", rune(low[i-1]))
			after := end == len(low) || strings.ContainsRune("-_.", rune(low[end])) || strings.HasSuffix(w, "_") || strings.HasSuffix(w, "-")
			if before && after {
				return true
			}
			next := strings.Index(low[i+1:], w)
			if next < 0 {
				break
			}
			i += 1 + next
		}
	}
	return false
}

// lowerRun is seven lower-case letters in a row: a word, in a short value.
var lowerRun = regexp.MustCompile(`[a-z]{7}`)

// bearerToken reports whether a bearer value looks like a token rather than
// a fixture or prose: no stand-in word as a part of it, not one character
// repeated, and, below 20 characters, a digit and no word in it; from 20
// on, two kinds of character at least.
func bearerToken(v string) bool {
	low := strings.ToLower(v)
	if fixture(low) {
		return false
	}
	if strings.Trim(low, low[:1]) == "" {
		return false // a mask: xxxxxxxxxxxxxxxx
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

// secretWords are the words a key's name holds when its value is a secret.
var secretWords = map[string]bool{"password": true, "passwd": true, "pwd": true, "secret": true, "secrets": true,
	"token": true, "apikey": true, "credential": true, "credentials": true, "passphrase": true}

// secretKeyName reports whether a JSON or YAML key's name says its value is
// a secret: a word of secretWords, or an API, private, access, client or
// signing key, with words split at _ . - and at a change to upper case.
func secretKeyName(name string) bool {
	var words []string
	cur := []rune{}
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	prev := rune(0)
	for _, r := range name {
		switch {
		case r == '_' || r == '.' || r == '-':
			flush()
		case unicode.IsUpper(r) && prev != 0 && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	for i, w := range words {
		if secretWords[w] {
			return true
		}
		if w == "key" && i > 0 {
			switch words[i-1] {
			case "api", "private", "access", "client", "signing", "secret", "encryption", "master":
				return true
			}
		}
	}
	return false
}

// yamlLiteral is a YAML value that is a literal, not code that happens to sit
// after a colon (an object literal's `password: hashed,` or a call).
func yamlLiteral(v string) bool {
	return !strings.HasSuffix(v, ",") && !strings.ContainsAny(v, "()")
}
