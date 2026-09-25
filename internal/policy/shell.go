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

// Bounds on the split, so a hostile command costs linear time. Past one, the
// split is incomplete and the caller must not allow the command.
const (
	maxSplitBytes   = 64 << 10
	maxSegments     = 1024
	maxFormsPerPart = 16
)

// commandSegments returns the whole command, then each command inside it,
// including those in $(...), backticks, subshells and process substitution.
// complete is false when a bound stopped the split short.
func commandSegments(command string) (out []string, complete bool) {
	out = []string{command}
	if len(command) > maxSplitBytes {
		return out, false
	}
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
		forms, whole := strippedForms(p)
		for _, f := range forms {
			add(f)
		}
		if !whole || len(out) > maxSegments {
			return out, false
		}
	}
	return out, true
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
// complete is false when the readings exceed maxFormsPerPart.
func strippedForms(segment string) (forms []string, complete bool) {
	fields := strings.Fields(segment)
	starts := map[int]bool{}
	// Each word is visited at most once per role, so the work is linear and
	// the recursion depth is at most the number of words.
	visited := map[int]bool{}
	optionsSeen := map[[2]int]bool{}
	var from func(i int)
	from = func(i int) {
		for ; i < len(fields) && !visited[i]; i++ {
			visited[i] = true
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
				wrapperOptions(fields, i+1, skip, optionsSeen, from)
				return
			}
			starts[i] = true
			return
		}
	}
	from(0)
	if len(starts) > maxFormsPerPart {
		return nil, false
	}
	for i := range fields {
		if starts[i] {
			forms = append(forms, strings.Join(fields[i:], " "))
		}
	}
	return forms, true
}

// wrapperOptions follows a wrapper's options from i, reading each both as a
// flag and as taking the next word, then skips skip positional words. A lone
// "-" is an option too (env - is env -i).
func wrapperOptions(fields []string, i, skip int, seen map[[2]int]bool, next func(int)) {
	if seen[[2]int{i, skip}] {
		return
	}
	seen[[2]int{i, skip}] = true
	if i < len(fields) && fields[i] == "--" {
		next(i + 1 + skip)
		return
	}
	if i < len(fields) && strings.HasPrefix(fields[i], "-") {
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
