package agent

import (
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/secretfiles"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A credential a person pastes into a message (an `oc login --token=…`
// command, a bearer token, a key) reaches neither the model nor the record.
// Each is replaced by [pasted:ID] and held in the session's memory, and the
// model is told what was pasted, never the value, so it can offer to log in:
// k8s_login with that id stores it and keeps the cluster, on one approval.

// withholdPasted is text with each pasted credential replaced, what was
// withheld as ID:kind, comma-separated, and the note the model gets.
func (l *Loop) withholdPasted(text string) (string, string, string) {
	found := secretfiles.FindPasted(text)
	if len(found) == 0 {
		return text, "", ""
	}
	var b strings.Builder
	var ids, lines []string
	last := 0
	for _, p := range found {
		id := tools.HoldPasted(l.Session, tools.Pasted{Kind: p.Kind, Value: p.Value(text), Server: p.Server, Namespace: p.Namespace})
		b.WriteString(text[last:p.Start])
		if id == "" {
			// No session to hold it in: withheld all the same, and unusable.
			b.WriteString("[pasted credential]")
		} else {
			b.WriteString("[pasted:" + id + "]")
			ids = append(ids, id+":"+p.Kind)
			line := fmt.Sprintf("%s is %s", id, p.Kind)
			if p.Server != "" {
				line += " for the server " + p.Server
			}
			lines = append(lines, line)
		}
		last = p.End
	}
	b.WriteString(text[last:])
	if len(lines) == 0 {
		return b.String(), "", ""
	}
	note := "[Abhed: the user pasted a credential, which was withheld from you and from the record: " +
		strings.Join(lines, "; ") + ". Never ask them to paste it again, and never repeat or guess it. "
	if l.Tools != nil {
		if _, ok := l.Tools.Get("k8s_login"); ok {
			note += "For a cluster token with a server, offer to log in: call k8s_login with pasted set to its id " +
				"and a short cluster name; the user approves once, and Abhed stores the token as a secret, keeps " +
				"the cluster for their later sessions and logs in. "
		}
	}
	note += "To use any other pasted credential, the user stores it with `abhed secret set NAME`.]"
	return b.String(), strings.Join(ids, ","), note
}

// withheldDropped is a message that was not delivered, as the record keeps
// it: stored secrets and pasted credentials withheld, none held for use.
func withheldDropped(l *Loop, text string) string {
	text, _ = l.withholdSecrets(text)
	found := secretfiles.FindPasted(text)
	for i := len(found) - 1; i >= 0; i-- {
		text = text[:found[i].Start] + "[pasted credential]" + text[found[i].End:]
	}
	return text
}

// joinNotes joins the notes Abhed adds after a person's text.
func joinNotes(notes ...string) string {
	var out []string
	for _, n := range notes {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "\n\n")
}
