package hostgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// remoteConfig is the configuration host git runs a remote command with,
// after safety: the person's own SSH command, askpass, proxy, TLS check and
// credential helpers, read from their global and system files; for each
// remote reached, no proxy of its own and git's own upload and receive
// programs; ssh and https only; no pager, editor, signature check,
// submodule or automatic gc. These pins do not cover everything: git's URL
// matching lets an http.<url>.* setting (its proxy, its TLS check) outrank
// the command line's http.proxy and http.sslVerify whatever its scope, so
// a repository that sets any http.* key is refused (plantedProgram), and
// that refusal, not these pins, is what keeps its own out.
func remoteConfig(ctx context.Context, cwd string, remotes []string) [][2]string {
	global := func(key string) []string {
		var vals []string
		for _, v := range personValues(ctx, cwd, key) {
			vals = append(vals, v[1])
		}
		return vals
	}
	last := func(key, fallback string) string {
		if v := global(key); len(v) > 0 {
			return v[len(v)-1]
		}
		return fallback
	}
	cfg := [][2]string{
		// The person's SSH, which checks host keys against known_hosts and never
		// writes it; BatchMode, as no one is there to type a passphrase.
		{"core.sshCommand", last("core.sshcommand", "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o UpdateHostKeys=no")},
		{"ssh.variant", last("ssh.variant", "auto")},
		{"core.askPass", last("core.askpass", "")},
		{"core.gitProxy", last("core.gitproxy", "")},
		{"http.proxy", last("http.proxy", "")},
		{"http.sslVerify", last("http.sslverify", "true")},
		{"core.alternateRefsCommand", ""},
		// A backstop: --no-pager and GIT_PAGER already keep the repository's pager off.
		{"core.pager", "cat"},
		{"protocol.ssh.allow", "always"},
		{"merge.verifySignatures", "false"},
		{"fetch.recurseSubmodules", "false"},
		{"push.recurseSubmodules", "no"},
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
		{"core.editor", ":"},
		{"sequence.editor", ":"},
	}
	for _, n := range remotes {
		cfg = append(cfg, [2]string{"remote." + n + ".proxy", ""},
			[2]string{"remote." + n + ".uploadpack", "git-upload-pack"},
			[2]string{"remote." + n + ".receivepack", "git-receive-pack"})
	}
	// Each credential helper key any file names is emptied, all of them
	// first, as an empty value clears the one list every key adds to; then
	// the person's global and system helpers are given again, in order.
	keys := []string{"credential.helper"}
	if out, err := gitCmd(ctx, cwd, true, readEnv(), "-C", cwd, "config", "--includes", "-z", "--name-only",
		"--get-regexp", `^credential\..*\.helper$`).Output(); err == nil {
		for _, k := range bytes.Split(out, []byte{0}) {
			if k := string(k); k != "" && !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	for _, k := range keys {
		cfg = append(cfg, [2]string{k, ""})
	}
	for _, k := range keys {
		for _, v := range global(k) {
			cfg = append(cfg, [2]string{k, v})
		}
	}
	return cfg
}

// personValues are key's values in the person's own configuration, each
// with its scope: system, global, and the files git ships with ("unknown",
// such as the keychain helper), never the repository's.
func personValues(ctx context.Context, cwd, key string) [][2]string {
	out, err := gitCmd(ctx, cwd, true, readEnv(), "-C", cwd, "config", "--includes", "-z", "--show-scope", "--get-all", key).Output()
	if err != nil {
		return nil
	}
	var vals [][2]string
	parts := bytes.Split(out, []byte{0})
	for i := 0; i+1 < len(parts); i += 2 {
		switch scope := string(parts[i]); scope {
		case "system", "global", "unknown":
			vals = append(vals, [2]string{scope, string(parts[i+1])})
		}
	}
	return vals
}

// personProxy is the proxy host git takes for https: the person's
// http.proxy, else their environment's; "" for none.
func personProxy(ctx context.Context, cwd string) string {
	if v := personValues(ctx, cwd, "http.proxy"); len(v) > 0 {
		return v[len(v)-1][1]
	}
	for _, k := range []string{"https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// proxyHost is a proxy URL's host and port.
func proxyHost(raw string) (string, uint16, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", 0, errors.New("no host")
	}
	port := uint16(1080)
	if strings.HasPrefix(u.Scheme, "http") {
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return "", 0, errors.New("no port")
		}
		port = uint16(n)
	}
	return strings.ToLower(u.Hostname()), port, nil
}

// hooksNotRun are the hook folders the person's own configuration names
// (core.hooksPath, such as a machine's secret-scanning pre-push hook), which
// host git does not run for a brokered command: hooks stay off on the host,
// as for every git Abhed runs there. Recorded, so a reviewer sees it.
func hooksNotRun(ctx context.Context, cwd string) []map[string]string {
	var out []map[string]string
	for _, v := range personValues(ctx, cwd, "core.hooksPath") {
		if v[1] != "" && v[1] != "/dev/null" {
			out = append(out, map[string]string{"scope": v[0], "path": v[1]})
		}
	}
	return out
}

// personHooks is the hook folder the person's own configuration names
// (core.hooksPath in their system or global file, such as a machine's
// secret-scanning pre-push hook), relative to the repository's top when it
// is relative, as git takes it, and its scope; "" for none. Its pre-push
// hook runs in the sandbox for a brokered push (prePushRelay), never on the
// host, as a hook may run whatever the work tree holds.
func personHooks(ctx context.Context, cwd, root string) (path, scope string) {
	vals := personValues(ctx, cwd, "core.hooksPath")
	if len(vals) == 0 {
		return "", ""
	}
	scope, path = vals[len(vals)-1][0], vals[len(vals)-1][1]
	if path == "" || path == "/dev/null" {
		return "", ""
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, rest)
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	// Only a folder with a pre-push hook to run; git runs none else for a push.
	if info, err := os.Stat(filepath.Join(path, "pre-push")); err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", ""
	}
	return path, scope
}

// remoteEnv is the environment a remote command adds: ssh and https, no
// prompt, no pager and no editor.
var remoteEnv = []string{"GIT_ALLOW_PROTOCOL=ssh:https", "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:",
	"GIT_MERGE_AUTOEDIT=no", "GIT_PAGER=cat", "PAGER=cat"}

// userInfo is a URL's user and password, which output never shows.
var userInfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)

// redact is s without the user and password of any URL in it.
func redact(s string) string { return userInfo.ReplaceAllString(s, "$1") }

// redactSed is redact for sed -E, unbuffered, on the host's sed.
func redactSed() string {
	flag := "-u"
	if runtime.GOOS == "darwin" || strings.HasSuffix(runtime.GOOS, "bsd") {
		flag = "-l"
	}
	return "sed " + flag + ` -E 's#([A-Za-z][A-Za-z0-9+.-]*://)[^/@[:space:]]+@#\1#g'`
}

// seenEnv, when set, is given each host command's environment; tests set it.
var seenEnv func(env []string)

// hostScript is a host process running git (each of runs in dir, its exit
// the first failure's) with env, its output merged and redacted.
func hostScript(ctx context.Context, dir string, env []string, runs [][]string) *exec.Cmd {
	if err := checkEnv(env); err != nil {
		return &exec.Cmd{Err: err}
	}
	if seenEnv != nil {
		seenEnv(env)
	}
	path, err := GitPath(dir)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	var b strings.Builder
	b.WriteString("set -o pipefail; { s=0; ")
	for _, args := range runs {
		b.WriteString(`"$0" -C ` + shellQuote(dir))
		for _, a := range args {
			b.WriteString(" " + shellQuote(a))
		}
		b.WriteString(` || { c=$?; [ "$s" = 0 ] && s=$c; }; `)
	}
	b.WriteString(`exit "$s"; } 2>&1 | ` + redactSed())
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", b.String(), path) // #nosec G204 -- fixed shell; every word quoted
	cmd.Dir = dir
	cmd.Env = env
	// A pipe a leftover holds does not keep Wait waiting. Its own session
	// is given by whoever runs it: the bash tool for a call, runNow here.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// repoEnv is the environment host git runs a remote command with in cwd's
// repository: git's own dropped, the common git folder pinned, the safety
// settings and cfg in the command-line scope, and remoteEnv.
func repoEnv(ctx context.Context, cwd string, cfg [][2]string) ([]string, error) {
	r := New(ctx, cwd)
	if r.unknown != nil {
		return nil, r.unknown
	}
	if err := r.redirected(); err != nil {
		return nil, err
	}
	return hostEnv(r.env(nil), append(append(append([][2]string(nil), safety...), r.drivers...), cfg...), remoteEnv...)
}

// hostEnv is the one way host git's environment for a remote command is
// built: base and then overrides, each variable once (the later value
// kept), then cfg as the only block of command-line configuration. git reads
// one GIT_CONFIG_COUNT, and the environment keeps the last of a name, so a
// second block would replace every pin before it: base or overrides naming
// any GIT_CONFIG_ variable is refused.
func hostEnv(base []string, cfg [][2]string, overrides ...string) ([]string, error) {
	var out []string
	at := map[string]int{}
	for _, kv := range append(append([]string(nil), base...), overrides...) {
		name, _, _ := strings.Cut(kv, "=")
		if configBlock(name) {
			return nil, fmt.Errorf("host git's environment was given %s outside its one configuration block", name)
		}
		if i, ok := at[name]; ok {
			out[i] = kv
			continue
		}
		at[name] = len(out)
		out = append(out, kv)
	}
	return append(out, configEnv(cfg)...), nil
}

// readEnv is host git's environment for reading configuration and refs:
// git's own dropped and the safety settings as its one block.
func readEnv() []string {
	env, err := hostEnv(Env(), safety)
	if err != nil {
		return append(Env(), configEnv(safety)...) // Env holds no block: not reached
	}
	return env
}

// configBlock reports a variable of git's command-line configuration block.
func configBlock(name string) bool {
	return name == "GIT_CONFIG_COUNT" || name == "GIT_CONFIG_PARAMETERS" ||
		strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// checkEnv says why env is not one host git may run with: a variable named
// twice, or more than one block of command-line configuration.
func checkEnv(env []string) error {
	seen := map[string]bool{}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if seen[name] {
			return fmt.Errorf("host git's environment names %s twice, so Abhed does not run it", name)
		}
		seen[name] = true
	}
	return nil
}

// fetchRuns are the git runs a fetch of p's remotes is, each to the URL it
// was judged on with its fetch refspecs on the command line, so nothing the
// repository's configuration says later changes where it goes.
func fetchRuns(op remoteOp, p plan, opts []string) [][]string {
	var runs [][]string
	for _, n := range p.names {
		args := append(append([]string{"--no-pager", "fetch"}, opts...), p.raw[n][0])
		if op.verb == "fetch" && len(op.positional) > 1 {
			for _, s := range op.positional[1:] {
				args = append(args, s)
				if dst, force := mapRef(p.specs[n], fullRef(s)); dst != "" && !strings.Contains(s, ":") {
					args = append(args, force+fullRef(s)+":"+dst)
				}
			}
		} else {
			args = append(args, p.specs[n]...)
		}
		runs = append(runs, args)
	}
	return runs
}

// fullRef is a branch name as a ref.
func fullRef(s string) string {
	s = strings.TrimPrefix(s, "+")
	if strings.HasPrefix(s, "refs/") || strings.Contains(s, ":") {
		return s
	}
	return "refs/heads/" + s
}

// mapRef is where specs (a remote's fetch refspecs) put src, and "+" when
// that refspec forces; "" when none takes it.
func mapRef(specs []string, src string) (string, string) {
	for _, s := range specs {
		force := ""
		if strings.HasPrefix(s, "+") {
			force, s = "+", s[1:]
		}
		from, to, ok := strings.Cut(s, ":")
		if !ok {
			continue
		}
		if fp, fs, glob := strings.Cut(from, "*"); glob {
			tp, ts, tglob := strings.Cut(to, "*")
			if tglob && strings.HasPrefix(src, fp) && strings.HasSuffix(src, fs) && len(src) >= len(fp)+len(fs) {
				return tp + src[len(fp):len(src)-len(fs)] + ts, force
			}
		} else if from == src {
			return to, force
		}
	}
	return "", ""
}

// hostCommand is the process of a fetch, ls-remote or push on the host,
// and p with what it set up for it.
func (b *HostBroker) hostCommand(ctx context.Context, cwd string, op remoteOp, p plan) (*exec.Cmd, plan) {
	cfg := remoteConfig(ctx, cwd, p.names)
	if op.verb == "push" && p.hooks != "" {
		// Host git calls Abhed's own pre-push, which runs the person's in the
		// sandbox; last in the one configuration block, over safety's none.
		relay, err := b.prePushRelay(ctx, p)
		if err != nil {
			return &exec.Cmd{Err: fmt.Errorf("git push: your pre-push hook could not be set up to run in the sandbox (%v), so Abhed does not push without it", err)}, p
		}
		cfg = append(cfg, [2]string{"core.hooksPath", relay.dir})
		p.relay = relay
	}
	env, err := repoEnv(ctx, cwd, cfg)
	if err != nil {
		return &exec.Cmd{Err: err}, p
	}
	var runs [][]string
	switch op.verb {
	case "fetch":
		runs = fetchRuns(op, p, op.opts)
	case "ls-remote":
		args := append(append([]string{"--no-pager", "ls-remote"}, op.opts...), p.raw[p.names[0]][0])
		runs = [][]string{append(args, op.positional[min(1, len(op.positional)):]...)}
	default:
		// A push goes by the remote's name: its URLs are held by the sandbox.
		runs = [][]string{append(append([]string{"--no-pager", "push"}, op.opts...), op.positional...)}
	}
	return hostScript(ctx, cwd, env, runs), p
}

// pull fetches on the host, at once, then merges or rebases in the
// sandbox: what was fetched, then the pull's own merge from the repository
// itself, so no checkout, filter, driver or hook runs on the host.
func (b *HostBroker) pull(ctx context.Context, cwd string, op remoteOp, p plan) *exec.Cmd {
	n := p.names[0]
	var branch string
	switch {
	case len(op.positional) > 1:
		branch = fullRef(op.positional[1])
	default:
		cur := currentBranch(ctx, cwd)
		merge, _ := gitOut(ctx, cwd, "config", "--get", "branch."+cur+".merge")
		remote, _ := gitOut(ctx, cwd, "config", "--get", "branch."+cur+".remote")
		if cur == "" || merge == "" || remote != n {
			return &exec.Cmd{Err: fmt.Errorf("git pull: the current branch has no upstream on %s to merge; name it (git pull %s <branch>)", n, n)}
		}
		branch = merge
	}
	tracking, _ := mapRef(p.specs[n], branch)
	if tracking == "" {
		return &exec.Cmd{Err: fmt.Errorf("git pull: %s's fetch refspecs keep no copy of %s; run git fetch %s, then git merge FETCH_HEAD", n, branch, n)}
	}
	var fetchOpts, mergeOpts []string
	for _, o := range op.opts {
		name, _, _ := strings.Cut(o, "=")
		if pullFetch[name] {
			fetchOpts = append(fetchOpts, o)
		}
		if !pullFetch[name] || name == "-q" || name == "--quiet" || name == "-v" || name == "--verbose" {
			mergeOpts = append(mergeOpts, o)
		}
	}
	env, err := repoEnv(ctx, cwd, remoteConfig(ctx, cwd, p.names))
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	fp := p
	fp.names = []string{n}
	host := hostScript(ctx, cwd, env, fetchRuns(remoteOp{verb: "fetch"}, fp, fetchOpts))
	out, err := b.runNow(ctx, cwd, op, p, host)
	line := "printf '%s' " + shellQuote(out) + "; "
	if err != nil {
		return b.command(ctx, cwd, line+"exit "+strconv.Itoa(exitCode(err)))
	}
	words := append(append([]string{"git", "pull"}, mergeOpts...), ".", tracking)
	for i, w := range words {
		words[i] = shellQuote(w)
	}
	return b.command(ctx, cwd, line+strings.Join(words, " "))
}

// clone clones on the host, at once, with no checkout, into a folder of
// Abhed's own the sandbox cannot reach, moves it into the workspace folder
// by folder with no link followed, then checks it out in the sandbox.
func (b *HostBroker) clone(ctx context.Context, cwd string, op remoteOp, p plan) *exec.Cmd {
	parent, err := cloneParent()
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("git clone: %w", err)}
	}
	tmp, err := os.MkdirTemp(parent, "clone-*")
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("git clone: %w", err)}
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	staged := filepath.Join(tmp, "repo")
	checkout := true
	args := []string{"--no-pager", "clone", "--no-checkout"}
	for _, o := range op.opts {
		if o == "-n" || o == "--no-checkout" {
			checkout = false
			continue
		}
		args = append(args, o)
	}
	args = append(args, op.url, staged)
	// Run outside every repository: no configuration but the person's is read.
	env, err := hostEnv(Env(), append(append([][2]string(nil), safety...), remoteConfig(ctx, tmp, []string{"origin"})...), remoteEnv...)
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("git clone: %w", err)}
	}
	host := hostScript(ctx, tmp, env, [][]string{args})
	out, err := b.runNow(ctx, cwd, op, p, host)
	out = strings.ReplaceAll(out, staged, op.dir)
	line := "printf '%s' " + shellQuote(out) + "; "
	if err != nil {
		return b.command(ctx, cwd, line+"exit "+strconv.Itoa(exitCode(err)))
	}
	parts, err := b.cloneTarget(ctx, cwd, op.dir)
	if err == nil {
		err = moveInto(sandbox.RealPath(b.workspace), parts, staged)
	}
	if err != nil {
		return b.command(ctx, cwd, line+"printf '%s\\n' "+shellQuote("git clone: "+err.Error())+" >&2; exit 1")
	}
	dest := filepath.Join(append([]string{sandbox.RealPath(b.workspace)}, parts...)...)
	b.learn(ctx, dest, true)
	if !checkout {
		return b.command(ctx, cwd, line+"true")
	}
	d := shellQuote(dest)
	return b.command(ctx, cwd, line+"if git -C "+d+" rev-parse -q --verify HEAD >/dev/null; then git -C "+d+" reset -q --hard; fi")
}

// cloneParent is the folder a clone is made in before it is moved into the
// workspace: the person's own clone folder, which no command can reach;
// tests set it.
var cloneParent = func() (string, error) { return privateDir("clone") }

// privateDir is the person's folder name under their Abhed folder, made
// private, outside every folder the sandbox's commands can write, with
// what a killed process left there over a day ago swept away.
func privateDir(name string) (string, error) {
	dir, err := home.PersonPath(name)
	if err != nil {
		return "", err
	}
	for _, a := range sandbox.WritableAreas() {
		if _, in := sandbox.Within(dir, a); in {
			return "", fmt.Errorf("%s is inside %s, which commands can write", dir, a)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a folder private to you", dir)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return dir, nil
}

// runNow runs a host step to its end, recorded as it starts and ends, and
// gives its output, redacted.
func (b *HostBroker) runNow(ctx context.Context, cwd string, op remoteOp, p plan, cmd *exec.Cmd) (string, error) {
	if cmd.Err != nil {
		return "", cmd.Err
	}
	p.early = true
	// Its own session: a cancel ends git and ssh with it, and nothing else.
	sandbox.EndWithCommand(cmd)
	b.recordStart(ctx, cwd, op, p, cmd)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Start()
	if err != nil {
		sandbox.Release(cmd)
		return "", err
	}
	err = cmd.Wait()
	sandbox.Exited(cmd, err)
	return redact(out.String()), err
}

// exitCode is err's exit code, 1 when it has none.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ee.ExitCode()
	}
	return 1
}

// refs are a repository's refs and HEAD, name to object, for what a command changed.
func refs(ctx context.Context, root string) map[string]string {
	out, err := gitOut(ctx, root, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, obj, ok := strings.Cut(line, " "); ok {
			m[name] = obj
		}
	}
	if head, err := gitOut(ctx, root, "rev-parse", "-q", "--verify", "HEAD"); err == nil {
		m["HEAD"] = head
	}
	return m
}

// changedRefs are the refs that differ between before and after, at most 200.
func changedRefs(before, after map[string]string) []map[string]string {
	var names []string
	for n := range after {
		if before[n] != after[n] {
			names = append(names, n)
		}
	}
	for n := range before {
		if _, ok := after[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []map[string]string
	for _, n := range names {
		if len(out) == 200 {
			out = append(out, map[string]string{"ref": "(more not listed)"})
			break
		}
		c := map[string]string{"ref": n}
		if v := before[n]; v != "" {
			c["from"] = v
		}
		if v := after[n]; v != "" {
			c["to"] = v
		}
		out = append(out, c)
	}
	return out
}

// recordStart records the brokered command before it runs and, once it has
// run, what it changed.
func (b *HostBroker) recordStart(ctx context.Context, cwd string, op remoteOp, p plan, cmd *exec.Cmd) {
	launch := sandbox.LaunchOf(ctx)
	remotes := map[string][]string{}
	for n, urls := range p.remotes {
		for _, u := range urls {
			remotes[n] = append(remotes[n], anonymize(u))
		}
	}
	network := "open"
	switch {
	case b.net.Allows != nil:
		network = "allowlist"
	case b.net.Off():
		network = "off"
	}
	hooks := hooksNotRun(ctx, cwd)
	base := func() map[string]any {
		pay := map[string]any{"command": "git " + op.verb, "workspace": b.workspace, "cwd": cwd, "network": network}
		switch {
		case p.hooks != "":
			pay["hooks_run"] = []map[string]string{{"scope": p.hooksScope, "path": p.hooks, "hook": "pre-push", "where": "sandbox"}}
		case len(hooks) > 0:
			pay["hooks_not_run"] = hooks
		}
		if launch.CallID != "" {
			pay["call_id"] = launch.CallID
		}
		if len(remotes) > 0 {
			pay["remotes"] = remotes
		}
		if len(p.hosts) > 0 {
			pay["hosts"] = p.hosts
		}
		if p.proxy != "" {
			pay["proxy"] = anonymize(p.proxy)
		}
		if p.step != "" {
			pay["asked"] = p.step
		}
		if op.force {
			pay["force"] = true
		}
		if p.early {
			// Reached the network as the call's command was built, before it started.
			pay["before_start"] = true
		}
		return pay
	}
	var before map[string]string
	if op.verb != "clone" && op.verb != "ls-remote" {
		before = refs(ctx, p.root)
	}
	if launch.Record != nil {
		pay := base()
		pay["stage"] = "started"
		_ = launch.Record(EvBrokered, pay)
	}
	sandbox.OnExit(cmd, func(err error) {
		after := context.WithoutCancel(ctx)
		pay := base()
		pay["stage"] = "exited"
		code := 0
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		pay["exit_code"] = code
		if p.relay != nil {
			if ran, code := p.relay.close(); ran {
				pay["pre_push_exit_code"] = code
			}
		}
		if before != nil {
			if changed := changedRefs(before, refs(after, p.root)); len(changed) > 0 {
				pay["changed"] = changed
			}
		}
		if launch.Record != nil {
			_ = launch.Record(EvBrokered, pay)
		}
	})
}

// RemoteFamily is the grant family a brokered remote line belongs to, for a
// person's "always for this repository": git fetch/pull for fetch, pull and
// ls-remote naming a remote by name, git push for a push that neither
// forces nor deletes. "" for any other line, a clone among them.
func RemoteFamily(line string) string {
	op, err, ok := parseRemote(line)
	if !ok || err != nil {
		return ""
	}
	if op.remote != "" {
		if _, _, err := remoteHost(op.remote); err == nil || strings.ContainsAny(op.remote, "/:@") {
			return ""
		}
	}
	switch op.verb {
	case "fetch", "pull", "ls-remote":
		return FamilyFetch
	case "push":
		if !op.force {
			return FamilyPush
		}
	}
	return ""
}

// The grant families of brokered remote commands.
const (
	FamilyFetch = "git fetch/pull"
	FamilyPush  = "git push"
)
