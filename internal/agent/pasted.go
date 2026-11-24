package agent

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/secretfiles"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A credential a person pastes into a message (an `oc login --token=…`
// command, a bearer token, a key) reaches neither the model nor the record.
// Each is replaced by [pasted:ID] and held in the session's memory, and the
// model is told what was pasted, never the value, so it can offer to log in:
// k8s_login with that id stores it and keeps the cluster, on one approval.

// secretRefRe is a stored secret's placeholder standing for a whole token.
var secretRefRe = regexp.MustCompile(`^\[secret:([A-Z][A-Z0-9_]*)\]$`)

// withholdPasted is text with each pasted credential replaced, what was
// withheld as ID:kind, comma-separated, and the note the model gets.
// raw is the message as typed and withheld the stored secrets the first pass
// replaced in it: a login's [secret:NAME] is taken as that secret only when
// the first pass wrote it here, never when the message itself held the text.
func (l *Loop) withholdPasted(text, raw, withheld string) (string, string, string) {
	found := secretfiles.FindPasted(text)
	if len(found) == 0 {
		return l.withholdHeld(text), "", ""
	}
	var b strings.Builder
	var ids, lines []string
	last := 0
	for _, p := range found {
		held := tools.Pasted{Kind: p.Kind, Value: p.Value(text), Server: p.Server, Namespace: p.Namespace}
		// A login whose token was a stored value reads [secret:NAME] here: it
		// names that secret, and its text is left as the record keeps it.
		if m := secretRefRe.FindStringSubmatch(held.Value); m != nil {
			// Only a value this message held, replaced by the first pass; a
			// [secret:NAME] typed or copied in names nothing, and is held as
			// nothing, so no login can read a secret by it.
			if p.Server == "" || !slices.Contains(strings.Split(withheld, ","), m[1]) ||
				strings.Contains(raw, "[secret:"+m[1]+"]") {
				continue
			}
			held.Value, held.SecretRef, held.Kind = "", m[1], "a login with the stored secret "+m[1]
			id := tools.HoldPasted(l.Session, held)
			b.WriteString(text[last:p.End])
			last = p.End
			if id != "" {
				ids = append(ids, id+":"+held.Kind)
				lines = append(lines, fmt.Sprintf("%s is %s for the server %s", id, held.Kind, p.Server))
			}
			continue
		}
		id := tools.HoldPasted(l.Session, held)
		b.WriteString(text[last:p.Start])
		if id == "" {
			// No session to hold it in: withheld all the same, unusable, and
			// named with no id so the sender is still told.
			b.WriteString("[pasted credential]")
			ids = append(ids, ":"+p.Kind)
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
	if len(lines) > 0 && l.Tools != nil {
		for _, t := range l.Tools.All() {
			if n, ok := t.(tools.PasteNoticer); ok {
				n.NotePasted()
			}
		}
	}
	if len(lines) == 0 {
		return out, strings.Join(ids, ","), ""
	}
	note := "[Abhed: the user pasted a credential, which was withheld from you and from the record: " +
		strings.Join(lines, "; ") + ". Never ask them to paste it again, and never repeat or guess it. "
	if l.Tools != nil {
		if t, ok := l.Tools.Get("k8s_login"); ok {
			if off, isOff := t.(interface{ LoginsOff() string }); isOff && off.LoginsOff() != "" {
				note += "Cluster logins are off on this machine, so do not call k8s_login: " + off.LoginsOff() +
					". Tell the user so. "
			} else {
				note += "For a cluster token with a server, offer to log in: call k8s_login with pasted set to its id " +
					"and a short cluster name; the user approves once, and Abhed stores the token as a secret, keeps " +
					"the cluster for their later sessions and logs in. "
			}
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

// dropped is d, a message that was not delivered, as the record keeps it:
// stored secrets and pasted credentials withheld, none held for use, and
// what was withheld named so the sender can be told.
func (l *Loop) dropped(d DroppedMessage) DroppedMessage {
	text, secrets := l.withholdSecrets(d.Text)
	var kinds []string
	for _, p := range tools.PastedHeld(l.Session) {
		if len(p.Value) >= minHeld && strings.Contains(text, p.Value) {
			kinds = append(kinds, p.ID+":"+p.Kind)
		}
	}
	text = l.withholdHeld(text)
	found := secretfiles.FindPasted(text)
	for i := len(found) - 1; i >= 0; i-- {
		kinds = append(kinds, ":"+found[i].Kind)
		text = text[:found[i].Start] + "[pasted credential]" + text[found[i].End:]
	}
	d.Text, d.SecretsWithheld, d.Pasted = text, secrets, strings.Join(kinds, ",")
	return d
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
