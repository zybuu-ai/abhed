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
		return l.withholdHeld(text), "", ""
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
			// The server named is the one held, which is where a login sends it.
			server := p.Server
			if held, ok := tools.PastedValue(l.Session, id); ok {
				server = held.Server
			}
			line := fmt.Sprintf("%s is %s", id, p.Kind)
			if server != "" {
				line += " for the server " + server
			}
			lines = append(lines, line)
		}
		last = p.End
	}
	b.WriteString(text[last:])
	out := l.withholdHeld(b.String())
	if len(lines) == 0 {
		return out, "", ""
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
	return out, strings.Join(ids, ","), note
}

// minHeld is the shortest held value withheld wherever it appears again:
// a shorter one would match ordinary words.
const minHeld = 8

// withholdHeld is text with each credential the session holds replaced by
// its placeholder, wherever it appears again: a later message, a tool's
// output, a hook's text.
func (l *Loop) withholdHeld(text string) string {
	if l == nil || l.Session == nil {
		return text
	}
	for _, p := range tools.PastedHeld(l.Session) {
		if len(p.Value) >= minHeld && strings.Contains(text, p.Value) {
			text = strings.ReplaceAll(text, p.Value, "[pasted:"+p.ID+"]")
		}
	}
	return text
}

// withheldDropped is a message that was not delivered, as the record keeps
// it: stored secrets and pasted credentials withheld, none held for use.
func withheldDropped(l *Loop, text string) string {
	text, _ = l.withholdSecrets(text)
	return withholdShapes(l.withholdHeld(text))
}

// withholdShapes is text with every pasted credential's shape replaced by
// [pasted credential], none held for use.
func withholdShapes(text string) string {
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
