package hostgit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Identity is the author and committer environment git would take for a
// commit in dir, read on the host, where reading configuration runs nothing:
// user.name and user.email from every file git reads there. The fence and
// container tiers give commands a home of their own, without the person's
// configuration, so their git commits carry this instead of failing for want
// of a name. A configuration file git cannot read (an include.path to a file
// this account may not open) makes git refuse to read any; each file is then
// read on its own, an unreadable include skipped. nil when neither is set.
func Identity(ctx context.Context, dir string) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name, email, err := identityOf(ctx, dir)
	if err != nil {
		name, email = identityByFile(ctx, dir)
	}
	var out []string
	if printable(name) {
		out = append(out, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
	}
	if printable(email) {
		out = append(out, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
	}
	return out
}

// identityPattern names the two keys an identity is.
const identityPattern = `^user\.(name|email)$`

// identityOf reads user.name and user.email as git in dir would; an error
// when git refused to read its configuration.
func identityOf(ctx context.Context, dir string) (name, email string, err error) {
	out, err := gitCmd(ctx, dir, true, Env(), "-C", dir, "config", "--includes", "-z", "--get-regexp", identityPattern).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "", "", nil // neither is set
	}
	if err != nil {
		return "", "", err
	}
	name, email = lastIdentity(out)
	return name, email, nil
}

// identityByFile reads the files git would, in its order (system, global,
// the repository's), each alone and outside any repository, with its
// includes where they can be read and without them where one cannot.
func identityByFile(ctx context.Context, dir string) (name, email string) {
	var files []string
	files = append(files, "/etc/gitconfig")
	if p, err := GitPath(dir); err == nil {
		files = append(files, filepath.Join(filepath.Dir(filepath.Dir(sandbox.RealPath(p))), "etc", "gitconfig"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		files = append(files, filepath.Join(xdg, "git", "config"), filepath.Join(home, ".gitconfig"))
	}
	if root := repoRoot(dir); root != "" {
		// A .git file's folder is not followed: reading it is what failed.
		if info, err := os.Lstat(filepath.Join(root, ".git")); err == nil && info.IsDir() {
			files = append(files, filepath.Join(root, ".git", "config"))
		}
	}
	// No global or system file and no repository: only the file named is read.
	env := append(Env(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if _, err := os.Stat(f); err != nil {
			continue
		}
		var out []byte
		for _, inc := range []string{"--includes", "--no-includes"} {
			cmd := gitCmd(ctx, dir, true, env, "-C", "/", "config", "--file", f, inc, "-z", "--get-regexp", identityPattern)
			cmd.Dir = "/"
			b, err := cmd.Output()
			var ee *exec.ExitError
			if err == nil || errors.As(err, &ee) && ee.ExitCode() == 1 {
				out = b
				break
			}
		}
		n, e := lastIdentity(out)
		if n != "" {
			name = n
		}
		if e != "" {
			email = e
		}
	}
	return name, email
}

// lastIdentity is the last user.name and user.email in git config -z
// --get-regexp output: key, newline, value, NUL.
func lastIdentity(out []byte) (name, email string) {
	for _, entry := range strings.Split(string(out), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		switch strings.ToLower(key) {
		case "user.name":
			name = value
		case "user.email":
			email = value
		}
	}
	return name, email
}

// printable reports a value fit for the environment: set, with no control
// character.
func printable(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
