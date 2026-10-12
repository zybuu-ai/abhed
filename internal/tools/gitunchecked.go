package tools

import (
	"path"
	"strings"
)

// GitUnchecked says why the command runs git code that no rule can read: an
// alias set on its command line, or a subcommand that is not git's own (an
// alias or extension), other than the extensions opted in to; "" when none.
// Destructive asks about these; where no one can be asked, they are refused.
func (c Canonical) GitUnchecked(gitExtensions map[string]bool) string {
	if len(gitExtensions) > 0 && aliasCapable(c.command) {
		gitExtensions = nil
	}
	forms := append([]string{c.command, c.Text}, c.shell.commands...)
	for _, form := range forms {
		for _, line := range []string{form, collapseSubstitutions(form)} {
			if why := gitUncheckedParts(line, gitExtensions); why != "" {
				return why
			}
		}
	}
	return ""
}

func gitUncheckedParts(command string, gitExtensions map[string]bool) string {
	for _, part := range strings.Split(shellBreaks.Replace(command), "\n") {
		words := strings.Fields(shellQuotes.Replace(part))
		pos := position{on: true}
		for i, w := range words {
			run := pos.next(w)
			if strings.TrimSuffix(CommandName(path.Base(w)), ".exe") != "git" && (!run || w != gitSubstitution) {
				continue
			}
			// gitDiscards reports an alias or a foreign subcommand before any discard.
			if what, ok := gitDiscards(words[i+1:], run, gitExtensions); ok && gitUncheckedReason(what) {
				return what
			}
		}
	}
	return ""
}

func gitUncheckedReason(what string) bool {
	return what == gitAliasReason || strings.HasSuffix(what, gitForeignSuffix)
}
