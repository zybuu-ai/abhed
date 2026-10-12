package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// safeGitKeys are the settings git init, clone and submodule write, naming no
// program; anything else in a new repository is taken out.
var safeGitKeys = map[string]map[string]bool{
	"core": set("repositoryformatversion", "filemode", "bare", "logallrefupdates", "ignorecase",
		"precomposeunicode", "symlinks", "autocrlf", "safecrlf", "eol", "worktree", "sharedrepository",
		"checkstat", "trustctime", "untrackedcache", "quotepath"),
	"remote.*":    set("url", "fetch", "pushurl", "push", "tagopt", "mirror", "prune", "promisor", "partialclonefilter"),
	"branch.*":    set("remote", "merge", "rebase", "pushremote", "description"),
	"submodule.*": set("url", "active", "branch", "shallow"),
	"extensions":  set("objectformat", "worktreeconfig", "partialclone", "preciousobjects", "refstorage", "compatobjectformat"),
	"user":        set("name", "email"),
	"init":        set("defaultbranch"),
	"pull":        set("rebase", "ff"),
	"fetch":       set("prune"),
	"gc":          set("auto"),
	"index":       set("version"),
	"lfs":         set("repositoryformatversion"),
	"clone":       set("defaultremotename"),
}

func set(words ...string) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

func readSmall(p string) ([]byte, error) {
	f, err := os.Open(p) // #nosec G304 -- a git folder's config the walk found
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// filterGitConfig is the configuration b with only the settings git init or
// clone write kept, and the names of those it dropped.
func filterGitConfig(b []byte) ([]byte, []string) {
	var out strings.Builder
	var bad []string
	section := ""
	for _, raw := range strings.SplitAfter(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			out.WriteString(raw)
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				bad = append(bad, line)
				continue
			}
			head := strings.TrimSpace(line[1:end])
			name, sub, quoted := strings.Cut(head, " ")
			if !quoted {
				// [section.sub] is the old spelling of a subsection.
				name, sub, quoted = strings.Cut(head, ".")
			}
			section = strings.ToLower(name)
			if quoted && strings.TrimSpace(sub) != "" {
				section += ".*"
			}
			out.WriteString(line[:end+1] + "\n")
			if rest := strings.TrimSpace(line[end+1:]); rest != "" && rest[0] != '#' && rest[0] != ';' {
				line = rest
			} else {
				continue
			}
		}
		key, _, _ := strings.Cut(line, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		// A value continued onto the next line is dropped with what follows it.
		if !safeGitKeys[section][key] || strings.HasSuffix(line, "\\") {
			bad = append(bad, section+"."+key)
			continue
		}
		out.WriteString("\t" + line + "\n")
	}
	return []byte(out.String()), bad
}

// hookIn names a file in the hooks folder h other than git's samples, or "".
func hookIn(h string) string {
	entries, err := os.ReadDir(h)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sample") {
			return filepath.Join(h, e.Name())
		}
	}
	return ""
}

// gitFolderOf is the git folder a .git entry is or names, or "".
func gitFolderOf(entry string) string {
	info, err := os.Lstat(entry)
	switch {
	case err != nil:
		return ""
	case info.IsDir():
		return entry
	case info.Mode().IsRegular():
		to, ok := strings.CutPrefix(readPointer(entry), "gitdir: ")
		if !ok || to == "" {
			return ""
		}
		if !filepath.IsAbs(to) {
			to = filepath.Join(filepath.Dir(entry), to)
		}
		return filepath.Clean(to)
	}
	return ""
}

// gitFolderIn is the real path of the git folder g is or names when it is in
// ws and not under any-case .abhed; inside reports whether it is, and dir is
// "" when there is none yet.
func gitFolderIn(ws, g string) (dir string, inside bool) {
	named := gitFolderOf(g)
	if named == "" {
		return "", true
	}
	real, err := filepath.EvalSymlinks(named)
	if err != nil {
		// Nothing there yet, so nothing to clean; looked at again next time.
		return "", true
	}
	root, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return real, false
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return real, false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.EqualFold(part, stateDir) {
			return real, false
		}
	}
	return real, true
}

// plantedGits strips what names a program git would run from git folders new
// since the session's first look, and says why the command is refused. A
// repository counts as seen only once it is clean, so a failed rewrite refuses
// every command until it is fixed.
func plantedGits(ctx context.Context, ws, backend string, mem *gitMemory, gits []string) error {
	mem.mu.Lock()
	if mem.based == nil {
		mem.based, mem.seenGit = map[string]bool{}, map[string]bool{}
	}
	first := !mem.based[ws]
	mem.based[ws] = true
	var fresh []string
	for _, g := range gits {
		if !mem.seenGit[g] {
			if first {
				mem.seenGit[g] = true
			} else {
				fresh = append(fresh, g)
			}
		}
	}
	mem.mu.Unlock()
	var entries []map[string]any
	var parts []string
	var clean []string
	for _, g := range fresh {
		dir, inside := gitFolderIn(ws, g)
		if !inside {
			// The host never writes where a pointer the command wrote sends it.
			entries = append(entries, map[string]any{"path": g, "repository": g, "points_to": dir, "outcome": plantRemaining,
				"settings": []string{"gitdir outside the workspace"}})
			parts = append(parts, g+" names the git folder "+dir+", which is outside the workspace or under "+stateDir+
				"; nothing there was changed; remove "+g+" or point it inside the workspace")
			continue
		}
		if dir == "" {
			continue
		}
		ok := true
		for _, c := range []string{"config", "config.worktree"} {
			if m := cleanGitConfig(filepath.Join(dir, c), g); m != nil {
				ok = ok && m["outcome"] != plantRemaining
				entries = append(entries, m)
				parts = append(parts, m["summary"].(string))
				delete(m, "summary")
			}
		}
		h := filepath.Join(dir, "hooks")
		if hook := hookIn(h); hook != "" {
			m := map[string]any{"path": h, "repository": g, "settings": []string{"hook " + filepath.Base(hook)}}
			if dest, err := quarantine(h, "hooks", "git-"); err == nil {
				m["outcome"], m["moved_to"] = plantMoved, dest
				parts = append(parts, h+" was moved to "+dest)
				makeEmpty(h, true)
			} else if rerr := os.RemoveAll(h); rerr == nil {
				m["outcome"] = plantRemoved
				parts = append(parts, h+" was removed")
			} else {
				ok = false
				m["outcome"] = plantRemaining
				parts = append(parts, h+" is still there and could not be taken out; remove it before running git there")
			}
			entries = append(entries, m)
		}
		if ok {
			clean = append(clean, g)
		}
	}
	mem.mu.Lock()
	for _, g := range clean {
		mem.seenGit[g] = true
	}
	mem.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	if launch := LaunchOf(ctx); launch.Record != nil {
		_ = launch.Record(EvGitPlanted, map[string]any{"entries": entries, "call_id": launch.CallID, "sandbox": backend, "workspace": ws,
			"reason": "a repository made during the session named a program git would run"})
	}
	return fmt.Errorf("sandbox: the command was not run: a repository made by an earlier command names a program git "+
		"would run (a setting git init or clone never writes, or a hook), which your next git command there, or in the "+
		"repository holding it, could run; %s", strings.Join(parts, "; "))
}

// cleanGitConfig keeps p's config to what git init or clone write, quarantining
// a copy first; nil when it held nothing else.
func cleanGitConfig(p, g string) map[string]any {
	info, err := os.Lstat(p)
	if err != nil {
		return nil
	}
	if !info.Mode().IsRegular() {
		// Rewriting it would write wherever it points.
		return map[string]any{"path": p, "repository": g, "settings": []string{"not a regular file"}, "outcome": plantRemaining,
			"summary": p + " is not a regular file, so git could read settings from wherever it points; replace it with a plain file"}
	}
	b, err := readSmall(p)
	if err != nil {
		return nil
	}
	kept, bad := filterGitConfig(b)
	if len(bad) == 0 {
		return nil
	}
	m := map[string]any{"path": p, "repository": g, "settings": bad}
	dest, qerr := keepCopy(p, b)
	if qerr == nil {
		m["moved_to"] = dest
	}
	// Written at the same path, as git's own config, so the repository keeps its remote and branches.
	if err := replaceFile(p, kept); err != nil {
		m["outcome"], m["error"] = plantRemaining, err.Error()
		m["summary"] = p + " names " + strings.Join(bad, ", ") + " and could not be rewritten; remove those settings before running git there"
		return m
	}
	m["outcome"] = "rewritten"
	m["summary"] = p + " named " + strings.Join(bad, ", ") + ", which were taken out"
	if qerr == nil {
		m["summary"] = m["summary"].(string) + " (the whole file is in " + dest + ")"
	}
	return m
}

// keepCopy writes b, the file at p as it was, to a folder of its own in the quarantine.
func keepCopy(p string, b []byte) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, stateDir, QuarantineDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "git-")
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, filepath.Base(p))
	return dest, os.WriteFile(dest, b, 0o600)
}

// replaceFile puts b at p by renaming a new file over it, so a hard link to p
// is cut rather than written through.
func replaceFile(p string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".config-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o644)); err != nil { // #nosec G302 -- git's own mode for config
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
