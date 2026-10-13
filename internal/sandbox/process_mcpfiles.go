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
func (s *Process) serverFilesFor(name, program string, env []string) (serverFiles, error) {
	return serverFilesIn(name, program, env, s.workspaces())
}

// serverFilesIn is a server's confinement with workspaces writable, its home made.
func serverFilesIn(name, program string, env, workspaces []string) (serverFiles, error) {
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
	f.prefix = installPrefix(real, resolveIn(program, env))
	return f, nil
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
	if filepath.Base(dir) != "bin" {
		return dir
	}
	parent := filepath.Dir(dir)
	for _, broad := range []string{home, filepath.Join(home, ".local"), filepath.Join(home, ".cargo")} {
		if parent == broad {
			return dir
		}
	}
	return parent
}

// bwrapArgs hide the home but for what the server is given, and the
// workspace's state; they come after the root is bound.
func (f serverFiles) bwrapArgs() []string {
	args := []string{"--tmpfs", f.realHome}
	if f.prefix != "" {
		args = append(args, "--ro-bind", f.prefix, f.prefix)
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
	if f.prefix != "" {
		fmt.Fprintf(&b, "(allow file-read-data (subpath %q))\n", f.prefix)
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
