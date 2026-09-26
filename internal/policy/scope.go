package policy

import (
	"regexp"
	"strings"
)

// scopeTools are the only programs offered a one-click "always allow": no word
// on the line can make them run a command. git still runs the repository's own
// hooks and config, which the sandbox tier contains. A tool with subcommands
// maps to the ones allowed; nil means its scope is the program alone.
var scopeTools = map[string]map[string]bool{
	// Not config (aliases run commands), fetch (--upload-pack), grep (-O runs a pager)
	// or remote (update fetches; add -f runs a remote helper the URL names).
	"git": set("status", "diff", "log", "show", "branch", "add", "commit", "restore", "switch",
		"checkout", "stash", "rev-parse", "ls-files", "blame", "tag"),
	"ls": nil, "cat": nil, "head": nil, "tail": nil, "wc": nil, "pwd": nil, "echo": nil,
	"which": nil, "file": nil, "stat": nil, "du": nil, "df": nil, "tree": nil, "grep": nil,
	"jq": nil, "diff": nil, "uniq": nil, "cut": nil, "tr": nil,
	// File operations the agent can already do with write. Not sort (--compress-program)
	// or rg (--pre), which run a program named in their arguments.
	"mkdir": nil, "touch": nil, "cp": nil, "mv": nil,
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
	}
	for _, a := range fields[1:] {
		if codeFlag(a) {
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

// codeFlag reports a flag that hands a program code or a command to run,
// alone or in a cluster of short flags such as -ec.
func codeFlag(a string) bool {
	flag, _, _ := strings.Cut(a, "=")
	switch {
	case flag == "--eval" || flag == "--exec":
		return true
	case strings.HasPrefix(a, "--") || !strings.HasPrefix(a, "-"):
		return false
	}
	return strings.ContainsAny(a[1:], "ce")
}
