package hostgit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// plan is what a brokered command will do.
type plan struct {
	// sandboxed is a line left to the sandbox: a repository Abhed has not
	// read the remotes of, or a remote it does not have.
	sandboxed bool
	root      string
	// settings are the repository's remote settings now.
	settings string
	// names are the remotes reached, in order; remotes their URLs as git
	// reaches them (rewritten by the person's insteadOf), raw as configured,
	// and specs their fetch refspecs.
	names        []string
	remotes, raw map[string][]string
	specs        map[string][]string
	hosts        []string
	proxy        string
	ask, step    string
	bind         string
	// hooks is the person's own hook folder whose pre-push a push runs in
	// the sandbox, and its scope; relay is what runs it there.
	hooks, hooksScope string
	relay             *prePushRelay
	// early is a host step run as the command is built, before it starts:
	// a pull's fetch, a clone.
	early bool
}

// plan reads what op in cwd would reach, and why it is refused or asked.
func (b *HostBroker) plan(ctx context.Context, cwd string, op remoteOp) (plan, error) {
	if !b.inWorkspace(cwd) {
		return plan{}, fmt.Errorf("git %s: the command runs outside the workspace, so Abhed does not run it on the host", op.verb)
	}
	if op.verb == "clone" {
		return b.planClone(ctx, cwd, op)
	}
	root := b.repoIn(cwd)
	b.mu.Lock()
	start, known := b.known[root]
	b.mu.Unlock()
	if root == "" || !known {
		// Not a repository Abhed read at the start, or one it cloned.
		return plan{sandboxed: true}, nil
	}
	if err := b.personConfigHeld(ctx, cwd, op.verb); err != nil {
		return plan{}, err
	}
	// Where the sandbox binds git's files as each command starts, one started
	// before the clone could still write the clone's configuration.
	if start.cloned && (b.holdsNew == nil || !b.holdsNew()) {
		return plan{}, fmt.Errorf("git %s: this repository was cloned during this session, and on this sandbox tier a command "+
			"started before the clone can still write its configuration, so Abhed does not run git %s on the host for it in this session. "+
			"A later session brokers it when it is that session's workspace (open it there); otherwise run git %s yourself", op.verb, op.verb, op.verb)
	}
	// The git folder git would use must be the one known: a .git swapped for
	// a link to another repository is not reached.
	if common := b.commonOf(ctx, root); common != start.common {
		return plan{}, fmt.Errorf("git %s: the repository's git folder is not the one it was when the session started "+
			"(or lies outside the workspace), so Abhed does not run it on the host", op.verb)
	}
	settings, err := remoteSettings(ctx, root)
	if err != nil {
		return plan{}, fmt.Errorf("git %s: the repository's settings could not be read (%v), so Abhed does not run it on the host", op.verb, err)
	}
	if why := plantedProgram(settings); why != "" {
		return plan{}, fmt.Errorf("git %s: the repository's own configuration %s, which would steer git run on the host as you; "+
			"Abhed does not run git %s on the host while it does", op.verb, why, op.verb)
	}
	p := plan{root: root, settings: settings, remotes: map[string][]string{}, raw: map[string][]string{}, specs: map[string][]string{}}
	names, err := b.remotesOf(ctx, cwd, op)
	if err != nil {
		return plan{}, err
	}
	if len(names) == 0 {
		return plan{sandboxed: true}, nil
	}
	push := op.verb == "push"
	p.proxy = personProxy(ctx, cwd)
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\x00%t\x00", root, push)
	for _, n := range names {
		urls, err := remoteURLs(ctx, cwd, n, push)
		if err != nil || len(urls) == 0 {
			return plan{}, fmt.Errorf("git %s: the remote %s has no URL host git could read", op.verb, n)
		}
		raw := configValues(ctx, cwd, "remote."+n+".url")
		if len(raw) == 0 {
			return plan{}, fmt.Errorf("git %s: the remote %s has no URL host git could read", op.verb, n)
		}
		p.names = append(p.names, n)
		p.remotes[n], p.raw[n] = urls, raw
		p.specs[n] = configValues(ctx, cwd, "remote."+n+".fetch")
		fmt.Fprintf(digest, "%s\x00%q\x00%q\x00", n, raw, urls)
		// What a push sends is the remote's too: a mirror or a push refspec
		// set since a grant was kept (one that deletes or forces, say) is
		// another remote's, and the grant no longer answers for it. Added
		// only when set, so a grant kept without them still matches.
		mirror, pushSpecs := configValues(ctx, cwd, "remote."+n+".mirror"), configValues(ctx, cwd, "remote."+n+".push")
		if len(mirror) > 0 || len(pushSpecs) > 0 {
			fmt.Fprintf(digest, "mirror\x00%q\x00push\x00%q\x00", mirror, pushSpecs)
		}
		for _, u := range urls {
			host, port, err := remoteHost(u)
			if err != nil {
				return plan{}, fmt.Errorf("git %s: the remote %s: %v", op.verb, n, err)
			}
			if err := b.allowed(op.verb, host, port, "the remote "+n+"'s host"); err != nil {
				return plan{}, err
			}
			if !slices.Contains(p.hosts, host) {
				p.hosts = append(p.hosts, host)
			}
			if p.proxy != "" && strings.HasPrefix(strings.ToLower(u), "https://") {
				if err := b.allowedProxy(op.verb, p.proxy); err != nil {
					return plan{}, err
				}
			}
		}
	}
	sort.Strings(p.hosts)
	// A push the repository's own configuration makes a mirror, a force or a
	// delete is one that can lose commits, whatever its words say.
	implied := false
	if push {
		p.hooks, p.hooksScope = personHooks(ctx, cwd, root)
		for _, n := range p.names {
			if len(pushImplied(settings, n)) > 0 {
				implied = true
			}
		}
	}
	p.bind = root + "\x00" + hex.EncodeToString(digest.Sum(nil))[:24]
	where := strings.Join(p.hosts, ", ")
	if p.proxy != "" {
		where += " (through your proxy " + anonymize(p.proxy) + ")"
	}
	who := "runs through host git as you, with your SSH agent, keychain and credential helpers,"
	switch {
	case push && implied:
		p.ask, p.step = fmt.Sprintf("git %s %s and, as the repository's own configuration makes it (a mirror, a forcing or a deleting refspec), "+
			"can replace or delete commits on %s; this exact form is asked each time", op.verb, who, where), StepHostGitEach
	case settings != start.settings:
		p.ask, p.step = fmt.Sprintf("git %s %s and the repository's remote settings changed during this session, "+
			"so host git reaches %s only with your approval, asked each time", op.verb, who, where), StepHostGitEach
	case push && op.force:
		p.ask, p.step = fmt.Sprintf("git %s %s and can replace or delete commits on %s; this exact form is asked each time", op.verb, who, where), StepHostGitEach
	case b.net.Off():
		p.ask, p.step = fmt.Sprintf("git %s %s and reaches %s although the sandbox's network is off; "+
			"only this remote of this repository is reached, and only with your approval", op.verb, who, where), StepHostGit
	}
	return p, nil
}

// personConfigHeld says why the person's own git configuration, which
// host git takes its SSH command and credential helpers from, is not theirs
// alone: a file of it, or one it includes, lies where commands can write.
func (b *HostBroker) personConfigHeld(ctx context.Context, cwd, verb string) error {
	areas := append([]string{b.workspace}, writableAreas()...)
	err := PersonConfigWritable(ctx, cwd, areas)
	if err == nil {
		err = personProgramsHeld(ctx, cwd, areas, hostPATH(areas))
	}
	if err != nil {
		return fmt.Errorf("git %s: %v, so Abhed does not run git %s on the host as you; move it out of the agent's reach, or run it yourself", verb, err, verb)
	}
	return nil
}

// hostPATH is the PATH host git runs with for b: the person's own, less
// the folders in the workspace, those commands can write, and relative ones.
func (b *HostBroker) hostPATH() string {
	return hostPATH(append([]string{b.workspace}, writableAreas()...))
}

// allowed says why the egress allowlist refuses host and port, nil when it does not.
func (b *HostBroker) allowed(verb, host string, port uint16, what string) error {
	if b.net.Allows == nil {
		return nil
	}
	if ok, why := b.net.Allows(host, port); !ok {
		return fmt.Errorf("git %s: the egress allowlist does not allow %s:%d (%s), %s, "+
			"so host git does not reach it either; add it to the allowlist", verb, host, port, why, what)
	}
	return nil
}

// allowedProxy is allowed for the person's proxy, which host git connects to.
func (b *HostBroker) allowedProxy(verb, proxy string) error {
	u := proxy
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	host, port, err := proxyHost(u)
	if err != nil {
		return fmt.Errorf("git %s: your proxy %s could not be read: %v", verb, anonymize(proxy), err)
	}
	return b.allowed(verb, host, port, "your proxy")
}

// planClone is a clone's plan: the URL is the person's to approve, every
// time, into a new folder of the workspace that is no git folder.
func (b *HostBroker) planClone(ctx context.Context, cwd string, op remoteOp) (plan, error) {
	host, port, err := remoteHost(op.url)
	if err != nil {
		return plan{}, fmt.Errorf("git clone: %v", err)
	}
	// host:path names a local folder when one is there, and git takes it so.
	if !strings.Contains(op.url, "://") {
		if _, err := os.Lstat(filepath.Join(cwd, op.url)); err == nil {
			return plan{}, fmt.Errorf("git clone: %s is a local path, not a remote; only ssh and https remotes are reached through host git", op.url)
		}
	}
	if _, err := b.cloneTarget(ctx, cwd, op.dir); err != nil {
		return plan{}, err
	}
	if err := b.personConfigHeld(ctx, cwd, "clone"); err != nil {
		return plan{}, err
	}
	if err := b.allowed("clone", host, port, "the URL's host"); err != nil {
		return plan{}, err
	}
	p := plan{hosts: []string{host}, remotes: map[string][]string{"origin": {op.url}}, step: StepHostGitEach}
	p.proxy = personProxy(ctx, cwd)
	if p.proxy != "" && strings.HasPrefix(strings.ToLower(op.url), "https://") {
		if err := b.allowedProxy("clone", p.proxy); err != nil {
			return plan{}, err
		}
	}
	p.ask = fmt.Sprintf("git clone runs through host git as you, with your SSH agent, keychain and credential helpers, "+
		"and copies %s into %s; a clone is asked each time", anonymize(op.url), op.dir)
	if b.net.Off() {
		p.ask += ", and reaches " + host + " although the sandbox's network is off"
	}
	return p, nil
}

// cloneTarget is where a clone into dir, named from cwd, goes, relative to
// the workspace's real folder: a new folder, with no link and no git folder
// on the way. Checked again, folder by folder, when the clone is moved there.
func (b *HostBroker) cloneTarget(ctx context.Context, cwd, dir string) ([]string, error) {
	abs := dir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, dir)
	}
	abs = filepath.Clean(abs)
	ws := sandbox.RealPath(b.workspace)
	var rel string
	for _, w := range append([]string{ws}, sandbox.PathForms(b.workspace)...) {
		if r, err := filepath.Rel(w, abs); err == nil && r != "." && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel = r
			break
		}
	}
	if rel == "" {
		return nil, fmt.Errorf("git clone: %s is outside the workspace, so Abhed does not run it on the host", dir)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	at := ws
	for i, part := range parts {
		if strings.EqualFold(part, ".git") {
			return nil, fmt.Errorf("git clone: %s is inside a git folder, which a clone must not write; Abhed does not run it on the host", dir)
		}
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		switch {
		case err != nil && i < len(parts)-1:
			return nil, fmt.Errorf("git clone: %s's folder %s is not there; make it first", dir, filepath.Dir(at))
		case err != nil:
		case info.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("git clone: %s is reached through a link (%s), so Abhed does not run it on the host", dir, at)
		case i == len(parts)-1:
			return nil, fmt.Errorf("git clone: %s is already there; clone into a new folder", dir)
		case !info.IsDir():
			return nil, fmt.Errorf("git clone: %s is not a folder", at)
		case b.isGitFolder(ctx, at):
			return nil, fmt.Errorf("git clone: %s is inside a git folder, which a clone must not write; Abhed does not run it on the host", dir)
		}
	}
	return parts, nil
}

// isGitFolder reports whether dir is a known repository's git folder.
func (b *HostBroker) isGitFolder(_ context.Context, dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range b.known {
		if c, err := os.Stat(k.common); err == nil && os.SameFile(c, info) {
			return true
		}
	}
	return false
}

// remotesOf are the remotes op reaches: the one named, every one for fetch
// --all, or the current branch's (origin when it has none). None when the
// remote named is not one the repository has, which leaves the line to the
// sandbox, as a URL does.
func (b *HostBroker) remotesOf(ctx context.Context, cwd string, op remoteOp) ([]string, error) {
	have, err := configured(ctx, cwd)
	if err != nil {
		return nil, fmt.Errorf("git %s: the repository's remotes could not be read: %v", op.verb, err)
	}
	switch {
	case op.all:
		return have, nil
	case op.remote != "":
		if slices.Contains(have, op.remote) {
			return []string{op.remote}, nil
		}
		return nil, nil
	}
	name := "origin"
	if branch := currentBranch(ctx, cwd); branch != "" {
		key := "branch." + branch + ".remote"
		if op.verb == "push" {
			if r, err := gitOut(ctx, cwd, "config", "--get", "branch."+branch+".pushRemote"); err == nil && r != "" {
				name = r
			} else if r, err := gitOut(ctx, cwd, "config", "--get", "remote.pushDefault"); err == nil && r != "" {
				name = r
			} else if r, err := gitOut(ctx, cwd, "config", "--get", key); err == nil && r != "" {
				name = r
			}
		} else if r, err := gitOut(ctx, cwd, "config", "--get", key); err == nil && r != "" {
			name = r
		}
	}
	if !slices.Contains(have, name) {
		return nil, nil
	}
	return []string{name}, nil
}

// currentBranch is the branch checked out in cwd, "" when detached.
func currentBranch(ctx context.Context, cwd string) string {
	b, err := gitOut(ctx, cwd, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return b
}

// configured are the repository's remotes.
func configured(ctx context.Context, cwd string) ([]string, error) {
	out, err := gitOut(ctx, cwd, "remote")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// configValues are key's values as git reads them in cwd, in order.
func configValues(ctx context.Context, cwd, key string) []string {
	out, err := gitCmd(ctx, cwd, true, readEnv(), "-C", cwd, "config", "--includes", "-z", "--get-all", key).Output()
	if err != nil {
		return nil
	}
	var vals []string
	for _, v := range bytes.Split(out, []byte{0}) {
		if len(v) > 0 {
			vals = append(vals, string(v))
		}
	}
	return vals
}

// remoteURLs are the URLs git reaches for remote, with insteadOf applied.
func remoteURLs(ctx context.Context, cwd, remote string, push bool) ([]string, error) {
	args := []string{"remote", "get-url", "--all"}
	if push {
		args = append(args, "--push")
	}
	out, err := gitOut(ctx, cwd, append(args, remote)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// gitOut is git's output in cwd, read on the host: reading configuration
// and refs runs nothing.
func gitOut(ctx context.Context, cwd string, args ...string) (string, error) {
	out, err := gitCmd(ctx, cwd, true, readEnv(), append([]string{"-C", cwd}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// settingKeys are the configuration a fetch, pull or push reads to choose
// where it goes and how it authenticates, or that names a program.
var settingKeys = []string{"remote.", "url.", "http.", "credential.", "ssh.", "protocol.", "include.", "includeif.",
	"hook.", "core.sshcommand", "core.gitproxy", "core.askpass", "core.worktree", "core.alternaterefscommand", "uploadpack.",
	"core.pager", "pager."}

// remoteSettings are the repository's settings among settingKeys, from
// every file git reads (includes followed), in order, as one string.
func remoteSettings(ctx context.Context, root string) (string, error) {
	out, err := gitCmd(ctx, root, true, readEnv(), "-C", root, "config", "--includes", "-z", "--show-scope", "--list").Output()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	parts := bytes.Split(out, []byte{0})
	for i := 0; i+1 < len(parts); i += 2 {
		scope := string(parts[i])
		if scope == "command" {
			continue
		}
		kv := string(parts[i+1])
		key, _, _ := strings.Cut(kv, "\n")
		lower := strings.ToLower(key)
		for _, k := range settingKeys {
			if strings.HasPrefix(lower, k) {
				fmt.Fprintf(&b, "%s\x00%s\x00", scope, kv)
				break
			}
		}
	}
	return b.String(), nil
}

// plantedProgram says what of settings, in the repository's own files,
// would steer host git whatever the command line sets: a hook, a remote
// helper, another work tree, an upload or receive program, a proxy, an http
// setting (proxy, TLS, extra headers, cookies, address overrides) or a URL
// rewrite. "" when nothing does. The person's own files are theirs.
func plantedProgram(settings string) string {
	parts := strings.Split(settings, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		scope := parts[i]
		if scope != "local" && scope != "worktree" {
			continue
		}
		key, _, _ := strings.Cut(parts[i+1], "\n")
		lower := strings.ToLower(key)
		remoteKey := func(suffix string) bool {
			return strings.HasPrefix(lower, "remote.") && strings.HasSuffix(lower, suffix)
		}
		switch {
		case strings.HasPrefix(lower, "hook."):
			return "names a hook (" + key + ")"
		case remoteKey(".vcs"):
			return "names a remote helper (" + key + ")"
		case lower == "core.worktree":
			return "moves the work tree elsewhere (" + key + ")"
		case remoteKey(".uploadpack"), remoteKey(".receivepack"):
			return "names the program run on the remote (" + key + ")"
		case remoteKey(".proxy"), remoteKey(".proxyauthmethod"):
			return "sets a proxy (" + key + ")"
		case strings.HasPrefix(lower, "http."):
			return "sets how https is reached (" + key + ")"
		case strings.HasPrefix(lower, "url."):
			return "rewrites remote URLs (" + key + ")"
		case lower == "core.sshcommand", lower == "core.askpass", lower == "core.gitproxy",
			strings.HasPrefix(lower, "credential.") && strings.HasSuffix(lower, ".helper"):
			// Overridden on the command line too; refused so a lost pin could not run it.
			return "names a program for git to run with your credentials (" + key + ")"
		}
	}
	return ""
}

// repoIn is the real top folder of the repository holding dir, when it is
// inside the workspace; "" otherwise.
func (b *HostBroker) repoIn(dir string) string {
	root := repoRoot(dir)
	if root == "" || !b.inWorkspace(root) {
		return ""
	}
	return sandbox.RealPath(root)
}

// inWorkspace reports whether p is the workspace or inside it, by its
// spelling and where its links lead.
func (b *HostBroker) inWorkspace(p string) bool {
	p = filepath.Clean(p)
	for _, w := range sandbox.PathForms(b.workspace) {
		if _, ok := sandbox.Within(p, w); ok {
			if _, ok := sandbox.Within(sandbox.RealPath(p), sandbox.RealPath(w)); ok {
				return true
			}
		}
	}
	return false
}
