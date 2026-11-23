package agent

import (
	"regexp"
	"strings"
)

// selfReason is why a self-introduction or a statement of what the assistant
// can do is not checked: it says nothing about this session's work.
const selfReason = "a self-introduction or a statement of what the assistant can do; not a claim about this session's work"

// selfLeadRe opens a sentence about the assistant itself, after an optional
// greeting: who it is ("I'm Abhed", "I am your assistant") or what it can
// do in general ("I can help with", "I'm here to"). "I can confirm", "I can
// see" and the like report what was found, and are not among them.
var selfLeadRe = regexp.MustCompile(`^(?:(?:hi|hello|hey|greetings|welcome)(?: there)?[!,.]*\s+)?` +
	`(?:(?:i'm|i am|this is|my name is)\s+(?:abhed\b|an?\s|your\s)|` +
	`(?:i can|i'm able to|i am able to|i'm here to|i am here to)\s+(?:(?:also|help|assist|answer|explain|read|write|run|edit|search|work|look|review|debug|plan|create|build|use)\b))`)

// selfAfterRe is wording that turns a sentence about the assistant into a
// report of the session: something done, seen or found.
var selfAfterRe = regexp.MustCompile(`\b(?:i've|i have|i had|i did|i was|i just|i already|already|just now|so far|in this session|this session|i ran|i made|i wrote|i fixed|i changed|i updated|i added|i removed|i found|i checked|i tested|i verified|i deployed|confirm|confirmed|verified|tested|deployed|released|built|ran|wrote|made|changed|updated|added|removed|edited|created|passed|passes|pass|fixed|failing|failed|succeeded|done|finished|completed|now)\b`)

// selfSecondRe is a second clause, or a state said of something, after the
// lead: a joining word or mark, or a form of be or have.
var selfSecondRe = regexp.MustCompile(`[;:!?]|\s[-–—]\s|,\s*(?:and|but|so|yet|which|while|because|although|now)\b|\b(?:and|but|so)\s+(?:the|your|its?|this|that|these|those|all|every|my|our|i|we|you|he|she|they|there)\b|\b(?:is|are|was|were|be|been|being|has|have|had|isn't|aren't|wasn't|weren't|hasn't|haven't|it's|that's|there's|they're)\b`)

// selfDescription reports a sentence that introduces the assistant or says
// what it can do, and reports nothing of this session: no command, file,
// number, host, test or change in it, and no wording of something done.
func selfDescription(c Claim) bool {
	if c.Quote != "" || len(c.Evidence) > 0 {
		return false
	}
	low := normText(citeRe.ReplaceAllString(c.Text, " "))
	lead := selfLeadRe.FindStringIndex(low)
	if lead == nil || selfAfterRe.MatchString(low) {
		return false
	}
	// Only a sentence that is wholly about the assistant: no second clause
	// and no state of anything after the lead ("I'm Abhed, and the server is
	// healthy" is a claim about the server, checked as one).
	if rest := low[lead[1]:]; selfSecondRe.MatchString(rest) {
		return false
	}
	f := extractFacts(c.Text)
	if len(f.paths) > 0 || len(f.hosts) > 0 || len(f.numbers) > 0 || len(f.counted) > 0 || len(f.fileLines) > 0 ||
		len(f.ran) > 0 || f.success || f.changed || f.overclaim || f.mergeClean || f.found || f.negated ||
		strings.Contains(c.Text, "`") {
		return false
	}
	return true
}
