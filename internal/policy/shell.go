package policy

import (
	"path"
	"strings"
)

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
		for _, f := range strippedForms(p) {
			add(f)
		}
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

// wrapperWords run the command that follows them, after their options and,
// for timeout, a duration.
var wrapperWords = map[string]int{
	"env": 0, "command": 0, "exec": 0, "nohup": 0, "nice": 0, "builtin": 0, "sudo": 0, "doas": 0,
	"coproc": 0, "time": 0, "timeout": 1, "xargs": 0, "setsid": 0, "stdbuf": 0, "ionice": 0,
}

// strippedForms returns the segment without its leading keywords, VAR=value
// assignments, redirections and wrappers. Whether an option takes a value
// is not known, so both readings are returned: an extra form only adds a match.
func strippedForms(segment string) []string {
	fields := strings.Fields(segment)
	var out []string
	seen := map[[2]int]bool{}
	var from func(i int)
	from = func(i int) {
		for ; i < len(fields); i++ {
			w := fields[i]
			if shellPrefixWords[w] || isAssignment(w) {
				continue
			}
			if redirect, bare := isRedirection(w); redirect {
				if bare {
					i++
				}
				continue
			}
			if skip, wrapper := wrapperWords[path.Base(w)]; wrapper {
				wrapperOptions(fields, i+1, skip, seen, from)
				return
			}
			out = append(out, strings.Join(fields[i:], " "))
			return
		}
	}
	from(0)
	return out
}

// wrapperOptions follows a wrapper's options from i, reading each both as a
// flag and as taking the next word, then skips skip positional words.
func wrapperOptions(fields []string, i, skip int, seen map[[2]int]bool, next func(int)) {
	if seen[[2]int{i, skip}] {
		return
	}
	seen[[2]int{i, skip}] = true
	if i < len(fields) && fields[i] == "--" {
		next(i + 1 + skip)
		return
	}
	if i < len(fields) && len(fields[i]) > 1 && strings.HasPrefix(fields[i], "-") {
		wrapperOptions(fields, i+1, skip, seen, next)
		wrapperOptions(fields, i+2, skip, seen, next)
		return
	}
	next(i + skip)
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
