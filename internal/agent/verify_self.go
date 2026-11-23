package agent

import (
	"regexp"
	"strings"
)

// selfReason is why a self-introduction or a statement of what the assistant
// can do is not checked: it says nothing about this session's work.
const selfReason = "a self-introduction or a statement of what the assistant can do; not a claim about this session's work"

// The exemption is an allowlist: a sentence is exempt only when, after an
// optional greeting, it is wholly made of the words below. Anything else in
// it (an adjective, a past participle, a state, a second clause) leaves it to
// be checked as any claim is.

// selfGreetingRe is an optional greeting before the lead.
var selfGreetingRe = regexp.MustCompile(`^(?:hi|hello|hey|greetings|welcome)(?: there)?[!,.]*\s+`)

// selfIdentityRe leads an introduction; selfCapabilityRe leads a statement
// of what the assistant can do.
var (
	selfIdentityRe   = regexp.MustCompile(`^(?:i'm|i am|this is|my name is)\s+`)
	selfCapabilityRe = regexp.MustCompile(`^(?:i can|i'm able to|i am able to|i'm here to|i am here to)\s+(?:also\s+)?`)
)

// selfRoleRe is one role an introduction may name.
var selfRoleRe = regexp.MustCompile(`^(?:abhed|(?:your|an?)\s+(?:(?:technical|general|ai|coding|engineering|personal)\s+){0,2}(?:assistant|agent|helper)(?:\s+(?:that works|working)\s+in\s+your\s+(?:workspace|project|repository))?|a\s+harness\s+for\s+(?:deep\s+)?agents)$`)

// selfVerbs are the general things the assistant says it can do.
var selfVerbs = map[string]bool{"help": true, "assist": true, "answer": true, "explain": true, "read": true, "write": true,
	"run": true, "edit": true, "search": true, "review": true, "debug": true, "plan": true, "build": true, "use": true,
	"work": true, "create": true}

// selfNouns are the plain objects such a statement may name.
var selfNouns = map[string]bool{"code": true, "files": true, "commands": true, "questions": true,
	"infrastructure": true, "project": true, "projects": true, "workspace": true, "repository": true, "repositories": true,
	"tests": true, "scripts": true, "configuration": true, "docs": true, "documentation": true, "bugs": true,
	"servers": true, "clusters": true, "kubernetes": true, "git": true, "shell": true, "terminal": true, "sandbox": true,
	"tasks": true, "tools": true, "you": true}

// selfDeterminers may come before a noun; selfPreps join a noun to another.
var (
	selfDeterminers = map[string]bool{"the": true, "your": true, "a": true}
	selfPreps       = map[string]bool{"in": true, "about": true, "on": true, "across": true}
)

// selfItemSplitRe splits a list of objects or roles.
var selfItemSplitRe = regexp.MustCompile(`\s*,\s*(?:and\s+|or\s+)?|\s+(?:and|or)\s+`)

// selfDescription reports a sentence that is wholly an introduction or a
// general statement of what the assistant can do, made only of the words
// above; a cited or quoted one, or one with a number, path or code, never is.
func selfDescription(c Claim) bool {
	if c.Quote != "" || len(c.Evidence) > 0 || strings.ContainsAny(c.Text, "`0123456789/:;") {
		return false
	}
	low := normText(citeRe.ReplaceAllString(c.Text, " "))
	low = strings.TrimSpace(strings.TrimRight(low, ".! "))
	low = selfGreetingRe.ReplaceAllString(low, "")
	// "I'm here to help" leads a capability, not an introduction.
	if m := selfCapabilityRe.FindString(low); m != "" {
		for i, it := range selfItemSplitRe.Split(low[len(m):], -1) {
			if !selfItem(strings.Fields(it), i == 0) {
				return false
			}
		}
		return true
	}
	if m := selfIdentityRe.FindString(low); m != "" {
		roles := selfItemSplitRe.Split(low[len(m):], -1)
		if len(roles) > 2 {
			return false
		}
		for _, r := range roles {
			if !selfRoleRe.MatchString(r) {
				return false
			}
		}
		return true
	}
	return false
}

// selfItem reports one item of a capability's list: a verb (the first item
// must have one), an optional "with", then a plain object, which may be
// joined to another by a preposition, each after an optional determiner.
// A verb alone ("help") is an item too.
func selfItem(w []string, first bool) bool {
	if len(w) == 0 {
		return false
	}
	if selfVerbs[w[0]] {
		w = w[1:]
	} else if first {
		return false
	}
	if len(w) > 0 && w[0] == "with" {
		w = w[1:]
	}
	if len(w) == 0 {
		return true
	}
	for {
		if selfDeterminers[w[0]] {
			w = w[1:]
		}
		if len(w) == 0 || !selfNouns[w[0]] {
			return false
		}
		w = w[1:]
		if len(w) == 0 {
			return true
		}
		if !selfPreps[w[0]] || len(w) == 1 {
			return false
		}
		w = w[1:]
	}
}
