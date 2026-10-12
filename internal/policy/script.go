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
// bash ./s.sh is judged on what s.sh holds. A script the line may run from
// another folder, or may write before it runs, is not read but refused.
func (e *Engine) readScripts(canon tools.Canonical) scriptRead {
	var out scriptRead
	cwd := e.Cwd()
	if cwd == "" {
		return out
	}
	e.readScriptsIn(&out, canon, cwd, 0, lineWrites{})
	return out
}

// lineWrites are the files the lines around a script write, and the PATH
// they set, which a script started from them inherits.
type lineWrites struct {
	files    []string
	hidden   string
	pathSet  bool
	pathDirs []string
}

// readScriptsIn reads the scripts canon runs and reports whether canon, run
// in the caller's shell as a sourced script is, may change its folder.
func (e *Engine) readScriptsIn(out *scriptRead, canon tools.Canonical, cwd string, depth int, outer lineWrites) (moves bool) {
	writes := lineWrites{
		files:    append(append([]string(nil), outer.files...), canon.Writes()...),
		hidden:   firstNonEmpty(outer.hidden, canon.WriteHidden()),
		pathSet:  outer.pathSet || canon.PathSet(),
		pathDirs: append(append([]string(nil), outer.pathDirs...), canon.PathDirs()...),
	}
	moves = canon.MovesFolder()
	// In a script these ask, as what else a script hides does; on the line
	// itself they refuse.
	if canon.ScriptsCut() {
		if depth > 0 {
			out.ask = "a script it runs runs more scripts than the check reads"
		} else {
			out.refuse = "it runs more scripts than the check reads"
			return moves
		}
	}
	scripts, why := pathScripts(canon, writes)
	if why != "" {
		if depth == 0 {
			out.refuse = "it " + why
			return moves
		}
		out.ask = "a script it runs " + why
	}
	for _, s := range scripts {
		if out.refuse != "" {
			return moves
		}
		name := s.Path
		if strings.HasPrefix(name, "~") {
			out.refuse = "it names the script " + name + " from a home folder the check cannot place"
			return moves
		}
		p := name
		if !filepath.IsAbs(p) {
			if moves && depth > 0 {
				out.ask = "a script it runs names the script " + name + " by a relative name after a cd may have changed the folder"
				continue
			}
			if moves {
				out.refuse = "it runs the script " + name + " by a relative name after a cd, or a script it sources, may have changed the folder"
				return moves
			}
			p = filepath.Join(cwd, p)
		}
		if writes.hidden != "" {
			out.refuse = "it runs the script " + name + " and may write it first (" + writes.hidden + ")"
			return moves
		}
		for _, w := range writes.files {
			if writesScript(w, name) {
				out.refuse = "it writes the script " + name + " and then runs it"
				return moves
			}
		}
		text, shell, err := tools.ReadScript(p, s.Direct)
		switch {
		case errors.Is(err, tools.ErrScriptTooLong):
			out.ask = "the script " + name + " is too long to check against the rules"
			continue
		case errors.Is(err, fs.ErrNotExist) && s.Startup:
			continue // a shell starts without it
		case err != nil && s.Direct:
			// A program the line may build first (make && ./bin/app), or one
			// no shell could read as a script either.
			continue
		case errors.Is(err, fs.ErrNotExist):
			out.refuse = "the script " + name + " does not exist yet"
			return moves
		case err != nil:
			out.refuse = "the script " + name + " cannot be read"
			return moves
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
		case s.Startup && sc.SetsStartupDir():
			out.ask = "the start-up file " + name + " moves where the shell reads its other start-up files"
		}
		segments, complete := commandSegments(text)
		if !complete {
			out.ask = "the script " + name + " is too long or complex to check each part against the rules"
		}
		out.subjects = append(append(out.subjects, segments...), sc.Commands()...)
		inner := sc.MovesFolder()
		if depth+1 < maxScriptDepth {
			inner = e.readScriptsIn(out, sc, cwd, depth+1, writes)
		} else if len(sc.Scripts()) > 0 {
			out.ask = "the script " + name + " runs scripts nested too deep to check against the rules"
		}
		// A sourced script runs in this shell: a cd in it moves what follows.
		moves = moves || (s.Sourced && inner)
	}
	return moves
}

// pathScripts are the scripts canon runs, with each bare program looked up in
// the folders a PATH the line sets names; why says what cannot be read.
func pathScripts(canon tools.Canonical, writes lineWrites) (scripts []tools.Script, why string) {
	scripts = canon.Scripts()
	if why := canon.StartupHidden(); why != "" {
		return nil, "names " + why
	}
	if !writes.pathSet {
		return scripts, ""
	}
	for _, s := range scripts {
		if s.Sourced && !strings.Contains(s.Path, "/") {
			return nil, "sources " + s.Path + " by a name a PATH it sets may find in another folder"
		}
	}
	if len(writes.pathDirs) > 0 && canon.BareCut() {
		return nil, "names more programs than the check looks up on the PATH it sets"
	}
	scripts = append([]tools.Script(nil), scripts...)
	for _, name := range canon.Bare() {
		for _, dir := range writes.pathDirs {
			p := filepath.Join(dir, name)
			if writes.hidden != "" {
				return nil, "runs " + name + " through the PATH it sets and may write it first (" + writes.hidden + ")"
			}
			for _, w := range writes.files {
				if writesScript(w, p) {
					return nil, "writes " + p + " and then runs it as " + name + " through the PATH it sets"
				}
			}
			// Read as a program named by a path: one that is not there is skipped.
			scripts = append(scripts, tools.Script{Path: p, Direct: true})
		}
	}
	return scripts, ""
}

// writesScript reports a written file that is the script or a folder on its
// path, by name in any case, since a link or another spelling may reach it.
func writesScript(written, script string) bool {
	base := filepath.Base(filepath.Clean(written))
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return false
	}
	for _, part := range strings.Split(filepath.Clean(script), string(filepath.Separator)) {
		if part != "" && part != "." && part != ".." && strings.EqualFold(part, base) {
			return true
		}
	}
	return false
}
