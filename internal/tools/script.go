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
	}
	// A binary or another interpreter's script is known by its start, whatever its size.
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	head = head[:n]
	if direct {
		if bytes.IndexByte(head, 0) >= 0 {
			return "", false, nil
		}
		if line, ok := bytes.CutPrefix(head, []byte("#!")); ok && !scriptShells[shebangProgram(string(line))] {
			return "", false, nil
		}
	}
	rest, err := io.ReadAll(io.LimitReader(f, MaxScriptBytes+1-int64(n)))
	if err != nil {
		return "", false, err
	}
	head = append(head, rest...)
	if len(head) > MaxScriptBytes {
		return "", false, ErrScriptTooLong
	}
	return string(head), true, nil
}

// shebangProgram is the program a #! line names, past env and its options.
// ScriptShell is the program a script's #! line names, "" when it has none.
func ScriptShell(text string) string {
	if line, ok := strings.CutPrefix(text, "#!"); ok {
		return shebangProgram(line)
	}
	return ""
}

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
