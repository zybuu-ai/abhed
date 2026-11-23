package hostgit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// The sandbox keeps the person's SSH keys, SSH agent and credential helpers
// from commands, so git fetch, pull, push and clone cannot authenticate
// there. A command that is exactly one of them, in plain words with the usual
// options, has its network part run through host git instead: as the person,
// with their SSH agent, keychain and credential helpers, while the sandboxed
// agent never reads a key. Only the network part: a pull's merge or rebase
// and a clone's checkout run in the sandbox, so no filter, driver or hook a
// repository names ever runs on the host.
//
// The repository's configuration is input the agent may have written, so
// host git is given only what it needs from it: fetch and ls-remote go to
// the remote's URL, read when the call was judged, with its fetch refspecs on
// the command line; the SSH command, askpass, proxy and credential helpers
// are the person's global ones; the safety settings, the pager and the
// upload and receive programs are pinned on the command line; and a
// repository whose own configuration names a hook, a remote helper, a proxy,
// an http setting, an upload or receive program or a URL rewrite is not
// brokered. Nor is any repository unless the sandbox holds git's
// configuration against its commands (protect_git, held), so what was read
// when the call was judged is what host git reads when it runs.

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
	// holds reports whether the sandbox holds git's configuration against
	// its commands; nil, or false, brokers no remote command. holdsNew
	// reports whether it holds a repository made during the session against
	// commands started before; nil, or false, brokers none such.
	holds, holdsNew func() bool
	// noRemotes leaves remote commands to the sandbox (a server's sessions),
	// as off does, saying why (the configuration turned it off).
	noRemotes bool
	off       string

	mu sync.Mutex
	// known are the repositories whose remotes host git may reach, by their
	// real top folder: the workspace's own, as at the session's start, and
	// those cloned for it, as they were cloned.
	known map[string]repoState
	// judged are the remote settings each call was judged on, by session and call.
	judged map[string]judgement
}

// repoState is what a known repository was when it became known.
type repoState struct {
	// cloned is a repository cloned during the session.
	cloned   bool
	settings string
	// common is the real common git folder, inside the workspace.
	common string
}

type judgement struct {
	settings string
	at       time.Time
}

// NewBroker is a Broker for workspace. It reads the remote settings of the
// workspace's repository now, as the ones the session starts with. No
// remote command is brokered until HoldsGit says the sandbox holds git's
// configuration.
func NewBroker(workspace string, tier func() sandbox.Tier, network Network, command func(ctx context.Context, cwd, line string) *exec.Cmd) *HostBroker {
	b := &HostBroker{workspace: workspace, tier: tier, net: network, command: command,
		known: map[string]repoState{}, judged: map[string]judgement{}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if root := b.repoIn(workspace); root != "" {
		b.learn(ctx, root, false)
	}
	return b
}

// HoldsGit sets what says whether the sandbox holds git's configuration in
// the workspace against its commands (sandbox.HoldsGit), and whether it
// holds a repository cloned during the session against commands already
// running (sandbox.HoldsNewGit).
func (b *HostBroker) HoldsGit(holds, holdsNew func() bool) *HostBroker {
	b.holds, b.holdsNew = holds, holdsNew
	return b
}

// NoRemotes leaves every remote command to the sandbox, as a server's
// sessions need: host git would run as the server's own account for whoever
// reaches it. Worktree commands are still brokered.
func (b *HostBroker) NoRemotes() *HostBroker {
	b.noRemotes = true
	return b
}

// RemotesOff leaves every remote command to the sandbox, as before 1.3.0,
// for why (sandboxconfig.HostGitOff), which the session facts give; ""
// leaves them brokered.
func (b *HostBroker) RemotesOff(why string) *HostBroker {
	b.off = why
	return b
}

// BrokersRemoteCommands reports whether b runs remote commands on the host.
func (b *HostBroker) BrokersRemoteCommands() bool { return !b.noRemotes && b.off == "" }

// learn makes root a known repository, as it is now; cloned marks one
// cloned during the session.
func (b *HostBroker) learn(ctx context.Context, root string, cloned bool) {
	s, err := remoteSettings(ctx, root)
	if err != nil {
		return
	}
	common := b.commonOf(ctx, root)
	if common == "" {
		return
	}
	b.mu.Lock()
	b.known[root] = repoState{settings: s, common: common, cloned: cloned}
	b.mu.Unlock()
}

// commonOf is root's real common git folder when it is inside the
// workspace, "" otherwise.
func (b *HostBroker) commonOf(ctx context.Context, root string) string {
	r := New(ctx, root)
	if r.unknown != nil || r.common == "" {
		return ""
	}
	c := sandbox.RealPath(r.common)
	if !b.inWorkspace(c) {
		return ""
	}
	return c
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

// held reports whether the sandbox holds git's configuration now.
func (b *HostBroker) held() bool { return b.holds != nil && b.holds() }

// Command builds line's process: through host git for a brokered command,
// else as command builds it.
func (b *HostBroker) Command(ctx context.Context, cwd, line string) *exec.Cmd {
	if op, err, ok := parseRemote(line); ok {
		// held first: it waits for a sandbox still being chosen, whose tier is then known.
		if !b.held() || !b.onHost() || !b.BrokersRemoteCommands() {
			return b.command(ctx, cwd, line)
		}
		if err != nil {
			return &exec.Cmd{Err: err}
		}
		return b.remoteCommand(ctx, cwd, line, op)
	}
	// Worktree commands too only where the sandbox holds git's configuration,
	// so the drivers read now are the ones host git sees.
	args, ok := brokered(line)
	if !ok || !b.held() || !b.onHost() {
		return b.command(ctx, cwd, line)
	}
	if err := brokerPaths(b.workspace, cwd, args); err != nil {
		return &exec.Cmd{Err: err}
	}
	// On the host, whatever sandbox is registered for the workspace.
	return New(ctx, cwd).commandWith(ctx, nil, nil, true, args...)
}

// Ask is why the call key (sandbox.Launch.Key: the session and the call's
// id), running line in cwd, needs a person's approval before host git runs
// it, the step to ask it under, and what a grant for it is bound to (the
// repository's real folder and its remotes as judged); "" when policy
// decides alone. An error refuses it before anyone is asked. The remote
// settings it judged are kept for the call, and it runs only on those.
func (b *HostBroker) Ask(key, cwd, line string) (reason, step, bind string, err error) {
	op, perr, ok := parseRemote(line)
	if !ok || !b.held() || !b.onHost() || !b.BrokersRemoteCommands() {
		return "", "", "", nil
	}
	if perr != nil {
		return "", "", "", perr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	plan, err := b.plan(ctx, cwd, op)
	if err != nil || plan.sandboxed {
		return "", "", "", err
	}
	if key != "" {
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
		b.judged[key] = judgement{settings: plan.settings, at: time.Now()}
		b.mu.Unlock()
	}
	return plan.ask, plan.step, plan.bind, nil
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
	j, judged := b.judged[launch.Key()]
	if judged {
		delete(b.judged, launch.Key())
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
	switch op.verb {
	case "pull":
		return b.pull(ctx, cwd, op, plan)
	case "clone":
		return b.clone(ctx, cwd, op, plan)
	}
	cmd, plan := b.hostCommand(ctx, cwd, op, plan)
	if cmd.Err != nil {
		if plan.relay != nil {
			plan.relay.close()
		}
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
	// opts are the options as given, positional the other words, after the verb.
	opts, positional []string
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
		"-b", "--branch", "-n", "--no-checkout", "--progress", "--no-progress"),
}

// pullFetch are a pull's options for its fetch, run on the host; the rest
// are for its merge or rebase, run in the sandbox, as are -q and -v too.
var pullFetch = set("-q", "--quiet", "-v", "--verbose", "-p", "--prune", "-t", "--tags", "--no-tags", "--depth",
	"--unshallow", "--progress", "--no-progress")

// globalValued are git's options before the verb that take the next word.
var globalValued = set("-c", "-C", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix", "--exec-path")

// cloneValueFlags are clone's options taking their value in the next word
// when not given with =.
var cloneValueFlags = set("-b", "--branch")

// valued are options whose value must come with =.
var valued = set("--depth", "--deepen")

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
			op.opts = append(op.opts, a)
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
		} else {
			op.opts = append(op.opts, a)
		}
		if verb == "push" && forceFlags[name] {
			op.force = true
		}
	}
	if expect {
		return op, nil, false
	}
	op.positional = positional
	switch verb {
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
		if verb == "pull" && len(positional) > 2 {
			return op, nil, false // an octopus merge
		}
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
