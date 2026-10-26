package secretfiles

import "regexp"

// contentPattern is one shape of secret written into text; value is the
// submatch that is the secret itself (0 for the whole match).
type contentPattern struct {
	re    *regexp.Regexp
	value int
}

// contentPatterns are the shapes of keys and tokens written into text, the
// one list both the monitor and redaction read: a private key block, tokens
// whose form says what they are, passwords in URLs, and assignments named
// as secrets. A prefix no word ends in needs no boundary before it, so a
// token run into other text is still found.
var contentPatterns = []contentPattern{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|\z)`), 0},
	{regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}\b`), 0},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{40,}\b`), 0},
	{regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}\b`), 0},
	{regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}\b`), 0},
	{regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b`), 0},
	{regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{16,}\b`), 0},
	{regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}\b`), 0},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), 0},
	{regexp.MustCompile(`https://hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9/_-]{20,}`), 0},
	// A URL's password, and a query parameter named as a secret.
	{regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@"'` + "`" + `]*:([^\s/@"'` + "`" + `]+)@`), 1},
	{regexp.MustCompile(`(?i)[?&][a-z0-9_]*(?:token|secret|password|passwd|api_?key|signature)[a-z0-9_]*=([^\s&#"'` + "`" + `]{8,})`), 1},
	{regexp.MustCompile(`(?i)\baws_secret_access_key["']?\s*[=:]\s*["']?(\S{20,})`), 1},
	{regexp.MustCompile(`(?im)^\s*(?:export\s+)?[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|PRIVATE_KEY)[A-Z0-9_]*\s*=\s*(\S{8,})`), 1},
}

// HasKeyMaterial reports whether text carries a key or token by its shape.
func HasKeyMaterial(text string) bool {
	for _, p := range contentPatterns {
		if p.re.MatchString(text) {
			return true
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
		found = true
		out := make([]byte, 0, len(text))
		last := 0
		for _, l := range locs {
			start, end := l[2*p.value], l[2*p.value+1]
			if start < 0 {
				continue
			}
			out = append(out, text[last:start]...)
			out = append(out, "[secret]"...)
			last = end
		}
		text = string(append(out, text[last:]...))
	}
	return text, found
}
