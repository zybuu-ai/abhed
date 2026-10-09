package driver

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Line is the shell command line the request runs: Command as given, or
// Argv with each word quoted for a POSIX shell. The process and container
// drivers run exactly this line, and the control plane screens and records
// it, so what is judged is what runs.
func (r ExecRequest) Line() (string, error) {
	if r.Command != "" {
		return r.Command, nil
	}
	return QuoteArgv(r.Argv)
}

// QuoteArgv joins argv into one line a POSIX shell (and bash) splits back
// into exactly argv. A word with a NUL byte cannot be passed and is refused.
func QuoteArgv(argv []string) (string, error) {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if strings.IndexByte(a, 0) >= 0 {
			return "", Errorf(ErrInvalid, "argv[%d] cannot be quoted: it holds a NUL byte", i)
		}
		q, err := syntax.Quote(a, syntax.LangPOSIX)
		if err != nil {
			// Not printable, such as a newline or a tab: single quotes keep
			// every byte but NUL as it is, in any POSIX shell.
			q = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts[i] = q
	}
	return strings.Join(parts, " "), nil
}
