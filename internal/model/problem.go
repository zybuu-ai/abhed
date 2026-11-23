package model

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// Kinds of provider problem a person can act on.
const (
	ProblemKey         = "key"         // the key is missing, wrong or not allowed this model
	ProblemCredit      = "credit"      // the account has no credit or quota left
	ProblemRetired     = "retired"     // the model is retired or not found
	ProblemRateLimit   = "rate_limit"  // too many requests for now
	ProblemUnavailable = "unavailable" // the provider failed on its side
	ProblemOther       = "other"
)

// Problem is a provider's refusal as a person reads it: its kind, the
// provider's own words, and the status it answered.
type Problem struct {
	Kind    string `json:"kind"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
}

var (
	creditRe  = regexp.MustCompile(`(?i)credit balance|insufficient[_ ]quota|exceeded your current quota|billing|payment required|out of credits?|quota (?:has been )?exceeded`)
	retiredRe = regexp.MustCompile(`(?i)no longer available|is not found|not found for api version|model[^\n]{0,80}?(?:not found|does not exist|deprecated|retired|decommissioned|discontinued|not supported)|(?:deprecated|retired|decommissioned) model|unknown model|invalid model`)
)

// ProblemOf reads a model call's error as a provider problem, or nil when
// the provider did not answer with one (a dial error, a timeout).
func ProblemOf(err error) *Problem {
	var se *StatusError
	if !errors.As(err, &se) {
		return nil
	}
	msg := providerMessage(se.Body)
	p := &Problem{Status: se.Status, Message: msg}
	body := se.Body + " " + msg
	switch {
	case se.Status == http.StatusPaymentRequired || creditRe.MatchString(body):
		p.Kind = ProblemCredit
	case se.Status == http.StatusNotFound || (se.Status == http.StatusBadRequest || se.Status == http.StatusForbidden) && retiredRe.MatchString(body):
		p.Kind = ProblemRetired
	case se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden:
		p.Kind = ProblemKey
	case se.Status == http.StatusTooManyRequests:
		p.Kind = ProblemRateLimit
	case se.Status >= 500:
		p.Kind = ProblemUnavailable
	default:
		p.Kind = ProblemOther
	}
	if p.Message == "" {
		p.Message = http.StatusText(se.Status)
	}
	return p
}

// providerMessage is the provider's own message from an error body: the
// error.message most providers answer with, or the body itself, clipped.
func providerMessage(body string) string {
	body = strings.TrimSpace(body)
	var v any
	if json.Unmarshal([]byte(body), &v) == nil {
		if m := findMessage(v, 0); m != "" {
			return clipProblem(m)
		}
	}
	return clipProblem(body)
}

// findMessage looks for a message, detail or error string, as Anthropic,
// OpenAI, Gemini and their gateways nest it.
func findMessage(v any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if m := findMessage(e, depth+1); m != "" {
				return m
			}
		}
	case map[string]any:
		for _, k := range []string{"message", "detail", "error_description"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		if s, ok := t["error"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if e, ok := t["error"]; ok {
			return findMessage(e, depth+1)
		}
	}
	return ""
}

// keyShapes are what a provider's message may echo of a key: it is never
// repeated to the chat.
var keyShapes = regexp.MustCompile(`(?i)\b(?:sk|pk|rk|ak)-[A-Za-z0-9_.\-]+|\bAIza[0-9A-Za-z_\-]+|\bBearer\s+\S+|\bgh[pousr]_\w+|\bxox[abprs]-[\w-]+|\b(?:api[_-]?key|key|token)=[^\s&"']+`)

func clipProblem(s string) string {
	s = keyShapes.ReplaceAllString(s, "[redacted]")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}
