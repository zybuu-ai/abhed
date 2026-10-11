package policy

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// maxScriptDepth bounds how deep scripts that run scripts are read.
const maxScriptDepth = 2

// scriptRead is what the scripts a command runs hold for the rules: the
// commands in them, why one cannot be read at all, and why one was not read
// in full.
type scriptRead struct {
	subjects    []string
	refuse, ask string
}

// readScripts reads the scripts canon runs as shell text, from the folder
// the command starts in, so deny and ask rules see the commands in them:
// bash ./s.sh is judged on what s.sh holds.
func (e *Engine) readScripts(canon tools.Canonical) scriptRead {
	var out scriptRead
	cwd := e.Cwd()
	if cwd == "" {
		return out
	}
	e.readScriptsIn(&out, canon, cwd, 0)
	return out
}

func (e *Engine) readScriptsIn(out *scriptRead, canon tools.Canonical, cwd string, depth int) {
	for _, s := range canon.Scripts() {
		if out.refuse != "" {
			return
		}
		name := s.Path
		if strings.HasPrefix(name, "~") {
			out.refuse = "it names the script " + name + " from a home folder the check cannot place"
			return
		}
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		for _, w := range canon.Writes() {
			if w == name || filepath.Join(cwd, w) == filepath.Clean(p) || w == p {
				out.refuse = "it writes the script " + name + " and then runs it"
				return
			}
		}
		text, shell, err := tools.ReadScript(p, s.Direct)
		switch {
		case errors.Is(err, tools.ErrScriptTooLong):
			out.ask = "the script " + name + " is too long to check against the rules"
			continue
		case err != nil && s.Direct:
			// A program the line may build first (make && ./bin/app), or one
			// no shell could read as a script either.
			continue
		case errors.Is(err, fs.ErrNotExist):
			out.refuse = "the script " + name + " does not exist yet"
			return
		case err != nil:
			out.refuse = "the script " + name + " cannot be read"
			return
		case !shell:
			continue
		}
		// What a script hides from the rules is put to a person, not refused:
		// build scripts often run built text, and before this none was read.
		sc := tools.CanonicalCommand(text)
		switch {
		case sc.Unparsed() || sc.Incomplete():
			out.ask = "the script " + name + " cannot be parsed as the shell reads it"
		case sc.Opaque() != "":
			out.ask = "the script " + name + " runs text no rule can read (" + sc.Opaque() + ")"
		case sc.ProgramHidden():
			out.ask = "the script " + name + " names a program by an expansion that edits its value"
		case sc.IFSSplit:
			out.ask = "the script " + name + " may split its words by a changed IFS"
		}
		segments, complete := commandSegments(text)
		if !complete {
			out.ask = "the script " + name + " is too long or complex to check each part against the rules"
		}
		out.subjects = append(append(out.subjects, segments...), sc.Commands()...)
		if depth+1 < maxScriptDepth {
			e.readScriptsIn(out, sc, cwd, depth+1)
		} else if len(sc.Scripts()) > 0 {
			out.ask = "the script " + name + " runs scripts nested too deep to check against the rules"
		}
	}
}
