package hostgit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// The sandbox keeps the person's SSH keys, SSH agent and credential helpers
// from commands, so git fetch, pull, push and clone cannot authenticate
// there. A command that is exactly one of them, in plain words with the usual
// options, runs through host git instead: as the person, with their SSH agent,
// keychain and credential helpers, while the sandboxed agent never reads a key.
//
// Host git reaches only the remotes the repository had when the session
// started (a remote added or changed since is asked about each time), or a
// clone URL a person approved. The repository's own configuration cannot name
// a program for it to run: the safety settings stay on, the SSH command, askpass
// and credential helpers are the person's global ones, and a repository that
// names a hook or remote helper in its configuration is not brokered.

// EvBrokered is one git command run through host git for the sandbox:
// recorded when it starts and again, with the refs it changed, when it ends.
const EvBrokered = "hostgit.brokered"

// Network is what the sandbox's network lets host git reach for it.
type Network struct {
	// Open is the network on with no allowlist: any host.
	Open bool
	// Allows, under the egress allowlist, says whether host and port are
	// allowed, and if not why; nil when the allowlist is not in force.
	Allows func(host string, port uint16) (bool, string)
}

// Off reports whether the sandbox's network is off.
func (n Network) Off() bool { return !n.Open && n.Allows == nil }

// Ask steps the loop gives a brokered call: hostgit is answered by a person,
// or by a grant they kept for the repository; hostgitEach by a person each time.
const (
	StepHostGit     = "hostgit"
	StepHostGitEach = "hostgit_each"
)

// HostBroker runs the brokered git commands on the host; see Broker.
type HostBroker struct {
	workspace string
	tier      func() sandbox.Tier
	net       Network
	command   func(ctx context.Context, cwd, line string) *exec.Cmd

	mu sync.Mutex
	// known are the repositories whose remotes host git may reach, by their
	// real top folder: the workspace's own, as at the session's start, and
	// those cloned for it, as they were cloned.
	known map[string]string
	// judged are the remote settings each call was judged on, by call id.
	judged map[string]judgement
}

type judgement struct {
	settings string
	at       time.Time
}

// NewBroker is a Broker for workspace. It reads the remote settings of the
// workspace's repository now, as the ones the session starts with.
func NewBroker(workspace string, tier func() sandbox.Tier, network Network, command func(ctx context.Context, cwd, line string) *exec.Cmd) *HostBroker {
	b := &HostBroker{workspace: workspace, tier: tier, net: network, command: command,
		known: map[string]string{}, judged: map[string]judgement{}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if root := b.repoIn(workspace); root != "" {
		if s, err := remoteSettings(ctx, root); err == nil {
			b.known[root] = s
		}
	}
	return b
}

// Broker wraps a sandbox's Command so the git commands the sandbox keeps from
// working run through host git: worktree add, remove and prune and submodule
// update (the sandbox holds git's folders), and fetch, pull, push, clone,
// ls-remote and remote -v (it hides the person's keys). Each must be the
// whole command line, in plain words. Anything else goes to command as it
// was. Only the process tier and the fence are brokered, as only they run
// commands on this host. Unless netOpen (network on, no allowlist), a
// submodule update must say --no-fetch and can only check out what is there.
func Broker(workspace string, tier func() sandbox.Tier, netOpen bool, command func(ctx context.Context, cwd, line string) *exec.Cmd) func(ctx context.Context, cwd, line string) *exec.Cmd {
	return NewBroker(workspace, tier, Network{Open: netOpen}, command).Command
}

// onHost reports whether the tier runs commands on this host.
func (b *HostBroker) onHost() bool {
	if b.tier == nil {
		return false
	}
	switch b.tier() {
	case sandbox.TierProcess, sandbox.TierFence:
		return true
	}
	return false
}

// Command builds line's process: through host git for a brokered command,
// else as command builds it.
func (b *HostBroker) Command(ctx context.Context, cwd, line string) *exec.Cmd {
	if op, err, ok := parseRemote(line); ok {
		if !b.onHost() {
			return b.command(ctx, cwd, line)
		}
		if err != nil {
			return &exec.Cmd{Err: err}
		}
		return b.remoteCommand(ctx, cwd, line, op)
	}
	args, ok := brokered(line)
	if !ok || !b.onHost() {
		return b.command(ctx, cwd, line)
	}
	if err := brokerPaths(b.workspace, cwd, args); err != nil {
		return &exec.Cmd{Err: err}
	}
	var env []string
	if args[0] == "submodule" && !b.net.Open {
		if !slices.Contains(args, "--no-fetch") && !slices.Contains(args, "-N") {
			return &exec.Cmd{Err: errors.New("git submodule update: the sandbox's network is off or held to an allowlist, " +
				"and git run for it on the host would reach the network outside both; add --no-fetch to check out " +
				"what is already fetched, or fetch the submodules yourself")}
		}
		// --no-fetch still clones a submodule not yet cloned; no transport is allowed.
		env = []string{"GIT_ALLOW_PROTOCOL=" + noProtocol}
	}
	// On the host, whatever sandbox is registered for the workspace.
	return New(ctx, cwd).commandWith(ctx, nil, env, true, args...)
}

// Ask is why the call callID, running line in cwd, needs a person's approval
// before host git runs it, and the step to ask it under; "" when policy
// decides alone. An error refuses it before anyone is asked. The remote
// settings it judged are kept for the call, and it runs only on those.
func (b *HostBroker) Ask(callID, cwd, line string) (reason, step string, err error) {
	op, perr, ok := parseRemote(line)
	if !ok || !b.onHost() {
		return "", "", nil
	}
	if perr != nil {
		return "", "", perr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	plan, err := b.plan(ctx, cwd, op)
	if err != nil || plan.sandboxed {
		return "", "", err
	}
	if callID != "" {
		b.mu.Lock()
		// Calls refused or declined leave theirs; the oldest go first.
		if len(b.judged) >= 256 {
			oldest := ""
			for id, j := range b.judged {
				if oldest == "" || j.at.Before(b.judged[oldest].at) {
					oldest = id
				}
			}
			delete(b.judged, oldest)
		}
		b.judged[callID] = judgement{settings: plan.settings, at: time.Now()}
		b.mu.Unlock()
	}
	return plan.ask, plan.step, nil
}

// remoteCommand builds a brokered remote command, or says why it is not run.
func (b *HostBroker) remoteCommand(ctx context.Context, cwd, line string, op remoteOp) *exec.Cmd {
	plan, err := b.plan(ctx, cwd, op)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	if plan.sandboxed {
		return b.command(ctx, cwd, line)
	}
	launch := sandbox.LaunchOf(ctx)
	b.mu.Lock()
	j, judged := b.judged[launch.CallID]
	if judged {
		delete(b.judged, launch.CallID)
	}
	b.mu.Unlock()
	switch {
	case judged && j.settings != plan.settings:
		return &exec.Cmd{Err: errors.New("git " + op.verb + ": the repository's remote settings changed after the call was approved, " +
			"so Abhed does not run it on the host; ask again")}
	case !judged && plan.ask != "":
		// Built outside a judged call (a hook's command), nothing asked a person.
		return &exec.Cmd{Err: fmt.Errorf("git %s: %s, so it needs your approval as an agent's call", op.verb, plan.ask)}
	}
	cmd := b.hostCommand(ctx, cwd, op, plan)
	if cmd.Err != nil {
		return cmd
	}
	b.recordStart(ctx, cwd, op, plan, cmd)
	return cmd
}

// remoteOp is a brokered remote command, as parsed.
type remoteOp struct {
	verb string
	// args are git's arguments, the verb first.
	args []string
	// remote is the remote named, "" for the default; all is fetch --all.
	remote string
	all    bool
	// url and dir are a clone's.
	url, dir string
	// force is a push that can lose commits on the remote: a force or a delete.
	force bool
}

// remoteVerbs are the brokered remote commands, with the options each takes
// in plain words; an option taking its value in the next word is listed in
// cloneValueFlags. Any other option leaves the line to the sandbox.
var remoteVerbs = map[string]map[string]bool{
	"fetch": set("-q", "--quiet", "-v", "--verbose", "-p", "--prune", "-P", "--prune-tags", "-t", "--tags", "-n", "--no-tags",
		"--all", "--dry-run", "-f", "--force", "--progress", "--no-progress", "--atomic", "--unshallow", "--depth", "--deepen",
		"--no-write-fetch-head", "--write-fetch-head", "--keep"),
	"pull": set("-q", "--quiet", "-v", "--verbose", "--ff-only", "--ff", "--no-ff", "--rebase", "--no-rebase", "--no-edit",
		"-p", "--prune", "-t", "--tags", "--no-tags", "--depth", "--unshallow", "--autostash", "--no-autostash", "--stat",
		"--no-stat", "-n", "--commit", "--no-commit", "--progress", "--no-progress", "--signoff", "--no-signoff"),
	"push": set("-q", "--quiet", "-v", "--verbose", "-u", "--set-upstream", "--tags", "--follow-tags", "--no-follow-tags",
		"--dry-run", "-n", "--porcelain", "--atomic", "--no-atomic", "--no-verify", "--verify", "--progress", "--no-progress",
		"-f", "--force", "--force-with-lease", "--force-if-includes", "--no-force-with-lease", "-d", "--delete"),
	"ls-remote": set("-q", "--quiet", "-h", "--heads", "-b", "--branches", "-t", "--tags", "--refs", "--symref", "--exit-code"),
	"clone": set("-q", "--quiet", "-v", "--verbose", "--depth", "--single-branch", "--no-single-branch", "--no-tags",
		"-b", "--branch", "-n", "--no-checkout", "--filter", "--progress", "--no-progress"),
	"remote": set("-v", "--verbose"),
}

// globalValued are git's options before the verb that take the next word.
var globalValued = set("-c", "-C", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix", "--exec-path")

// cloneValueFlags are clone's options taking their value in the next word
// when not given with =.
var cloneValueFlags = set("-b", "--branch")

// valued are options whose value must come with =.
var valued = set("--depth", "--deepen", "--filter")

// forceFlags make a push one that can lose commits on the remote.
var forceFlags = set("-f", "--force", "--force-with-lease", "--force-if-includes", "-d", "--delete")

// refusedFlags name a program or a file for git to run or read, or more
// configuration, in some verb's options; a line with one is refused on the
// host rather than left to the sandbox.
var refusedFlags = set("--upload-pack", "--receive-pack", "--exec", "--config", "--config-env", "--template",
	"--separate-git-dir", "--reference", "--reference-if-able", "--bundle-uri", "--git-dir", "--work-tree",
	"--exec-path", "--namespace", "--super-prefix")

// refusedShort are short options that name a program in these verbs:
// clone's and ls-remote's -u (the upload pack), clone's -c (configuration).
var refusedShort = map[string]map[string]bool{
	"clone":     set("-u", "-c"),
	"ls-remote": set("-u"),
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// plainLine reports a line in plain words: no quoting, expansion, redirection
// or second command.
func plainLine(line string) bool {
	for _, r := range line {
		plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" ./_-+@:=,~^", r)
		if !plain {
			return false
		}
	}
	return true
}

// parseRemote reads line as a brokered remote command. ok is false for a
// line that is not one, which goes on as before; ok with an error is one
// the host refuses: a setting given before the verb or in a variable, an
// option naming a program, or a transport that runs one.
func parseRemote(line string) (op remoteOp, err error, ok bool) {
	line = strings.TrimSpace(line)
	words := strings.Fields(line)
	// Variables set in front of git, such as GIT_SSH_COMMAND.
	i := 0
	for i < len(words) && strings.Contains(words[i], "=") && !strings.HasPrefix(words[i], "-") && !strings.HasPrefix(words[i], "=") {
		i++
	}
	if i >= len(words) || words[i] != "git" {
		return op, nil, false
	}
	assigned := i > 0
	rest := words[i+1:]
	// Options before the verb: -c, -C, --git-dir and the like.
	j := 0
	for j < len(rest) && strings.HasPrefix(rest[j], "-") {
		if globalValued[rest[j]] {
			j++ // its value
		}
		j++
	}
	if j >= len(rest) {
		return op, nil, false
	}
	verb := rest[j]
	if _, brokerable := remoteVerbs[verb]; !brokerable {
		return op, nil, false
	}
	// A line with more on it runs in the sandbox, as it always did.
	if !plainLine(line) {
		return op, nil, false
	}
	switch {
	case assigned:
		return op, fmt.Errorf("git %s: a variable set before git (%s) could name a program for host git to run; "+
			"Abhed runs git %s on the host only as the plain command, with your own git settings", verb, words[0], verb), true
	case j > 0:
		return op, fmt.Errorf("git %s: an option before the command (%s) could change what host git runs; "+
			"Abhed runs git %s on the host only as the plain command, with your own git settings", verb, rest[0], verb), true
	}
	op = remoteOp{verb: verb, args: rest[j:]}
	args := rest[j+1:]
	if why := refusedWord(verb, args); why != "" {
		return op, fmt.Errorf("git %s: %s, so Abhed does not run it on the host", verb, why), true
	}
	var positional []string
	expect := false
	for _, a := range args {
		if expect {
			expect = false // the option's value
			continue
		}
		if a == "--" {
			return op, nil, false
		}
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		if !remoteVerbs[verb][name] {
			return op, nil, false
		}
		nextValue := verb == "clone" && cloneValueFlags[name]
		switch {
		case hasValue && name == "--rebase":
			if value != "true" && value != "false" && value != "merges" {
				return op, nil, false
			}
		case hasValue && !valued[name] && !nextValue && name != "--force-with-lease":
			return op, nil, false
		case !hasValue && valued[name]:
			return op, nil, false
		case !hasValue && nextValue:
			expect = true
		}
		if verb == "fetch" && name == "--all" {
			op.all = true
		}
		if verb == "push" && forceFlags[name] {
			op.force = true
		}
	}
	if expect {
		return op, nil, false
	}
	switch verb {
	case "remote":
		if len(args) != 1 || len(positional) != 0 {
			return op, nil, false
		}
	case "clone":
		if len(positional) < 1 || len(positional) > 2 {
			return op, nil, false
		}
		op.url = positional[0]
		if _, _, err := remoteHost(op.url); err != nil {
			return op, fmt.Errorf("git clone: %v", err), true
		}
		if len(positional) == 2 {
			op.dir = positional[1]
		} else {
			op.dir = humanish(op.url)
		}
		if op.dir == "" || strings.HasPrefix(op.dir, "-") {
			return op, nil, false
		}
	default:
		if len(positional) > 0 {
			op.remote = positional[0]
			if op.all {
				return op, nil, false
			}
		}
		if verb == "push" {
			for _, spec := range positional[min(1, len(positional)):] {
				// +src:dst forces; :dst deletes.
				if strings.HasPrefix(spec, "+") || strings.HasPrefix(spec, ":") {
					op.force = true
				}
			}
		}
	}
	return op, nil, true
}

// refusedWord says why args hold a word host git must not be given, or "".
func refusedWord(verb string, args []string) string {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		lower := strings.ToLower(a)
		switch {
		case refusedFlags[name]:
			return "the option " + name + " names a program or configuration for git"
		case refusedShort[verb][name], strings.HasPrefix(a, "-c") && verb == "clone" && len(a) > 2 && !strings.HasPrefix(a, "--"):
			return "the option " + name + " names a program or configuration for git"
		case strings.Contains(lower, "sshcommand"), strings.Contains(lower, "ssh_command"), strings.Contains(lower, "uploadpack"),
			strings.Contains(lower, "receivepack"), strings.Contains(lower, "gitproxy"), strings.Contains(lower, "askpass"),
			strings.Contains(lower, "credential.helper"):
			return "the word " + a + " names a program for git to run"
		case strings.Contains(a, "::"), strings.HasPrefix(lower, "ext:"), strings.HasPrefix(lower, "file:"), strings.HasPrefix(lower, "fd:"):
			return "the word " + a + " names a transport that runs a program or reads local files"
		}
	}
	return ""
}

// humanish is the folder git clone makes for url when none is named.
func humanish(u string) string {
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, "/.git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.TrimSuffix(u, ".git")
	if u == "" || u == "." || u == ".." {
		return ""
	}
	return u
}

// remoteHost is the host and port a git URL reaches and its transport:
// ssh (ssh://, or user@host:path) or https only.
func remoteHost(raw string) (host string, port uint16, err error) {
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "ssh://"), strings.HasPrefix(lower, "git+ssh://"), strings.HasPrefix(lower, "ssh+git://"):
		u, perr := url.Parse(raw)
		if perr != nil || u.Hostname() == "" {
			return "", 0, fmt.Errorf("%s is not a URL git can reach", anonymize(raw))
		}
		port = 22
		if u.Scheme == "https" {
			port = 443
		}
		if p := u.Port(); p != "" {
			n, perr := strconv.ParseUint(p, 10, 16)
			if perr != nil || n == 0 {
				return "", 0, fmt.Errorf("%s names no port git can reach", anonymize(raw))
			}
			port = uint16(n)
		}
		return strings.ToLower(u.Hostname()), port, nil
	case strings.Contains(raw, "://"):
		return "", 0, fmt.Errorf("%s: only ssh and https remotes are reached through host git", anonymize(raw))
	}
	// scp-like: [user@]host:path, the colon before any slash.
	colon := strings.Index(raw, ":")
	if colon <= 0 || strings.Contains(raw[:colon], "/") || strings.HasPrefix(raw, "-") {
		return "", 0, fmt.Errorf("%s is a local path, not a remote; only ssh and https remotes are reached through host git", raw)
	}
	h := raw[:colon]
	if at := strings.LastIndex(h, "@"); at >= 0 {
		h = h[at+1:]
	}
	h = strings.Trim(h, "[]")
	if h == "" || strings.HasPrefix(h, "-") {
		return "", 0, fmt.Errorf("%s names no host", raw)
	}
	return strings.ToLower(h), 22, nil
}

// anonymize is a URL without a password or token, for output and the record.
func anonymize(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(a URL that could not be read)"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has || u.Scheme == "https" {
			u.User = nil
		}
	}
	return u.String()
}

// plan is what a brokered command will do.
type plan struct {
	// sandboxed is a line left to the sandbox: a repository Abhed has not
	// read the remotes of, or a remote it does not have.
	sandboxed bool
	root      string
	// settings are the repository's remote settings now.
	settings string
	// remotes are the remotes reached, each with its URLs.
	remotes map[string][]string
	hosts   []string
	// ask is why a person must approve it; step how.
	ask, step string
}

// plan reads what op in cwd would reach, and why it is refused or asked.
func (b *HostBroker) plan(ctx context.Context, cwd string, op remoteOp) (plan, error) {
	if !b.inWorkspace(cwd) {
		return plan{}, fmt.Errorf("git %s: the command runs outside the workspace, so Abhed does not run it on the host", op.verb)
	}
	if op.verb == "clone" {
		return b.planClone(cwd, op)
	}
	root := b.repoIn(cwd)
	b.mu.Lock()
	start, known := b.known[root]
	b.mu.Unlock()
	if root == "" || !known {
		// Not a repository Abhed read at the start, or one it cloned.
		return plan{sandboxed: true}, nil
	}
	settings, err := remoteSettings(ctx, root)
	if err != nil {
		return plan{}, fmt.Errorf("git %s: the repository's settings could not be read (%v), so Abhed does not run it on the host", op.verb, err)
	}
	if why := plantedProgram(settings); why != "" {
		return plan{}, fmt.Errorf("git %s: the repository's configuration %s, which host git would run as you; "+
			"Abhed does not run git %s on the host while it does", op.verb, why, op.verb)
	}
	p := plan{root: root, settings: settings, remotes: map[string][]string{}}
	if op.verb == "remote" {
		return p, nil
	}
	names, err := b.remotesOf(ctx, cwd, op)
	if err != nil {
		return plan{}, err
	}
	if len(names) == 0 {
		return plan{sandboxed: true}, nil
	}
	push := op.verb == "push"
	for _, n := range names {
		urls, err := remoteURLs(ctx, cwd, n, push)
		if err != nil || len(urls) == 0 {
			return plan{}, fmt.Errorf("git %s: the remote %s has no URL host git could read", op.verb, n)
		}
		p.remotes[n] = urls
		for _, u := range urls {
			host, port, err := remoteHost(u)
			if err != nil {
				return plan{}, fmt.Errorf("git %s: the remote %s: %v", op.verb, n, err)
			}
			if b.net.Allows != nil {
				if ok, why := b.net.Allows(host, port); !ok {
					return plan{}, fmt.Errorf("git %s: the egress allowlist does not allow %s:%d (%s), the remote %s's host, "+
						"so host git does not reach it either; add it to the allowlist", op.verb, host, port, why, n)
				}
			}
			if !slices.Contains(p.hosts, host) {
				p.hosts = append(p.hosts, host)
			}
		}
	}
	sort.Strings(p.hosts)
	where := strings.Join(p.hosts, ", ")
	who := "runs through host git as you, with your SSH agent, keychain and credential helpers,"
	switch {
	case settings != start:
		p.ask, p.step = fmt.Sprintf("git %s %s and the repository's remote settings changed during this session, "+
			"so host git reaches %s only with your approval, asked each time", op.verb, who, where), StepHostGitEach
	case push && op.force:
		p.ask, p.step = fmt.Sprintf("git %s %s and can replace or delete commits on %s; this exact form is asked each time", op.verb, who, where), StepHostGitEach
	case b.net.Off():
		p.ask, p.step = fmt.Sprintf("git %s %s and reaches %s although the sandbox's network is off; "+
			"only this repository's configured remote is reached, and only with your approval", op.verb, who, where), StepHostGit
	}
	return p, nil
}

// planClone is a clone's plan: the URL is the person's to approve, every time.
func (b *HostBroker) planClone(cwd string, op remoteOp) (plan, error) {
	host, port, err := remoteHost(op.url)
	if err != nil {
		return plan{}, fmt.Errorf("git clone: %v", err)
	}
	dir := op.dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	if !b.inWorkspace(filepath.Dir(filepath.Clean(dir))) || filepath.Clean(dir) == filepath.Clean(b.workspace) {
		return plan{}, fmt.Errorf("git clone: %s is outside the workspace, so Abhed does not run it on the host", op.dir)
	}
	if b.net.Allows != nil {
		if ok, why := b.net.Allows(host, port); !ok {
			return plan{}, fmt.Errorf("git clone: the egress allowlist does not allow %s:%d (%s), so host git does not reach it either; add it to the allowlist", host, port, why)
		}
	}
	p := plan{root: filepath.Clean(dir), hosts: []string{host}, remotes: map[string][]string{"origin": {op.url}}, step: StepHostGitEach}
	p.ask = fmt.Sprintf("git clone runs through host git as you, with your SSH agent, keychain and credential helpers, "+
		"and copies %s into %s; a clone is asked each time", anonymize(op.url), op.dir)
	if b.net.Off() {
		p.ask += ", and reaches " + host + " although the sandbox's network is off"
	}
	return p, nil
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
	if branch, err := gitOut(ctx, cwd, "symbolic-ref", "-q", "--short", "HEAD"); err == nil && branch != "" {
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

// configured are the repository's remotes.
func configured(ctx context.Context, cwd string) ([]string, error) {
	out, err := gitOut(ctx, cwd, "remote")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
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
	out, err := gitCmd(ctx, cwd, true, append(Env(), configEnv(safety)...), append([]string{"-C", cwd}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// settingKeys are the configuration a fetch, pull or push reads to choose
// where it goes and how it authenticates, or that names a program.
var settingKeys = []string{"remote.", "url.", "http.", "credential.", "ssh.", "protocol.", "include.", "includeif.",
	"hook.", "core.sshcommand", "core.gitproxy", "core.askpass", "core.worktree", "core.alternaterefscommand", "uploadpack."}

// remoteSettings are the repository's settings among settingKeys, from
// every file git reads (includes followed), in order, as one string.
func remoteSettings(ctx context.Context, root string) (string, error) {
	out, err := gitCmd(ctx, root, true, append(Env(), configEnv(safety)...), "-C", root, "config", "--includes", "-z", "--show-scope", "--list").Output()
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

// plantedProgram says what of settings names a program host git would run
// whatever the command line sets: a hook or a remote helper in the
// repository's own configuration. "" when nothing does.
func plantedProgram(settings string) string {
	parts := strings.Split(settings, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		scope := parts[i]
		if scope != "local" && scope != "worktree" {
			continue
		}
		key, _, _ := strings.Cut(parts[i+1], "\n")
		lower := strings.ToLower(key)
		switch {
		case strings.HasPrefix(lower, "hook."):
			return "names a hook (" + key + ")"
		case strings.HasPrefix(lower, "remote.") && strings.HasSuffix(lower, ".vcs"):
			return "names a remote helper (" + key + ")"
		case lower == "core.worktree":
			return "moves the work tree elsewhere (" + key + ")"
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

// remoteConfig is the configuration host git runs a remote command with,
// after safety: the person's own SSH command, askpass, proxy and credential
// helpers, read from their global and system files, and never the
// repository's; ssh and https only; no editor, signature check or submodule.
func remoteConfig(ctx context.Context, cwd string) [][2]string {
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
		{"core.alternateRefsCommand", ""},
		{"protocol.ssh.allow", "always"},
		{"merge.verifySignatures", "false"},
		{"fetch.recurseSubmodules", "false"},
		{"push.recurseSubmodules", "no"},
		{"core.editor", ":"},
		{"sequence.editor", ":"},
	}
	// Each credential helper key any file names is emptied, then given the
	// person's global and system helpers for it, in order.
	keys := []string{"credential.helper"}
	if out, err := gitCmd(ctx, cwd, true, append(Env(), configEnv(safety)...), "-C", cwd, "config", "--includes", "-z", "--name-only",
		"--get-regexp", `^credential\..*\.helper$`).Output(); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Split(splitNUL)
		for sc.Scan() {
			if k := sc.Text(); !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	for _, k := range keys {
		cfg = append(cfg, [2]string{k, ""})
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
	out, err := gitCmd(ctx, cwd, true, append(Env(), configEnv(safety)...), "-C", cwd, "config", "--includes", "-z", "--show-scope", "--get-all", key).Output()
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

// remoteEnv is the environment a remote command adds: ssh and https, no
// prompt, and no editor.
var remoteEnv = []string{"GIT_ALLOW_PROTOCOL=ssh:https", "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:", "GIT_MERGE_AUTOEDIT=no"}

// hostCommand is op's process on the host.
func (b *HostBroker) hostCommand(ctx context.Context, cwd string, op remoteOp, p plan) *exec.Cmd {
	cfg := remoteConfig(ctx, cwd)
	if op.verb != "clone" {
		return New(ctx, cwd).commandWith(ctx, cfg, remoteEnv, true, op.args...)
	}
	// A clone is no command on cwd's repository: its git folders are not named.
	r := New(ctx, cwd)
	if r.unknown != nil {
		return &exec.Cmd{Err: r.unknown}
	}
	all := append(append(append([][2]string(nil), safety...), r.drivers...), cfg...)
	return gitCmd(ctx, cwd, true, append(append(Env(), configEnv(all)...), remoteEnv...), append([]string{"-C", cwd}, op.args...)...)
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
// run, what it changed; a clone's repository is then one host git may reach.
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
		if len(hooks) > 0 {
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
		if p.step != "" {
			pay["asked"] = p.step
		}
		if op.force {
			pay["force"] = true
		}
		return pay
	}
	var before map[string]string
	if op.verb != "clone" && op.verb != "remote" && op.verb != "ls-remote" {
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
		switch {
		case op.verb == "clone" && code == 0:
			pay["repository"] = p.root
			if s, err := remoteSettings(after, p.root); err == nil {
				if root := b.repoIn(p.root); root != "" {
					b.mu.Lock()
					b.known[root] = s
					b.mu.Unlock()
				}
			}
		case before != nil:
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
// person's "always for this repository": git fetch/pull for fetch, pull,
// ls-remote and remote -v naming a remote by name, git push for a push that
// neither forces nor deletes. "" for any other line, a clone among them.
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
	case "fetch", "pull", "ls-remote", "remote":
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
