package policy

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
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
	subjects     []string
	refuse, ask  string
	trustedReads int
}

// scriptEnv is the environment a script runs in, with the HOME commands get
// when the line does not say.
func (e *Engine) scriptEnv(env map[string]string) map[string]string {
	if _, ok := env["HOME"]; ok {
		return env
	}
	h, visible := e.home()
	if !visible || !filepath.IsAbs(h) {
		return env
	}
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out["HOME"] = h
	return out
}

// readScripts reads the scripts canon runs as shell text, from the folder
// the command starts in, so deny and ask rules see the commands in them:
// bash ./s.sh is judged on what s.sh holds. A script the line may run from
// another folder, or may write before it runs, is not read but refused.
func (e *Engine) readScripts(canon tools.Canonical) scriptRead {
	var out scriptRead
	cwd := ""
	if e.Cwd != nil {
		cwd = e.Cwd()
	}
	if cwd == "" {
		if e.SandboxFiles {
			out.refuse = SandboxScripts(canon)
			return out
		}
		// Nothing is read; a start-up file a shell reads still cannot be passed over.
		if names := canon.HomeStartups(); len(names) > 0 {
			out.refuse = "it starts a shell that reads its start-up files (" + strings.Join(names, ", ") + ") from a HOME the check cannot read"
		}
		for _, s := range canon.Scripts() {
			if s.Startup && out.refuse == "" {
				out.refuse = "it starts a shell that reads start-up files from a folder the check cannot read (" + s.Path + ")"
			}
		}
		if why := canon.StartupHidden(); why != "" && out.refuse == "" {
			out.refuse = "it names " + why
		}
		return out
	}
	// A line that sets the shell's environment can steer a trusted file.
	e.readScriptsIn(&out, canon, cwd, 0, lineWrites{noBaseline: !canon.BaselineApplies()}, false)
	return out
}

// systemBins are folders a sandbox's image holds its programs in: a program
// named by a path there is taken as the image's, not a script.
var systemBins = map[string]bool{"/bin": true, "/sbin": true, "/usr/bin": true, "/usr/sbin": true,
	"/usr/local/bin": true, "/usr/local/sbin": true}

// SandboxScripts says why canon would run a file the check cannot read in a
// sandbox (a script, a sourced or start-up file, a PATH it sets), or "".
func SandboxScripts(canon tools.Canonical) string {
	if names := canon.HomeStartups(); len(names) > 0 {
		return "it starts a shell that reads its start-up files (" + strings.Join(names, ", ") + ") from a HOME the check cannot read"
	}
	if why := canon.StartupHidden(); why != "" {
		return "it names " + why
	}
	if why := canon.ScriptHidden(); why != "" {
		return why
	}
	if canon.ScriptsCut() {
		return "it runs more scripts than the check reads"
	}
	for _, s := range canon.Scripts() {
		switch {
		case s.Startup:
			return "it starts a shell that reads start-up files from a folder the check cannot read (" + s.Path + ")"
		case s.Direct && path.IsAbs(s.Path) && systemBins[path.Dir(path.Clean(s.Path))]:
		default:
			return "it runs " + s.Path + ", which the check cannot read in the sandbox"
		}
	}
	if canon.PathSet() && len(canon.Bare()) > 0 && (len(canon.PathDirs()) > 0 || canon.PathUnknown()) {
		return "it runs " + canon.Bare()[0] + " through a PATH it sets, which the check cannot read in the sandbox"
	}
	return ""
}

// home is the HOME the sandbox gives a command, and whether the host can read it.
func (e *Engine) home() (string, bool) {
	if e.Home != nil {
		return e.Home()
	}
	h := os.Getenv("HOME")
	return h, h != ""
}

// homeScripts are the start-up files the shells canon starts read from the
// HOME the sandbox gives them; why says when that HOME cannot be read.
func (e *Engine) homeScripts(canon tools.Canonical, noBaseline bool) (scripts []tools.Script, why string) {
	names := canon.HomeStartups()
	if len(names) == 0 {
		return nil, ""
	}
	dir, visible := e.home()
	if visible && dir == "" {
		return nil, "" // a home not made yet, which starts empty
	}
	if !visible || !filepath.IsAbs(dir) {
		return nil, "starts a shell that reads its start-up files (" + strings.Join(names, ", ") + ") from a HOME the check cannot read"
	}
	env := canon.Env()
	for _, n := range names {
		p := filepath.Join(dir, n)
		scripts = append(scripts, tools.Script{Path: p, Startup: true, Env: env, Trusted: !noBaseline && e.trustedStartup(p)})
	}
	return scripts, ""
}

// lineWrites are the files the lines around a script write, and the PATH
// they set, which a script started from them inherits.
type lineWrites struct {
	files    []string
	hidden   string
	pathSet  bool
	pathDirs []string
	// noBaseline turns the person's baseline off, for a line that sets the
	// environment its shells get.
	noBaseline bool
}

// maxTrustedReads bounds the trusted files one command reads through.
const maxTrustedReads = 64

// readScriptsIn reads the scripts canon runs and reports whether canon, run
// in the caller's shell as a sourced script is, may change its folder.
func (e *Engine) readScriptsIn(out *scriptRead, canon tools.Canonical, cwd string, depth int, outer lineWrites, trusted bool) (moves bool) {
	writes := lineWrites{
		files:      append(append([]string(nil), outer.files...), canon.Writes()...),
		hidden:     firstNonEmpty(outer.hidden, canon.WriteHidden()),
		pathSet:    outer.pathSet || canon.PathSet(),
		pathDirs:   append(append([]string(nil), outer.pathDirs...), canon.PathDirs()...),
		noBaseline: outer.noBaseline,
	}
	moves = canon.MovesFolder()
	// A file sourced or run by a name the check cannot resolve is not read.
	// In a trusted file the name is the person's own, and only what resolves is read.
	if why := canon.ScriptHidden(); why != "" && !trusted {
		if depth == 0 {
			out.refuse = why
		} else {
			out.refuse = "a script it runs: " + why
		}
		return moves
	}
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
	if why == "" {
		// Only the shell the line itself starts may have its files trusted.
		home, hwhy := e.homeScripts(canon, writes.noBaseline || depth > 0 || trusted)
		scripts, why = append(scripts, home...), hwhy
	}
	if why != "" {
		if depth == 0 {
			out.refuse = "it " + why
			return moves
		}
		out.ask = "a script it runs " + why
	}
	zshenv := false
	for i := 0; i < len(scripts); i++ {
		s := scripts[i]
		if out.refuse != "" {
			return moves
		}
		name := s.Path
		p := name
		if strings.HasPrefix(name, "~") {
			home, ok := s.Env["HOME"]
			if !ok {
				if h, visible := e.home(); visible && filepath.IsAbs(h) {
					home, ok = h, true
				}
			}
			if !ok || name != "~" && !strings.HasPrefix(name, "~/") {
				out.refuse = "it names the script " + name + " from a home folder the check cannot place"
				return moves
			}
			p = home + name[1:]
		}
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
		// A zsh script reads .zshenv from the HOME it inherits, as zsh -c does.
		if s.Direct && !zshenv && tools.ScriptShell(text) == "zsh" && !canon.SetsStartupDir() {
			zshenv = true
			switch dir, visible := e.home(); {
			case visible && dir == "":
			case !visible || !filepath.IsAbs(dir):
				out.refuse = "it runs the zsh script " + name + ", which reads .zshenv from a HOME the check cannot read"
				return moves
			default:
				zp := filepath.Join(dir, ".zshenv")
				scripts = append(scripts, tools.Script{Path: zp, Startup: true, Env: s.Env, Trusted: !writes.noBaseline && e.trustedStartup(zp)})
			}
		}
		sc := tools.CanonicalCommandEnv(text, e.scriptEnv(s.Env))
		// A file the baseline holds as it is: its text is the person's own;
		// what it sources and runs is still read.
		if s.Trusted || trusted && !writes.noBaseline && e.trustedStartup(p) {
			if out.trustedReads++; out.trustedReads > maxTrustedReads {
				out.ask = "the trusted start-up files source more files than the check reads"
				continue
			}
			moves = e.readScriptsIn(out, sc, cwd, depth, writes, true) && s.Sourced || moves
			continue
		}
		// What a script hides from the rules is put to a person, not refused:
		// build scripts often run built text, and before this none was read.
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
		// What a script starts is not the line's own shell: no baseline in it.
		nested := writes
		nested.noBaseline = true
		if depth+1 < maxScriptDepth {
			inner = e.readScriptsIn(out, sc, cwd, depth+1, nested, false)
		} else if sc.ScriptHidden() != "" || sc.ScriptsCut() || sc.StartupHidden() != "" || len(sc.HomeStartups()) > 0 {
			// What is not even named is refused, as a hidden script one level up is.
			out.refuse = "the script " + name + " runs scripts nested too deep to check against the rules, some not named"
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
