package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// serverFiles confines an MCP server's files: home hidden but its install and
// its own home, so it cannot read ~/.ssh or plant an autostart.
type serverFiles struct {
	realHome   string   // the person's home, hidden
	home       string   // the server's own, its HOME
	prefix     string   // the server's install folder under the home, read-only; "" when outside it
	workspaces []string // writable, as the session's
	// scripts are the folders, under the home, of files the server's
	// configured arguments name, such as the script an interpreter runs:
	// read-only, as its install folder is.
	scripts []string
	// files are single files shown read-only, where their folder is not:
	// a program or script directly in a top-level folder of the home.
	files []string
}

// shown are what under the home the server reads: its install folder, its
// scripts' folders and the single files shown without their folder.
func (f serverFiles) shown() []string {
	var out []string
	if f.prefix != "" {
		out = append(out, f.prefix)
	}
	return append(append(out, f.scripts...), f.files...)
}

var serverNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// serverDirName is a server's folder name under ~/.abhed/mcp.
func serverDirName(name string) string { return serverNameChars.ReplaceAllString(name, "_") }

// outsideHome are the paths of ps not in the person's home, which a confined
// server never sees.
func (f serverFiles) outsideHome(ps []string) []string {
	var out []string
	for _, p := range ps {
		if !slices.ContainsFunc(PathForms(p), func(q string) bool { return insideAny(q, PathForms(f.realHome)) }) {
			out = append(out, p)
		}
	}
	return out
}

// serverFilesFor works out a server's confinement and makes its home.
func (s *Process) serverFilesFor(name string, argv, env []string) (serverFiles, error) {
	return serverFilesIn(name, argv, env, s.workspaces())
}

// serverFilesIn is a server's confinement with workspaces writable, its home made.
func serverFilesIn(name string, argv, env, workspaces []string) (serverFiles, error) {
	program := argv[0]
	real, err := os.UserHomeDir()
	if err != nil {
		return serverFiles{}, fmt.Errorf("sandbox: no home folder to hide from the server: %w", err)
	}
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r
	}
	f := serverFiles{realHome: real, workspaces: workspaces}
	f.home = filepath.Join(real, stateDir, "mcp", serverDirName(name), "home")
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		return serverFiles{}, fmt.Errorf("sandbox: the server's own home: %w", err)
	}
	resolved := resolveIn(program, env)
	f.prefix = installPrefix(real, resolved)
	// Its install folder is shown to it: never Abhed's state, nor a folder
	// holding it or credentials; a top-level folder only as the program.
	if state := filepath.Join(real, stateDir); f.prefix != "" && (insideAny(f.prefix, []string{state}) || insideAny(state, []string{f.prefix})) {
		return serverFiles{}, fmt.Errorf("sandbox: the server's program %s is installed in %s, which holds Abhed's state (%s) and is never shown to a server; "+
			"install it in a folder of its own, or set \"host_files\": true on the server", program, f.prefix, state)
	}
	if f.prefix != "" {
		switch homeShown(real, f.prefix) {
		case showNothing:
			return serverFiles{}, fmt.Errorf("sandbox: the server's program %s is installed in %s, which holds Abhed's state or credentials and is never shown to a server; "+
				"install it in a folder of its own, or set \"host_files\": true on the server", program, f.prefix)
		case showFile:
			f.prefix, f.files = "", []string{resolved}
		}
	}
	scripts, files := scriptFolders(real, argv[1:])
	f.scripts, f.files = scripts, append(f.files, files...)
	return f, nil
}

// scriptFolders are the folders of the existing files under home that args
// name by absolute path, such as `python3 ~/tools/mcp/server.py`: the
// server's own code, from the configuration; files are the files themselves
// where their folder is not shown (homeShown).
func scriptFolders(home string, args []string) (folders, files []string) {
	for _, a := range args {
		if !filepath.IsAbs(a) {
			continue
		}
		p := a
		if r, err := filepath.EvalSymlinks(a); err == nil {
			p = r
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || !insideAny(p, []string{home}) {
			continue
		}
		switch homeShown(home, filepath.Dir(p)) {
		case showFolder:
			if !slices.Contains(folders, filepath.Dir(p)) {
				folders = append(folders, filepath.Dir(p))
			}
		case showFile:
			if !slices.Contains(files, p) {
				files = append(files, p)
			}
		}
	}
	return folders, files
}

// What of a folder under home a server is shown.
const (
	showNothing = iota // credentials or Abhed's state: not even the file
	showFile           // a top-level or broad folder: only the named file
	showFolder         // a folder of its own: all of it, read-only
)

// homeShown is how much of dir, under home, a server may read: nothing in a
// credential folder or Abhed's state, nor in home or another folder that
// holds one of them (a file named there may be a credential); only the file
// itself in a folder directly in home (~/Downloads, ~/Projects, ~/bin,
// ~/.local), in ~/.local/bin or ~/.cargo/bin, and in a cloud drive's root
// or one folder below it (iCloud Drive and other apps' iCloud folders in
// ~/Library/Mobile Documents, ~/Library/CloudStorage's drives); otherwise
// the folder. Paths are compared without case, as macOS's disk does.
func homeShown(home, dir string) int {
	state := filepath.Join(home, stateDir)
	if insideAny(dir, []string{state}) {
		return showNothing
	}
	holds := insideAny(state, []string{dir})
	for _, c := range HomeSecrets {
		sec := filepath.Join(home, c)
		if insideAny(dir, []string{sec}) {
			return showNothing
		}
		holds = holds || insideAny(sec, []string{dir})
	}
	if holds {
		return showNothing
	}
	below := func(root string, max int) bool {
		rest, ok := Within(dir, root)
		return ok && len(rest) <= max
	}
	switch {
	case below(home, 1),
		below(filepath.Join(home, ".local", "bin"), 0), below(filepath.Join(home, ".cargo", "bin"), 0),
		// A cloud drive's root and the folders directly in it: a drive
		// (CloudStorage/<drive>) or an app's iCloud folder (Mobile
		// Documents/<container>), and one level below.
		below(filepath.Join(home, "Library", "Mobile Documents"), 2),
		below(filepath.Join(home, "Library", "CloudStorage"), 2):
		return showFile
	}
	return showFolder
}

// serverProgramMissing says a host server's program is not there to run: a
// sandbox would start, and the server's first write would only meet a closed pipe.
func serverProgramMissing(program string, env []string) error {
	if strings.ContainsRune(program, '/') {
		if fi, err := os.Stat(program); err == nil && !fi.IsDir() {
			return nil
		}
		return fmt.Errorf("sandbox: the server's program %s is not found", program)
	}
	if resolveIn(program, env) != "" {
		return nil
	}
	return fmt.Errorf("sandbox: the server's program %s is not found on its PATH", program)
}

// resolveIn finds program on the PATH env names, links followed; "" when it
// cannot be found.
func resolveIn(program string, env []string) string {
	path := program
	if !strings.ContainsRune(program, '/') {
		search := os.Getenv("PATH")
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				search = v
			}
		}
		path = ""
		for _, dir := range filepath.SplitList(search) {
			if !filepath.IsAbs(dir) {
				continue
			}
			if fi, err := os.Stat(filepath.Join(dir, program)); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
				path = filepath.Join(dir, program)
				break
			}
		}
	}
	if path == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// installPrefix is the folder a home program was installed in, never all of
// home, ~/.local or ~/.cargo, which hold keyrings and tokens.
func installPrefix(home, program string) string {
	if program == "" || !insideAny(program, []string{home}) {
		return ""
	}
	dir := filepath.Dir(program)
	if !strings.EqualFold(filepath.Base(dir), "bin") {
		return dir
	}
	parent := filepath.Dir(dir)
	for _, broad := range []string{home, filepath.Join(home, ".local"), filepath.Join(home, ".cargo")} {
		// Compared without case, as macOS's disk does.
		if rest, ok := Within(parent, broad); ok && len(rest) == 0 {
			return dir
		}
	}
	return parent
}

// bwrapArgs hide the home but for what the server is given, and the
// workspace's state; they come after the root is bound.
func (f serverFiles) bwrapArgs() []string {
	args := []string{"--tmpfs", f.realHome}
	for _, d := range f.shown() {
		args = append(args, "--ro-bind", d, d)
	}
	args = append(args, "--bind", f.home, f.home)
	for _, ws := range f.workspaces {
		if _, err := os.Stat(ws); err != nil {
			continue
		}
		args = append(args, "--bind", ws, ws, "--tmpfs", filepath.Join(ws, stateDir))
	}
	return args
}

// seatbeltRules deny reading and writing home but what the server is given, and
// the workspace's state.
func (f serverFiles) seatbeltRules() string {
	if f.realHome == "" {
		return "" // not confined: host_files
	}
	var b strings.Builder
	b.WriteString(";; The home folder, but for the server's own and its install folder.\n")
	for _, h := range PathForms(f.realHome) {
		fmt.Fprintf(&b, "(deny file-read-data file-write* (subpath %q))\n", h)
	}
	for _, d := range f.shown() {
		fmt.Fprintf(&b, "(allow file-read-data (subpath %q))\n", d)
	}
	fmt.Fprintf(&b, "(allow file-read-data file-write* (subpath %q))\n", f.home)
	for _, ws := range f.workspaces {
		fmt.Fprintf(&b, "(allow file-read-data file-write* (subpath %q))\n", ws)
	}
	for _, ws := range f.workspaces {
		fmt.Fprintf(&b, "(deny file-read-data file-write* (regex #\"^%s/%s(/|$)\"))\n", regexp.QuoteMeta(ws), foldRegex(stateDir))
	}
	return b.String()
}

// foldRegex matches s in any case, for a disk that ignores it.
func foldRegex(s string) string {
	var b strings.Builder
	for _, r := range s {
		lo, up := strings.ToLower(string(r)), strings.ToUpper(string(r))
		if lo == up {
			b.WriteString(regexp.QuoteMeta(string(r)))
			continue
		}
		b.WriteString("[" + lo + up + "]")
	}
	return b.String()
}

// serverHomeEnv points the server's home at its own.
func serverHomeEnv(env []string, home string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "HOME=") {
			out = append(out, kv)
		}
	}
	return append(out, "HOME="+home)
}

// serverFilesOnly runs a server with its files confined and its network as
// it is: the allowlist is off, so the server keeps the network it had.
func (s *Process) serverFilesOnly(ctx context.Context, f serverFiles, argv, env []string) (*exec.Cmd, error) {
	env = serverHomeEnv(serverEnv(env), f.home)
	var cmd *exec.Cmd
	switch s.backend {
	case "sandbox-exec":
		profile := "(version 1)\n(allow default)\n" + denyLaunchServices + f.seatbeltRules()
		cmd = exec.CommandContext(ctx, "sandbox-exec", append([]string{"-p", profile}, argv...)...) // #nosec G204 -- the configured server, confined
	case "bwrap":
		// bwrap as root keeps root's capabilities over the host, so a
		// confinement there would be in name only.
		if err := serverRootRefusal(os.Getuid(), os.Geteuid()); err != nil {
			return nil, fmt.Errorf("%w, or set host_files on the server to run it unconfined", err)
		}
		if !s.bwrapFreshOK() {
			return nil, errors.New("sandbox: bubblewrap cannot mount a private /proc here, so a server's files cannot be confined; " +
				"set host_files on the server to run it unconfined")
		}
		args := append([]string{"--die-with-parent", "--unshare-pid", "--dev-bind", "/", "/", "--proc", "/proc"}, serverRunHides()...)
		args = append(append(args, f.bwrapArgs()...), "--")
		cmd = exec.CommandContext(ctx, "bwrap", append(args, argv...)...) // #nosec G204 -- the configured server, confined
	default:
		return nil, fmt.Errorf("sandbox: the %s backend cannot confine a server's files", s.backend)
	}
	cmd.Env = env
	return cmd, nil
}
