package hostgit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// writableAreas are the host folders the sandbox's commands can write
// besides the workspace; tests set it.
var writableAreas = sandbox.WritableAreas

// PersonConfigWritable says why the person's own git configuration cannot
// be trusted as theirs for git run on the host from dir: a file of it (the
// system file, the global one, ~/.gitconfig and the XDG file, and every file
// they include), or the folder it is in, lies, links followed, in one of
// areas, which commands can write; or it could not all be read. nil when
// none does. Host git takes its SSH command, askpass and credential helpers
// from these files, so one a command could write would hand it a program.
func PersonConfigWritable(ctx context.Context, dir string, areas []string) error {
	files := ConfigFiles(ctx, dir)
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config"))
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		files = append(files, filepath.Join(x, "git", "config"))
	}
	// What git includes is listed only when every file can be read.
	// A file git cannot read, an include among them, makes it stop listing.
	for _, scope := range []string{"--global", "--system"} {
		cmd := gitCmd(ctx, dir, true, Env(), "-C", dir, "config", scope, "--includes", "--list")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil && strings.Contains(stderr.String(), "unable to access") {
			return fmt.Errorf("your %s git configuration could not be read in full (%s), so what it includes is not known",
				strings.TrimPrefix(scope, "--"), strings.TrimSpace(stderr.String()))
		}
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
	}
	return nil
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
