package policy

import "strings"

// A bash rule's glob sees the whole line, which may run several commands. The
// split below ignores quoting and is best effort: the sandbox is the boundary.

// hasShellControl reports whether command uses syntax that runs, feeds or
// redirects anything beyond one simple command. A false positive only asks.
func hasShellControl(command string) bool {
	return strings.ContainsAny(command, ";&|\n\r`<>()") || strings.Contains(command, "${")
}

// commandSegments returns the whole command, then each command inside it,
// including those in $(...), backticks, subshells and process substitution.
func commandSegments(command string) []string {
	out := []string{command}
	seen := map[string]bool{command: true}
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, p := range splitControl(command) {
		p = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), "$"))
		p = strings.TrimSpace(strings.TrimRight(p, "<>"))
		add(p)
		add(stripPrefixWords(p))
	}
	return out
}

// splitControl splits on ; & | newlines, backticks and parentheses, keeping
// the & and | of a redirection such as 2>&1, &> or >| inside its segment.
func splitControl(command string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch c {
		case '&', '|':
			if i > 0 && (command[i-1] == '>' || command[i-1] == '<') {
				continue
			}
			if c == '&' && i+1 < len(command) && command[i+1] == '>' {
				continue
			}
		case ';', '\n', '\r', '`', '(', ')':
		default:
			continue
		}
		parts = append(parts, command[start:i])
		start = i + 1
	}
	return append(parts, command[start:])
}

// shellPrefixWords come before a command without being it.
var shellPrefixWords = map[string]bool{
	"{": true, "}": true, "!": true, "if": true, "then": true, "else": true, "elif": true,
	"fi": true, "do": true, "done": true, "while": true, "until": true,
}

// wrapperWords run the command that follows them.
var wrapperWords = map[string]bool{
	"env": true, "command": true, "exec": true, "nohup": true, "nice": true,
	"builtin": true, "sudo": true, "coproc": true, "time": true,
}

// wrapperOptionArgs are wrapper options that take the next word as their value.
var wrapperOptionArgs = map[string]bool{"-n": true, "-u": true, "-g": true, "-a": true, "-C": true, "-S": true}

// stripPrefixWords drops leading keywords, wrappers and their options,
// VAR=value assignments and redirections.
func stripPrefixWords(segment string) string {
	fields := strings.Fields(segment)
	i := 0
	for i < len(fields) {
		w := fields[i]
		switch {
		case shellPrefixWords[w] || isAssignment(w):
			i++
		case wrapperWords[w]:
			i++
			for i < len(fields) && strings.HasPrefix(fields[i], "-") {
				if fields[i] == "--" {
					i++
					break
				}
				if wrapperOptionArgs[fields[i]] {
					i++
				}
				i++
			}
		default:
			redirect, bare := isRedirection(w)
			if !redirect {
				return strings.Join(fields[i:], " ")
			}
			i++
			if bare {
				i++
			}
		}
	}
	return ""
}

// isRedirection reports whether word is a redirection, and whether it is the
// operator alone, so that its target is the next word.
func isRedirection(word string) (redirect, bare bool) {
	w := strings.TrimLeft(word, "0123456789")
	if strings.HasPrefix(w, "&>") {
		w = w[1:]
	}
	op := strings.TrimLeft(w, "<>&|")
	if op == w || len(w)-len(op) > 3 {
		return false, false
	}
	return true, op == ""
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
