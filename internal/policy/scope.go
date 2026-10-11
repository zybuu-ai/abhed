package policy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// scopeTools are the only programs offered a one-click "always allow": no word
// on the line can make them run a command, and none can delete or write over
// work. git still runs the repository's own hooks and config, which the
// sandbox tier contains. A tool with subcommands maps to the ones allowed;
// nil means its scope is the program alone.
var scopeTools = map[string]map[string]bool{
	// Not config (aliases run commands), fetch (--upload-pack), grep (-O runs a pager)
	// or remote (update fetches; add -f runs a remote helper the URL names).
	// Not restore, checkout or stash, whose everyday forms sit beside ones that
	// discard work; stash offers only its read-only list and show.
	"git": set("status", "diff", "log", "show", "branch", "add", "commit", "switch",
		"stash", "rev-parse", "ls-files", "blame", "tag"),
	"ls": nil, "cat": nil, "head": nil, "tail": nil, "wc": nil, "pwd": nil, "echo": nil,
	"which": nil, "file": nil, "stat": nil, "du": nil, "df": nil, "grep": nil,
	"jq": nil, "diff": nil, "cut": nil, "tr": nil,
	// Not sort (--compress-program) or rg (--pre), which run a program named in
	// their arguments; nor cp, mv, uniq or tree (-o), which can write over a
	// file, and mv can move a folder out of the workspace.
	"mkdir": nil, "touch": nil,
	// Not install, ci or audit, which run package scripts, nor test, run, exec, x or dlx.
	// yarn and pnpm are left out: the workspace can choose the code either runs.
	"npm": set("ls", "outdated"),
	// Not install, which runs a package's build code.
	"pip": set("list", "show", "freeze"), "pip3": set("list", "show", "freeze"),
	// Not run, exec, build or compose.
	"docker": set("ps", "images", "logs", "version"),
	// Left out: go, whose go.mod toolchain line can run a go<version> found on PATH;
	// cargo, whose --config can set a rustc wrapper anywhere on the line; and
	// kubectl, whose --kubeconfig can name a credential plugin to run.
}

// scopeSubs narrows a listed subcommand to the words after it that may be scoped.
var scopeSubs = map[string]map[string]bool{
	"git stash": set("list", "show"),
}

func set(words ...string) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// bashScope is the prefix an "always allow" for a simple command covers, or ""
// when none is offered: the person can still approve once or write a rule.
func bashScope(fields []string) string {
	// An assignment in front, such as LD_PRELOAD= or PAGER=, can make any tool run code.
	if assignment.MatchString(fields[0]) {
		return ""
	}
	subs, ok := scopeTools[fields[0]]
	if !ok {
		return ""
	}
	n := 1
	if subs != nil {
		if n == len(fields) || !subs[fields[n]] {
			return ""
		}
		n++
		if next, nested := scopeSubs[strings.Join(fields[:n], " ")]; nested {
			if n == len(fields) || !next[fields[n]] {
				return ""
			}
			n++
		}
	}
	// A program with subcommands may take code by a short flag (git -c);
	// for the others -c and -e are counts or patterns (wc -c, grep -e).
	for _, a := range fields[1:] {
		if codeFlag(a, subs != nil) {
			return ""
		}
	}
	// A scope is compared as written, so a word the shell would unquote,
	// expand or glob could read as something the checks above never saw.
	for _, w := range fields[:n] {
		if strings.ContainsAny(w, "'\"\\$*?[]{}`") {
			return ""
		}
	}
	return strings.Join(fields[:n], " ")
}

// codeFlag reports a flag that hands a program code or a command to run:
// --eval or --exec, and, when short says so, -c or -e alone or in a
// cluster of short flags such as -ec.
func codeFlag(a string, short bool) bool {
	flag, _, _ := strings.Cut(a, "=")
	switch {
	case flag == "--eval" || flag == "--exec":
		return true
	case !short || strings.HasPrefix(a, "--") || !strings.HasPrefix(a, "-"):
		return false
	}
	return strings.ContainsAny(a[1:], "ce")
}

// resolvesOutsideRoots reports whether name, found on PATH as a shell would
// find it, is a file outside every workspace root: a scope for ls must not
// cover a planted ./venv/bin/ls. A relative PATH entry is the workspace's.
func (e *Engine) resolvesOutsideRoots(name string) bool {
	var roots []string
	if e.Roots != nil {
		for _, r := range e.Roots() {
			roots = append(roots, tools.RealPath(r))
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			return false
		}
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		real := tools.RealPath(p)
		for _, r := range roots {
			if rel, err := filepath.Rel(r, real); err == nil && filepath.IsLocal(rel) {
				return false
			}
			if rel, err := filepath.Rel(r, filepath.Clean(p)); err == nil && filepath.IsLocal(rel) {
				return false
			}
		}
		return true
	}
	// Not on PATH: a builtin such as pwd or echo, or nothing at all.
	return true
}

// suggestRule is an allow rule a person could add for exactly this command
// when no one-click scope is offered, or "" when none could match it.
func suggestRule(tool, subject string) string {
	if tool != "bash" || subject == "" || len(subject) > 200 || hasShellControl(subject) ||
		strings.ContainsAny(subject, "*?") || strings.TrimSpace(subject) != subject {
		return ""
	}
	return tool + "(" + subject + ")"
}
