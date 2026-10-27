package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// shellChoice is the interactive shell a terminal runs: its argv and the
// line it is judged and recorded as.
type shellChoice struct {
	argv []string
	line string
	zsh  bool
}

// bashChoice is bash at path, without the person's start-up files.
func bashChoice(path string) shellChoice {
	return shellChoice{argv: append([]string{path}, shellArgv[1:]...), line: TerminalCommand}
}

// zshLine is a zsh terminal as it is judged and recorded: no global or
// personal start-up file but Abhed's own.
const zshLine = "zsh -d -i"

// hostShell is the person's preferred shell where the process tier can run
// it: zsh where their login shell is zsh, else the newest bash, 4 or later
// where one is installed outside home (macOS's own /bin/bash is 3.2), else
// /bin/bash. A shell inside home is never chosen: home is closed to commands.
var hostShell = sync.OnceValue(chooseShell)

func chooseShell() shellChoice {
	home, _ := os.UserHomeDir()
	usable := func(p string) bool {
		if p == "" || !filepath.IsAbs(p) || home != "" && within(p, home) {
			return false
		}
		info, err := os.Stat(p)
		return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
	}
	login := os.Getenv("SHELL")
	if filepath.Base(login) == "zsh" && usable(login) {
		return shellChoice{argv: []string{login, "-d", "-i"}, line: zshLine, zsh: true}
	}
	var candidates []string
	if filepath.Base(login) == "bash" {
		candidates = append(candidates, login)
	}
	candidates = append(candidates, "/opt/homebrew/bin/bash", "/usr/local/bin/bash", "/usr/bin/bash", "/bin/bash")
	for _, c := range candidates {
		if usable(c) && bashMajor(c) >= 4 {
			return bashChoice(c)
		}
	}
	return bashChoice(shellArgv[0])
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// bashMajor is a bash's major version, or 0.
func bashMajor(path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--norc", "--noprofile", "-c", `echo "${BASH_VERSINFO[0]}"`).Output() // #nosec G204 -- a candidate shell, fixed arguments
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// zshrc is the one start-up file a zsh terminal reads: the sandbox's prompt,
// history in memory only, the toolchains' setup, and a missing program
// answered as bash's terminal answers it.
func zshrc(t Tier) string {
	// The prompt ends "$ " or "# " as bash's does, which the terminal's line
	// reading looks for; no mark for output without a newline.
	return `PS1='%F{8}(sandbox: ` + string(t) + `)%f %F{6}%1~%f %(!.#.$) '
HISTFILE=
unsetopt BEEP PROMPT_SP PROMPT_CR CHECK_JOBS CHECK_RUNNING_JOBS
setopt HUP
if [ -n "$ABHED_SHELL_SETUP" ]; then eval "$ABHED_SHELL_SETUP"; fi
unset ABHED_SHELL_SETUP PROMPT_COMMAND
command_not_found_handler() {
  if typeset -f command_not_found_handle >/dev/null; then command_not_found_handle "$@"; return $?; fi
  print -u2 "zsh: command not found: $1"; return 127
}
`
}

// loginEnvKeys are the person's settings a terminal keeps from their login
// environment: language, time zone, editor and colours, nothing that names a
// credential or changes what a program runs.
var loginEnvKeys = []string{"TZ", "EDITOR", "VISUAL", "COLORTERM", "CLICOLOR", "LSCOLORS", "LS_COLORS", "LESS", "TERM_PROGRAM"}

// loginEnv is that safe subset of this process's environment, with every
// LC_ setting.
func loginEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	for _, k := range loginEnvKeys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// ShellLiner is a sandbox whose terminal runs a shell other than bash's
// TerminalCommand, judged and recorded as the line it runs.
type ShellLiner interface {
	ShellLine() string
}

// TerminalCommandOf is the line sb's terminal is judged and recorded as.
func TerminalCommandOf(sb any) string {
	if l, ok := sb.(ShellLiner); ok {
		if line := l.ShellLine(); line != "" {
			return line
		}
	}
	return TerminalCommand
}
