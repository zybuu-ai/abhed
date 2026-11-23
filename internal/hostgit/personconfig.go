package hostgit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// writableAreas are the host folders the sandbox's commands can write
// besides the workspace; tests set it.
var writableAreas = sandbox.WritableAreas

// PersonConfigWritable says why the person's own git configuration cannot
// be trusted as theirs for git run on the host from dir. Its files are the
// system file, the global one (the file GIT_CONFIG_GLOBAL names where the
// person set it, else ~/.gitconfig and the XDG file) and every file they
// include, whatever the include's condition (a branch or folder can change
// after the check), followed to the end. One is refused when it, or the
// folder it is in, lies, links followed, in one of areas, which commands
// can write; when it is a hard link (another name for it may be in reach),
// owned by someone but the person or root, or writable by others; or when
// it cannot be read. Host git takes its SSH command, askpass and
// credential helpers from these files, so one a command could write would
// hand it a program.
func PersonConfigWritable(ctx context.Context, dir string, areas []string) error {
	files, err := personConfigFiles(ctx, dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		for _, a := range areas {
			if a == "" {
				continue
			}
			for _, p := range []string{f, filepath.Dir(f)} {
				if within(p, a) {
					return fmt.Errorf("your global git configuration (or the system one, or a file either includes) %s is in %s, "+
						"which commands can write", f, a)
				}
			}
		}
		if why := untrustedFile(f); why != "" {
			return fmt.Errorf("your global git configuration (or the system one, or a file either includes) %s %s", f, why)
		}
	}
	return nil
}

// personEnv is what of the person's environment chooses git's own
// configuration files (GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM,
// GIT_CONFIG_NOSYSTEM), which Env drops and the broker gives back to host
// git, once PersonConfigWritable has checked the files they name.
func personEnv() []string {
	var out []string
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for _, kv := range testEnv {
		if strings.HasPrefix(kv, "GIT_CONFIG_NOSYSTEM=") {
			out = append(out, kv)
		}
	}
	return out
}

// maxConfigFiles bounds the files the walk checks; past it, it refuses.
var maxConfigFiles = 200

// personConfigFiles are the files personEnv's git reads as the person's
// configuration, and every file they include, whatever its condition.
func personConfigFiles(ctx context.Context, dir string) ([]string, error) {
	env := map[string]string{}
	for _, kv := range personEnv() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	home, _ := os.UserHomeDir()
	// git opens a relative one from its own folder, the repository's: not checked here.
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if v := env[k]; v != "" && !filepath.IsAbs(v) {
			return nil, fmt.Errorf("%s names %q, a relative path, which git would open from the repository's folder; make it absolute", k, v)
		}
	}
	var roots []string
	if env["GIT_CONFIG_NOSYSTEM"] == "" {
		if f := env["GIT_CONFIG_SYSTEM"]; f != "" {
			roots = append(roots, f)
		} else {
			roots = append(roots, "/etc/gitconfig")
			if p, err := GitPath(dir); err == nil {
				// A git installed under a prefix reads <prefix>/etc/gitconfig.
				roots = append(roots, filepath.Join(filepath.Dir(filepath.Dir(sandbox.RealPath(p))), "etc", "gitconfig"))
			}
		}
	}
	if f, ok := env["GIT_CONFIG_GLOBAL"]; ok {
		if f != "" {
			roots = append(roots, f)
		}
	} else {
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" && home != "" {
			xdg = filepath.Join(home, ".config")
		}
		if xdg != "" {
			roots = append(roots, filepath.Join(xdg, "git", "config"))
		}
		if home != "" {
			roots = append(roots, filepath.Join(home, ".gitconfig"))
		}
	}
	seen := map[string]bool{}
	var out []string
	for len(roots) > 0 {
		f := roots[0]
		roots = roots[1:]
		if f = filepath.Clean(f); seen[f] {
			continue
		}
		if len(seen) >= maxConfigFiles {
			return nil, fmt.Errorf("your git configuration includes more than %d files, so not all of them were checked", maxConfigFiles)
		}
		seen[f] = true
		out = append(out, f)
		if _, err := os.Stat(f); err != nil {
			continue // git reads none there: only where it is is checked
		}
		// git warns of a file it cannot read and goes on as if it said nothing.
		fh, err := os.Open(f) // #nosec G304 -- the person's own configuration, read only
		if err != nil {
			return nil, fmt.Errorf("your git configuration file %s could not be read (%v), so what it holds is not known", f, err)
		}
		_ = fh.Close()
		incs, err := includesOf(ctx, dir, f)
		if err != nil {
			return nil, err
		}
		for _, v := range incs {
			switch {
			case strings.HasPrefix(v, "~") && !strings.HasPrefix(v, "~/"), strings.HasPrefix(v, "%(prefix)/"):
				// git finds these from another user's home or its own install; not followed here.
				return nil, fmt.Errorf("your git configuration file %s includes %q, a path Abhed does not follow (only ~/ and plain paths are)", f, v)
			case strings.HasPrefix(v, "~/") && home == "":
				return nil, fmt.Errorf("your git configuration file %s includes %q, and there is no home folder to find it in", f, v)
			}
			if rest, ok := strings.CutPrefix(v, "~/"); ok {
				v = filepath.Join(home, rest)
			} else if !filepath.IsAbs(v) {
				v = filepath.Join(filepath.Dir(f), v)
			}
			roots = append(roots, v)
		}
	}
	return out, nil
}

// includesOf are the paths file f includes, whatever their conditions.
func includesOf(ctx context.Context, dir, f string) ([]string, error) {
	cmd := gitCmd(ctx, dir, true, append(Env(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"),
		"-C", "/", "config", "--file", f, "--null", "--get-regexp", `^include(if)?\..*path$`)
	cmd.Dir = "/"
	out, err := cmd.Output()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return nil, nil // none
	case err != nil:
		return nil, fmt.Errorf("your git configuration file %s could not be read (%v), so what it includes is not known", f, err)
	}
	var paths []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if _, v, ok := strings.Cut(entry, "\n"); ok && v != "" {
			paths = append(paths, v)
		}
	}
	return paths, nil
}

// within reports whether p, with links followed, is dir or under it; names
// compare without case where the disk usually ignores it.
func within(p, dir string) bool {
	rp, rd := sandbox.RealPath(p), sandbox.RealPath(dir)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		rp, rd = strings.ToLower(rp), strings.ToLower(rd)
	}
	rel, err := filepath.Rel(rd, rp)
	return err == nil && filepath.IsLocal(rel) || err == nil && rel == "."
}
