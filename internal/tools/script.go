package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// MaxScriptBytes bounds a script the policy reads to judge a command that runs it.
const MaxScriptBytes = maxShellBytes

// ErrScriptTooLong marks a script past MaxScriptBytes.
var ErrScriptTooLong = errors.New("too long to read")

// scriptShells are the #! programs whose scripts are shell text the parser reads.
var scriptShells = map[string]bool{"sh": true, "bash": true, "dash": true, "ash": true, "ksh": true, "mksh": true, "zsh": true, "busybox": true}

// ReadScript reads the file p a command runs as shell text. For a program
// named by a path (direct), shell is false when it is a binary or another
// interpreter's script, which no shell reads. A file that is not a regular
// file, cannot be read or is past MaxScriptBytes is an error.
func ReadScript(p string, direct bool) (text string, shell bool, err error) {
	f, err := openNoWait(p)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		return "", false, err
	case !info.Mode().IsRegular():
		return "", false, fmt.Errorf("%s is not a regular file", path.Base(p))
	case info.Size() > MaxScriptBytes:
		return "", false, ErrScriptTooLong
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxScriptBytes+1))
	switch {
	case err != nil:
		return "", false, err
	case len(b) > MaxScriptBytes:
		return "", false, ErrScriptTooLong
	}
	if direct {
		head := b[:min(len(b), 512)]
		if bytes.IndexByte(head, 0) >= 0 {
			return "", false, nil // a binary
		}
		if line, ok := bytes.CutPrefix(head, []byte("#!")); ok {
			if !scriptShells[shebangProgram(string(line))] {
				return "", false, nil
			}
		}
	}
	return string(b), true, nil
}

// shebangProgram is the program a #! line names, past env and its options.
func shebangProgram(line string) string {
	line, _, _ = strings.Cut(line, "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	prog := path.Base(fields[0])
	if prog == "env" {
		for _, w := range fields[1:] {
			if !strings.HasPrefix(w, "-") && !strings.Contains(w, "=") {
				return path.Base(w)
			}
		}
		return ""
	}
	return prog
}
