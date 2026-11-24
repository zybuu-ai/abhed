package secretfiles

import (
	"regexp"
	"sort"
	"strings"
)

// Pasted is one credential found in what a person typed: where it is, what
// it looks like, and, for a login command, the server and namespace it was
// given with.
type Pasted struct {
	Start, End int
	Kind       string
	Server     string
	Namespace  string
}

// Value is the credential in text.
func (p Pasted) Value(text string) string { return text[p.Start:p.End] }

// loginCommand is an `oc login`, `kubectl … login` or `kubectl config
// set-credentials` command; its flags are read
// one by one, so their order does not matter.
var (
	// A line may continue with a backslash, as a pasted multi-line command does.
	loginCommand = regexp.MustCompile(`(?m)\b(?:oc|kubectl|kubectl-oidc_login|odo)\b(?:[^\n\\]|\\\r?\n|\\.)*?\b(?:login|set-credentials)\b(?:[^\n\\]|\\\r?\n|\\.)*`)
	tokenFlag    = regexp.MustCompile("(?:^|\\s)--token(?:=|\\s+)[\"']?([^\\s\"'`]+)")
	serverFlag   = regexp.MustCompile(`(?:^|\s)(?:--server|-s)(?:=|\s+)["']?(https://[^\s"']+)`)
	serverArg    = regexp.MustCompile(`\blogin\s+["']?(https://[^\s"']+)`)
	nsFlag       = regexp.MustCompile(`(?:^|\s)(?:--namespace|-n)(?:=|\s+)["']?([a-z0-9][a-z0-9-]{0,62})`)
	// An OpenShift API token, whatever it is written with.
	openshiftToken = regexp.MustCompile(`\bsha256~[A-Za-z0-9_-]{20,}`)
)

// kinds names, in the person's words, what each content pattern finds.
var kinds = map[int]string{
	0: "a private key", 1: "an AWS access key id", 2: "a GitHub token", 3: "a GitHub token",
	4: "a GitLab token", 5: "a Slack token", 6: "an API key", 7: "a payment API key",
	8: "a Google API key", 9: "a JSON web token", 10: "a Slack webhook URL",
	11: "a password in a URL", 12: "a token in a URL", 13: "an AWS secret key",
	14: "a bearer token",
}

// FindPasted finds the credentials in a message a person typed: a login
// command's token (with its server), an OpenShift token, and every key or
// token shape redaction knows that is not a placeholder. Matches do not
// overlap and are in order.
func FindPasted(text string) []Pasted {
	var out []Pasted
	taken := func(s, e int) bool {
		for _, p := range out {
			if s < p.End && e > p.Start {
				return true
			}
		}
		return false
	}
	for _, loc := range loginCommand.FindAllStringIndex(text, -1) {
		line := text[loc[0]:loc[1]]
		t := tokenFlag.FindStringSubmatchIndex(line)
		if t == nil || isPlaceholder(line[t[2]:t[3]]) || strings.HasPrefix(line[t[2]:t[3]], "$") {
			continue
		}
		p := Pasted{Start: loc[0] + t[2], End: loc[0] + t[3], Kind: "a cluster token"}
		if openshiftToken.MatchString(line[t[2]:t[3]]) {
			p.Kind = "an OpenShift token"
		}
		if m := serverFlag.FindStringSubmatch(line); m != nil {
			p.Server = strings.TrimRight(m[1], ".,;`)>]")
		} else if m := serverArg.FindStringSubmatch(line); m != nil {
			p.Server = strings.TrimRight(m[1], ".,;`)>]")
		}
		if m := nsFlag.FindStringSubmatch(line); m != nil {
			p.Namespace = m[1]
		}
		out = append(out, p)
	}
	for _, loc := range openshiftToken.FindAllStringIndex(text, -1) {
		if !taken(loc[0], loc[1]) {
			out = append(out, Pasted{Start: loc[0], End: loc[1], Kind: "an OpenShift token"})
		}
	}
	for i, p := range contentPatterns {
		if p.redactOnly {
			continue
		}
		for _, l := range p.re.FindAllStringSubmatchIndex(text, -1) {
			if !p.counts(text, l) {
				continue
			}
			s, e := l[2*p.value], l[2*p.value+1]
			if s < 0 || taken(s, e) {
				continue
			}
			kind := kinds[i]
			if kind == "" {
				kind = "a credential"
			}
			out = append(out, Pasted{Start: s, End: e, Kind: kind})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
