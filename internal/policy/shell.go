package policy

import "strings"

// A bash rule's glob matches the whole command line, which a shell may run as
// several commands. These helpers keep an allow rule to one simple command and
// let deny and ask rules see each command in a chain. The split is best effort
// and ignores quoting: the sandbox, not the pattern, is the boundary.

// hasShellControl reports whether command uses syntax that runs, feeds or
// redirects anything beyond one simple command. A false positive only asks.
func hasShellControl(command string) bool {
	return strings.ContainsAny(command, ";&|\n\r`<>()") || strings.Contains(command, "${")
}

// commandSegments returns the whole command followed by each command it
// contains: split on control operators and taken out of $(...), backticks,
// subshells and process substitution.
func commandSegments(command string) []string {
	out := []string{command}
	seen := map[string]bool{command: true}
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	parts := strings.FieldsFunc(command, func(r rune) bool {
		return strings.ContainsRune(";&|\n\r`()", r)
	})
	for _, p := range parts {
		p = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), "$"))
		p = strings.TrimSpace(strings.TrimRight(p, "<>"))
		add(p)
		add(stripPrefixWords(p))
	}
	return out
}

// shellPrefixWords come before a command without being it.
var shellPrefixWords = map[string]bool{
	"{": true, "}": true, "!": true, "if": true, "then": true, "else": true, "elif": true,
	"fi": true, "do": true, "done": true, "while": true, "until": true, "time": true,
}

// stripPrefixWords drops leading keywords and VAR=value assignments.
func stripPrefixWords(segment string) string {
	fields := strings.Fields(segment)
	i := 0
	for i < len(fields) && (shellPrefixWords[fields[i]] || isAssignment(fields[i])) {
		i++
	}
	return strings.Join(fields[i:], " ")
}

func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range word[:eq] {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}
