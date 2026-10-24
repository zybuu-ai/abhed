package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// The codes a verdict names its concern by.
const (
	CodeOK               = "ok"
	CodeUnverifiedTarget = "unverified-target"
	CodeUnverifiedHost   = "unverified-host"
	CodeUnknownProgram   = "unknown-program"
	CodeOutOfRemit       = "out-of-remit"
	CodeCredentialEgress = "credential-egress"
	CodeHoneytoken       = "honeytoken"
)

// Versions of the deterministic judgements, as verdicts carry them.
const (
	RulesVersion      = "rules-v1"
	HoneytokenVersion = "honeytoken-v1"
)

// Rules is the deterministic judge. It reads only the call, what the user
// said and what earlier calls showed, so its verdict on the same case is
// always the same, and it is fast enough to judge every call the fast path
// sends it.
type Rules struct {
	// LookPath reports whether a program can be found where commands run.
	// Nil leaves programs unchecked, as where commands run in a container
	// whose programs the host cannot see.
	LookPath func(program string) bool
	// Declared are the hosts and clusters the operator configured, by name
	// and address: the operator naming one is as good as the person naming it.
	Declared []string
	// Hosts are the configured ssh hosts, each by its name and address.
	Hosts []DeclaredHost
	// Home is the HOME a local command gets, so a ~ or $HOME/ target can be
	// placed against the workspace. Nil means no local home is known: a ~
	// target is then never placed, and one the person did not name is denied
	// as a home path outside the task. Production wires it from the bash
	// tool (internal/toolset); an SDK caller that leaves it nil gets that
	// stricter judgement.
	Home func() (string, bool)
}

// DeclaredHost is one configured ssh host.
type DeclaredHost struct{ Name, Addr string }

// Judge applies the rules: a deny for destructive work outside the remit or
// a program that is not there, an ask for a target nothing showed or a
// credential followed by a way out, and ok otherwise.
func (r Rules) Judge(_ context.Context, c Case) (Verdict, error) {
	k := facts(c)
	if r.Home != nil {
		if h, ok := r.Home(); ok {
			k.home, k.homeAvailable = h, true
		}
	}
	for _, d := range r.Declared {
		if d != "" {
			k.declared = append(k.declared, strings.ToLower(d))
		}
	}
	var ask *Verdict
	for _, check := range []func(*given, Case) *Verdict{r.drift, r.program, r.evidence, r.hostChoice, r.composition} {
		v := check(k, c)
		if v == nil {
			continue
		}
		v.Version = RulesVersion
		if v.Confidence == 0 {
			v.Confidence = 1
		}
		if v.Decision == policy.Deny {
			return *v, nil
		}
		if ask == nil {
			ask = v
		}
	}
	if ask != nil {
		return *ask, nil
	}
	return Verdict{Decision: policy.Allow, Code: CodeOK, Confidence: 1, Version: RulesVersion}, nil
}

// given is what the rules may take as given for one case.
type given struct {
	said string // everything the user said, lower case, the approved plan included
	// person is what the person said in their own messages only, with no
	// plan or runbook prose: what counts where the directory is unknown.
	person        string
	roots         []string
	home          string // the local home ~ stands for; "" when not known
	homeAvailable bool   // a local home was configured (even if blanked for a command)
	seen          func(string) bool
	// done are the subjects of earlier calls that succeeded.
	done     []string
	args     map[string]any
	declared []string
}

func facts(c Case) *given {
	k := &given{said: strings.ToLower(c.Said), seen: c.Seen}
	if k.said == "" {
		k.said = strings.ToLower(c.Remit)
	}
	k.person = k.said
	// What the plan the person approved names, they named.
	if c.Plan != "" {
		k.said += "\n" + strings.ToLower(c.Plan)
	}
	for _, r := range c.Roots {
		if r != "" {
			k.roots = append(k.roots, path.Clean(r))
		}
	}
	for _, p := range c.Recent {
		if !p.IsError && p.Subject != "" {
			k.done = append(k.done, p.Subject)
		}
	}
	_ = json.Unmarshal([]byte(c.Args), &k.args)
	return k
}

func (k *given) str(key string) string {
	s, _ := k.args[key].(string)
	return s
}

// named reports whether the person named the text: a path when they named
// it or a folder holding it, never a path that only begins the same way; any
// other name as a whole word, so prod is not named by production.
func (k *given) named(s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	if slices.Contains(k.declared, low) {
		return true
	}
	if strings.HasPrefix(low, "/") || strings.HasPrefix(low, "~") {
		p := path.Clean(low)
		for _, m := range userPaths.FindAllStringSubmatch(k.said, -1) {
			u := strings.TrimRight(m[1], ".,;:!?")
			if strings.HasPrefix(u, "//") {
				continue // a URL's host, not a path
			}
			if u = path.Clean(u); u != "/" && within(p, u) {
				return true
			}
		}
		return false
	}
	return mentions(k.said, low)
}

// observed reports whether an earlier observation or successful call showed
// the text, or the folder that will hold it.
func (k *given) observed(s string) bool {
	if s == "" {
		return false
	}
	dir := ""
	if strings.HasPrefix(s, "/") {
		if d := path.Dir(s); d != "/" && d != "." {
			dir = d + "/"
		}
	}
	for _, d := range k.done {
		if strings.Contains(d, s) || dir != "" && strings.Contains(d, dir) {
			return true
		}
	}
	if k.seen == nil {
		return false
	}
	if k.seen(s) {
		return true
	}
	if strings.HasPrefix(s, "/") {
		if dir := path.Dir(s); dir != "/" && dir != "." && k.seen(dir) {
			return true
		}
	}
	return false
}

// local reports whether a path is the session's own ground: under a root,
// in a temporary folder, or a device every program writes to.
func (k *given) local(p string) bool {
	p = path.Clean(p)
	for _, r := range k.roots {
		if p != r && within(p, r) {
			return true
		}
	}
	return scratch(p)
}

var tempDirs = func() []string {
	dirs := []string{"/tmp", "/var/tmp", "/private/tmp", "/private/var/folders", "/var/folders", "/dev/shm"}
	if t := os.TempDir(); t != "" {
		dirs = append(dirs, path.Clean(t))
	}
	return dirs
}()

func scratch(p string) bool {
	switch p {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/tty", "/dev/zero":
		return true
	}
	if strings.HasPrefix(p, "/dev/fd/") {
		return true
	}
	for _, t := range tempDirs {
		if within(p, t) {
			return true
		}
	}
	return false
}

// within reports whether p is dir or under it.
func within(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// userPaths finds the paths in what the person said: absolute ones, and a
// home folder's only with something after it (~/cache, ~bob/x), so "~ 10
// minutes" names nothing.
var userPaths = regexp.MustCompile(`(?:^|[\s"'(=:,])(/[^\s"'()<>|;&,` + "`" + `]*|~[A-Za-z0-9_.-]*/[^\s"'()<>|;&,/` + "`" + `][^\s"'()<>|;&,` + "`" + `]*)`)

// remit is the task as the person put it, short enough to quote back.
func remit(c Case) string {
	r := strings.Join(strings.Fields(c.Remit), " ")
	if rs := []rune(r); len(rs) > 160 {
		r = string(rs[:160]) + "…"
	}
	if r == "" {
		return "no task was given"
	}
	return fmt.Sprintf("the task is %q", r)
}

// line is the shell text a call runs, where it is shell.
func line(c Case, k *given) (string, bool) {
	switch c.Tool {
	case "bash", "ssh":
		s := k.str("command")
		return s, s != ""
	}
	return "", false
}

func remote(c Case) bool { return c.Tool == "ssh" || c.Tool == "ssh_put" }

// declaredRead is an ssh command the operator declared a read in
// ssh.read_commands: its program and what it touches are the operator's word.
func declaredRead(c Case) bool { return c.Tool == "ssh" && c.DeclaredRead != "" }

// drift denies destructive work whose target the remit never named and
// that is not the session's own ground.
func (r Rules) drift(k *given, c Case) *Verdict {
	if c.Tool == "k8s_apply" {
		if k.str("action") != "delete" {
			return nil
		}
		ns := k.str("namespace")
		if ns == "" || ns == "default" || k.named(ns) {
			return nil
		}
		return &Verdict{Decision: policy.Deny, Code: CodeOutOfRemit,
			Rationale: fmt.Sprintf("deleting in namespace %s, which the task never named; %s", ns, remit(c)),
			Advice:    "stay within the namespaces the task names, or ask the person before deleting anything else"}
	}
	src, ok := line(c, k)
	if !ok {
		return nil
	}
	start := ""
	if remote(c) {
		start = "~" // an ssh command starts in the login's home
	}
	s, ok := parseScriptAt(src, start)
	if !ok {
		// A line the parser refuses is one the check cannot read at all
		// (bash may still run it: `export HO\ME=/srv`). If it looks
		// destructive, the person is asked rather than the line let through.
		if looksDestructive(strings.Fields(src)) {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.6,
				Rationale: fmt.Sprintf("the command line could not be parsed, and it looks like it deletes or runs code; %s", remit(c)),
				Advice:    "write the command plainly so it can be checked, or confirm this is meant"}
		}
		return nil
	}
	where := "here"
	if remote(c) {
		where = "on " + k.str("host")
	}
	if s.homeWrites {
		k.home = "" // the line may write HOME: ~ is not placed against the session's home
	}
	for _, cmd := range s.cmds {
		if v := r.judgeCommand(k, c, cmd, where, 0); v != nil {
			return v
		}
	}
	// The fallback, which does not depend on finding where code is handed:
	// a line the check does not read completely that anywhere — a quoted
	// string, a here-document, an extra argument, an assignment value — names
	// a deleting program or a protected root is put to the person. The precise
	// rules above decide every line the check reads completely.
	if !r.lineFullyRead(s) && lineMentionsDanger(s) {
		return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.6,
			Rationale: fmt.Sprintf("the line runs something the check does not read in full, and it names a deleting program or a protected folder; %s", remit(c)),
			Advice:    "run the delete plainly, with the path in full, or confirm this is meant"}
	}
	return nil
}

// judgeCommand runs the out-of-remit rules over one command. The top level
// and code handed to a shell, eval or an interpreter both come through here;
// depth counts how far code was handed down.
func (r Rules) judgeCommand(k *given, c Case, cmd command, where string, depth int) *Verdict {
	// A command name the check cannot read plainly (\rm, r\m, a quoted or
	// expanded name) — the first word or the program past a wrapper — or a
	// wrapper with an option (sudo -u, env -C, command -p, nice -n …), may
	// hide the program or move the directory; its words are not read as a
	// command line. If they look destructive — a deleting program, an
	// interpreter, a protected root, an operand outside own ground — the
	// person is asked.
	if !plainHead(cmd) || !cmd.plainProgram() || optioned(wrapperPrefix(cmd.words)) {
		if looksDestructive(cmd.words) || r.strayOperand(k, c, cmd.words) {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
				Rationale: fmt.Sprintf("a command whose program or wrapper options the check does not read looks like it deletes or runs code; %s", remit(c)),
				Advice:    "run the delete plainly, with no wrapper options and the path in full, or confirm this is meant"}
		}
		return nil
	}
	prog, args := cmd.program()
	prog = path.Base(prog) // /bin/rm is rm
	// An argument the check cannot read (an expansion, a substitution, a glob)
	// may be the program an unknown runner runs, or a target; for a program the
	// rules do not judge precisely, it is asked about when its words look
	// destructive or an operand lies outside own ground.
	if !cmd.argsLiteral && !preciseProg(prog) && !containerProg(prog) {
		if looksDestructive(args) || r.strayOperand(k, c, args) {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
				Rationale: fmt.Sprintf("%s is given arguments only running the line would give, and they look like they delete or reach outside the task's ground; %s", prog, remit(c)),
				Advice:    "write the command and its paths plainly, or confirm this is meant"}
		}
	}
	// Each command find runs by -exec, -execdir, -ok or -okdir is judged as a
	// command of its own, with the directory unknown, whatever find's start.
	if prog == "find" {
		for _, words := range findExecCommands(args) {
			sub := command{words: words, argsLiteral: true, literal: true}
			if r.judgeCommand(k, c, sub, where, depth+1) != nil {
				return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
					Rationale: fmt.Sprintf("find runs a command by -exec that looks like it deletes or reaches outside the task's ground; %s", remit(c)),
					Advice:    "delete the paths the task names directly, or confirm this is meant"}
			}
		}
	}
	if v := r.moveVerdict(k, c, cmd, prog, args, where); v != nil {
		return v
	}
	if prog == "find" && findExecHidden(args) {
		return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
			Rationale: fmt.Sprintf("find runs a program by -exec that the check cannot read plainly; %s", remit(c)),
			Advice:    "name the program find runs in full, or confirm this is meant"}
	}
	// A shell, interpreter or xargs that find runs by -exec can delete what
	// it is handed; where find looks outside own ground, that is asked.
	if prog == "find" && findExecRunsCode(args) && !r.findStartsOwnGround(k, c, args) {
		return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
			Rationale: fmt.Sprintf("find runs a shell or interpreter by -exec over paths outside the task's ground; %s", remit(c)),
			Advice:    "delete the paths the task names directly, or confirm this is meant"}
	}
	for _, t := range destroys(prog, args) {
		if v := r.judgeTarget(k, c, cmd, prog, t, where); v != nil {
			return v
		}
	}
	// An interpreter one-liner's literal-path deletes were judged above by
	// destroys; here its shell-outs and the deleter names it spells.
	if code := oneLiner(prog, args); code != "" {
		if deleterWord.MatchString(code) || shellsOut.MatchString(code) || mentionsProtectedRoot(code) || psDelete.MatchString(code) {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
				Rationale: fmt.Sprintf("%s runs code the check does not read, which names a deleter, a protected folder or a shell-out; %s", prog, remit(c)),
				Advice:    "run the command plainly so it can be checked, or confirm this is meant"}
		}
	}
	// Code the command hands to a shell, to eval, or on the stdin of a shell
	// or interpreter is parsed and run through these rules with the
	// directory unknown; any non-allow there is an ask here, since the check
	// cannot be sure where it runs.
	for _, code := range handedShellCode(prog, args, cmd.stdin) {
		if r.codeVerdict(k, c, code, where, depth+1) != nil {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
				Rationale: fmt.Sprintf("%s runs code the check reads only in part, which looks like it deletes or shells out; %s", prog, remit(c)),
				Advice:    "run the command plainly so it can be checked, or confirm this is meant"}
		}
	}
	// Code the check cannot read at all — eval of an expansion, a shell's
	// -c given an expansion, a script that is a process substitution — is
	// asked about always: its text is not in the line to judge.
	if runsUnreadCode(cmd, prog, args) {
		return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
			Rationale: fmt.Sprintf("%s runs code that is built when the line runs, so the check cannot read it; %s", prog, remit(c)),
			Advice:    "run the command plainly so it can be checked, or confirm this is meant"}
	}
	if ns := kubectlDeleteNS(prog, args); ns != "" && ns != "default" && !k.named(ns) {
		return &Verdict{Decision: policy.Deny, Code: CodeOutOfRemit,
			Rationale: fmt.Sprintf("kubectl would delete in namespace %s, which the task never named; %s", ns, remit(c)),
			Advice:    "stay within the namespaces the task names, or ask the person before deleting anything else"}
	}
	// The fallback for one command: read in part, and naming a deleter or a
	// protected root, it is asked about (the line-level fallback covers text
	// spread over several commands).
	if !r.cmdFullyRead(cmd) && cmdMentionsDanger(cmd) {
		return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.6,
			Rationale: fmt.Sprintf("%s runs something the check does not read in full, and it names a deleting program or a protected folder; %s", prog, remit(c)),
			Advice:    "run the delete plainly, with the path in full, or confirm this is meant"}
	}
	return nil
}

// judgeTarget judges one target a command would destroy: a $ target it cannot
// read, a target under an unknown directory, a home path it cannot place, a
// protected root, or a path outside own ground. nil means own ground.
func (r Rules) judgeTarget(k *given, c Case, cmd command, prog, t, where string) *Verdict {
	if t == "" {
		return nil
	}
	if strings.HasPrefix(t, "$") && !homeish(t) {
		// A target only running the line would give. It is read only when
		// it is a variable set to a plain literal earlier in the same plain
		// chain, and then judged as that path; otherwise it is asked about.
		v, ok := literalVar(cmd, t)
		if !ok {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
				Rationale: fmt.Sprintf("%s would destroy a target only running the line would give; %s", prog, remit(c)),
				Advice:    "name the path to delete in full, somewhere the task covers, or confirm you mean this one"}
		}
		t = v
	}
	eff, known := effectiveTarget(cmd, t)
	// Where the directory is unknown, only what the person said in their
	// own messages names a relative target; the session's home still
	// places ~, since ~ depends on HOME, not the directory.
	p := *k
	if !cmd.dirKnown {
		p.said = k.person
	}
	kk := &p
	// A relative target under a directory the check does not claim to
	// know could point anywhere: it is always put to the person, never
	// allowed on a name alone and never a silent deny.
	if !known {
		return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
			Rationale: fmt.Sprintf("%s would destroy %s, but the directory it runs in is not one the check can be sure of, so where it points is not known; %s", prog, t, remit(c)),
			Advice:    "name the path to delete in full, or change into it with a plain cd joined by && first"}
	}
	// A ~ or $HOME target the check cannot place — because the directory
	// is unknown, or because the line may write HOME — is put to the
	// person rather than placed or silently denied, unless they named
	// it as a path outright.
	if homeish(eff) && !remote(c) && k.homeAvailable && kk.home == "" && !protectedRoot(eff) && !kk.named(homeTilde(eff)) {
		return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
			Rationale: fmt.Sprintf("%s would destroy %s, but HOME may have been changed in the line, so where ~ points is not known; %s", prog, t, remit(c)),
			Advice:    "name the path to delete in full, or confirm this is meant"}
	}
	// find's operand is where it looks, not what it deletes.
	if prog != "find" && protectedRoot(eff) {
		// Named outright as the thing to delete: ask, never deny, so a
		// person can still say yes; not named: deny.
		if kk.named(eff) {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.9,
				Rationale: fmt.Sprintf("%s would destroy %s %s whole; the task names it, so this is put to you", prog, display(eff, t), where),
				Advice:    "confirm you mean to delete all of it, or name the folders inside it instead"}
		}
		return &Verdict{Decision: policy.Deny, Code: CodeOutOfRemit,
			Rationale: fmt.Sprintf("%s would destroy %s %s whole, which no task is taken to mean; %s", prog, display(eff, t), where, remit(c)),
			Advice:    "delete only the folders inside it that the task names, or ask the person first"}
	}
	if r.ownGround(kk, c, eff) {
		return nil
	}
	return &Verdict{Decision: policy.Deny, Code: CodeOutOfRemit,
		Rationale: fmt.Sprintf("%s would destroy %s %s, outside what the task names; %s", prog, display(eff, t), where, remit(c)),
		Advice:    "keep destructive commands to the paths the task names, or ask the person first"}
}

// ownGround reports whether a destructive target, already resolved against the
// command's working directory, is within the remit.
func (Rules) ownGround(k *given, c Case, t string) bool {
	if homeish(t) {
		if k.named(homeTilde(t)) {
			return true
		}
		// Locally ~ is the session's home: placed there, a target under the
		// workspace (a workspace under ~) is the workspace's own ground.
		if !remote(c) && k.home != "" {
			if p, ok := expandHome(t, k.home); ok && k.local(p) {
				return true
			}
		}
		// Over ssh the home is the login's own, so a folder under it whose top
		// name the task gave as a bare word — not inside a path, not the host
		// it names — is the folder the task meant.
		return remote(c) && bareMentions(k.said, homeTop(t), strings.ToLower(k.str("host")))
	}
	if !strings.HasPrefix(t, "/") {
		clean := path.Clean(t)
		return clean != ".." && !strings.HasPrefix(clean, "../")
	}
	if k.named(t) {
		return true
	}
	if remote(c) {
		return scratch(path.Clean(t))
	}
	return k.local(t)
}

// effectiveTarget resolves a destructive target against the directory its
// command runs in. An absolute or home target stands as written; a relative
// one is joined onto the working directory, and a whole-directory shape
// (*, ., ./*, ..) resolves to the directory itself (or its parent). ok is
// false when the working directory could not be resolved.
func effectiveTarget(cmd command, t string) (string, bool) {
	if strings.HasPrefix(t, "/") || homeish(t) {
		return t, true
	}
	if !cmd.dirKnown {
		return t, false
	}
	base := cmd.dir
	homeBase := strings.HasPrefix(base, "~")
	rel := strings.TrimSuffix(strings.TrimPrefix(t, "./"), "/")
	whole := rel == "" || rel == "." || rel == "*"
	rel = strings.TrimSuffix(rel, "/*")
	switch {
	case whole:
		if homeBase {
			return "~", true // the home directory itself
		}
		return orDot(base), true
	case rel == "..":
		if homeBase {
			return "~", false // the parent of the login's home: not known, so ask
		}
		return orDot(path.Join(base, "..")), true
	default:
		j := joinDir(base, rel)
		if homeBase && !strings.HasPrefix(j, "~") {
			return j, false // a relative target that climbs out of the home: ask
		}
		return j, true
	}
}

// destroyerNames are programs that delete or overwrite; their name among a
// wrapped command's words makes it look destructive.
var destroyerNames = map[string]bool{"rm": true, "rmdir": true, "shred": true, "unlink": true, "srm": true,
	"truncate": true, "dd": true, "mkfs": true, "wipefs": true, "fdisk": true, "sfdisk": true, "parted": true, "find": true}

// looksDestructive reports words that name a deleting program, or a protected root.
func looksDestructive(words []string) bool {
	for _, w := range words {
		name := path.Base(strings.ReplaceAll(w, `\`, "")) // \rm, r\m and /bin/rm are rm
		if destroyerNames[name] || strings.HasPrefix(name, "mkfs.") || codeRunners[name] ||
			strings.HasPrefix(name, "python") {
			return true
		}
		if protectedRoot(w) {
			return true
		}
	}
	return false
}

// codeRunners are interpreters and shells, which run code the check does not
// read; one among a hidden command's words makes it look destructive.
var codeRunners = map[string]bool{"node": true, "perl": true, "ruby": true, "php": true, "bash": true, "sh": true,
	"zsh": true, "dash": true, "ksh": true, "fish": true, "awk": true, "gawk": true, "busybox": true, "xargs": true,
	"lua": true, "Rscript": true, "tclsh": true, "osascript": true, "mawk": true, "nawk": true, "julia": true,
	"bun": true, "deno": true, "pwsh": true, "powershell": true}

// strayOperand reports, for a command the check does not read as a command
// line, an operand that climbs out of or lies outside own ground: an absolute
// or ~ path not own ground, or a relative path that climbs above where it starts.
func (r Rules) strayOperand(k *given, c Case, words []string) bool {
	for _, w := range operands(words) {
		w = strings.ReplaceAll(w, `\`, "")
		switch {
		case strings.HasPrefix(w, "/") || homeish(w):
			if !r.ownGround(k, c, w) {
				return true
			}
		case !strings.HasPrefix(w, "$"):
			if clean := path.Clean(w); clean == ".." || strings.HasPrefix(clean, "../") {
				return true
			}
		}
	}
	return false
}

// plainHead reports a command whose name is a single plain literal.
func plainHead(cmd command) bool {
	if cmd.call == nil || len(cmd.call.Args) == 0 {
		return true // a redirection or assignment only
	}
	_, ok := plainWord(cmd.call.Args[0])
	return ok
}

// literalVar is the plain literal a $X or ${X} target was set to earlier in
// the same plain chain, where the directory is known.
func literalVar(cmd command, t string) (string, bool) {
	if !cmd.dirKnown || cmd.vars == nil {
		return "", false
	}
	name := strings.TrimPrefix(t, "$")
	if strings.HasPrefix(name, "{") && strings.HasSuffix(name, "}") {
		name = name[1 : len(name)-1]
	}
	for _, r := range name {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return "", false // not a single plain variable reference
		}
	}
	v, ok := cmd.vars[name]
	return v, ok
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}

// homeTop is the first path segment under a home directory: ~/releases/old → releases.
func homeTop(t string) string {
	rest := strings.TrimPrefix(homeTilde(t), "~")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[i+1:]
	}
	seg, _, _ := strings.Cut(rest, "/")
	return seg
}

// display names the resolved target, and adds the text as written when a cd moved it.
func display(eff, written string) string {
	if eff == written || eff == path.Clean(strings.TrimPrefix(written, "./")) {
		return eff
	}
	return eff + " (" + written + ")"
}

func homeish(t string) bool {
	return strings.HasPrefix(t, "~") || strings.HasPrefix(t, "$HOME") || strings.HasPrefix(t, "${HOME}")
}

// expandHome places a ~, ~/x, $HOME or ${HOME} target under home. A ~user
// target is not expanded.
func expandHome(t, home string) (string, bool) {
	t = homeTilde(t)
	if t == "~" {
		return path.Clean(home), true
	}
	if rest, ok := strings.CutPrefix(t, "~/"); ok {
		return path.Clean(path.Join(home, rest)), true
	}
	return "", false
}

// homeTilde writes $HOME and ${HOME} as ~, the way the person would name it.
func homeTilde(t string) string {
	for _, p := range []string{"${HOME}", "$HOME"} {
		if rest, ok := strings.CutPrefix(t, p); ok {
			return "~" + rest
		}
	}
	return t
}

// protectedRoot is a target no mention makes part of the remit: the root, a
// top-level folder (/etc, /usr, /srv, …), a home folder itself (~, ~bob,
// $HOME, /home/bob, /Users/bob, /root). Deleting one of these whole is
// always out of remit; the person names a folder inside it instead.
func protectedRoot(t string) bool {
	t = homeTilde(strings.TrimSpace(t))
	if rest, ok := strings.CutPrefix(t, "~"); ok {
		_, after, _ := strings.Cut(rest, "/")
		return path.Clean("/"+after) == "/"
	}
	if !strings.HasPrefix(t, "/") {
		return false
	}
	p := path.Clean(t)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch {
	case p == "/" || len(parts) == 1:
		return true
	case len(parts) == 2 && (parts[0] == "home" || parts[0] == "Users"):
		return true
	}
	return false
}

// destroys names what a command deletes or overwrites beyond recovery.
func destroys(prog string, args []string) []string {
	switch prog {
	case "rm", "rmdir", "shred", "unlink", "srm":
		return operands(args)
	case "truncate":
		var out []string
		for i := 0; i < len(args); i++ {
			if args[i] == "-s" || args[i] == "--size" || args[i] == "-r" {
				i++
				continue
			}
			if !strings.HasPrefix(args[i], "-") {
				out = append(out, args[i])
			}
		}
		return out
	case "dd":
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				return []string{v}
			}
		}
	case "find":
		if !hasFlag(args, "-delete") && !execsRemove(args) {
			return nil
		}
		out := findStarts(args)
		if len(out) == 0 {
			out = []string{"."}
		}
		return out
	case "mkfs", "wipefs", "fdisk", "sfdisk", "parted":
		return operands(args)
	}
	if strings.HasPrefix(prog, "mkfs.") {
		return operands(args)
	}
	if code := oneLiner(prog, args); code != "" {
		var out []string
		for _, m := range codeDeletes.FindAllStringSubmatch(code, -1) {
			for _, g := range m[1:] {
				if g != "" {
					out = append(out, g)
				}
			}
		}
		return out
	}
	return nil
}

// psDelete names PowerShell's delete cmdlets, whose paths are often unquoted.
var psDelete = regexp.MustCompile(`(?i)\b(?:Remove-Item|Remove-ItemProperty|Clear-Content)\b`)

// deleterWord names a deleting program as a whole word, wherever it sits in
// a string of code — `system "rm -rf /etc"`, `os.execvp("rm", …)`.
var deleterWord = regexp.MustCompile(`(?i)(?:^|[^-\w])(?:rm|rmdir|shred|unlink|srm|dd|mkfs|wipefs|fdisk|sfdisk|parted|truncate)\b`)

// shellsOut finds code that runs a command through a library call: a shell-out
// or a system call the plain-word scan would miss — Python's os.system and
// subprocess, Node's child_process, Lua's os.execute and io.popen, a system
// call in Perl or Ruby (system "…" or system(…)), AppleScript's do shell
// script, Tcl's file delete.
var shellsOut = regexp.MustCompile(`(?i)\bdo\s+shell\s+script\b|\bsubprocess\b|\bchild_process\b|` +
	`\bos\.(?:system|popen|execv?p?e?|spawn[a-z]*|execute)\s*\(|\bio\.popen\s*\(|\bexecSync\s*\(|\bspawnSync\s*\(|` +
	`\bsystem\s*[("']|\bfile\s+delete\b`)

// codeVerdict judges code handed to a shell, to eval, or on an interpreter's
// stdin: it scans the text for a deleter, a shell-out, a delete call or a
// protected root, then parses it and runs judgeCommand over every command
// with the directory unknown, and the fallback over the whole; any non-allow
// is an ask. Code handed deeper than maxHandDepth is asked about.
func (r Rules) codeVerdict(k *given, c Case, code, where string, depth int) *Verdict {
	ask := &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
		Rationale: fmt.Sprintf("code handed to a shell, eval or an interpreter looks like it deletes or shells out; %s", remit(c)),
		Advice:    "run the command plainly so it can be checked, or confirm this is meant"}
	if deleterWord.MatchString(code) || shellsOut.MatchString(code) ||
		codeDeletes.MatchString(code) || mentionsProtectedRoot(code) {
		return ask
	}
	if depth > maxHandDepth {
		return ask // a bound reached fails closed
	}
	s, ok := parseScript(code)
	if !ok {
		return nil // unparseable handed code: the text scans above are the check
	}
	for i := range s.cmds {
		s.cmds[i].dirKnown, s.cmds[i].vars = false, nil
	}
	for _, cmd := range s.cmds {
		if r.judgeCommand(k, c, cmd, where, depth) != nil {
			return ask
		}
	}
	if !r.lineFullyRead(s) && lineMentionsDanger(s) {
		return ask
	}
	return nil
}

// maxHandDepth bounds how deep handed code is followed; deeper code is asked about.
const maxHandDepth = 6

// mentionsProtectedRoot reports text that names a protected root as a path,
// leaving out the temporary folders, which are the session's own ground.
func mentionsProtectedRoot(text string) bool {
	for _, m := range userPaths.FindAllStringSubmatch(text, -1) {
		p := strings.TrimRight(strings.Trim(m[1], `"'`), ".,;:)")
		if protectedRoot(p) && !scratch(path.Clean(homeTilde(p))) {
			return true
		}
	}
	return false
}

// progBase is a program's base name with quotes and escapes removed, so
// /bin/rm, m and 'rm' all read as rm.
func progBase(prog string) string {
	return path.Base(strings.ReplaceAll(strings.Trim(prog, `"'`), `\`, ""))
}

// lineMentionsDanger reports a line that names a deleting program as a word or
// a protected root anywhere the person's text reaches: a parsed word, an
// assignment value, a here-document body. It reads the parsed words, not the
// raw source, so a comment or a script's file name does not count.
func lineMentionsDanger(s script) bool {
	for _, cmd := range s.cmds {
		if cmdMentionsDanger(cmd) {
			return true
		}
	}
	return false
}

// commandText is every piece of a command's text the person wrote: its words,
// its assignment values, and anything on its stdin.
func commandText(cmd command) []string {
	out := append([]string(nil), dangerWords(cmd)...)
	out = append(out, cmd.stdin...)
	out = append(out, cmd.assigns...)
	return out
}

// dangerWords are a command's words with the ones that only carry a human
// message or an edit expression dropped, so a deleter word in prose (a commit
// or PR message, a sed script) is not read as a command.
func dangerWords(cmd command) []string {
	prog, args := cmd.program()
	base := progBase(prog)
	skipVal := map[string]bool{"-m": true, "--message": true, "-b": true, "--body": true, "--title": true, "-t": true}
	sed := base == "sed" || base == "gsed"
	head := append([]string(nil), cmd.words[:len(cmd.words)-len(args)]...)
	var out []string
	sawScript := false
	prose := func(v string) bool { return strings.ContainsAny(v, " \t\n") }
	for i := 0; i < len(args); i++ {
		a := args[i]
		if skipVal[a] && i+1 < len(args) && prose(args[i+1]) {
			i++ // drop a message value that is prose, not a command
			continue
		}
		if k, v, ok := strings.Cut(a, "="); ok && skipVal[k] && prose(v) {
			continue
		}
		if sed && !strings.HasPrefix(a, "-") && !sawScript {
			sawScript = true // the sed script is an expression, not a command
			continue
		}
		out = append(out, a)
	}
	return append(head, out...)
}

// cmdMentionsDanger reports a command whose text names a deleter or (outside a
// container command, where one-segment paths are container paths) a protected
// root.
func cmdMentionsDanger(cmd command) bool {
	prog, _ := cmd.program()
	container := containerProg(progBase(prog))
	for _, w := range commandText(cmd) {
		if deleterWord.MatchString(w) {
			return true
		}
		if !container && mentionsProtectedRoot(w) {
			return true
		}
	}
	return false
}

// lineFullyRead reports a line the check reads completely, so the precise
// rules decide it and the fallback need not. It is an allowlist, not a
// denylist of known handlers: a command is read completely only when it
// neither hands nor runs code the rules did not see, and its program is one
// the rules judge or carries no deleter, shell or interpreter among its words.
// Any other command — an unknown program that may run its arguments, a shell
// given code, a here-document — leaves the line to the fallback.
func (r Rules) lineFullyRead(s script) bool {
	for _, cmd := range s.cmds {
		if !r.cmdFullyRead(cmd) {
			return false
		}
	}
	return true
}

// cmdFullyRead reports one command read completely by the precise rules.
func (r Rules) cmdFullyRead(cmd command) bool {
	if !plainHead(cmd) || !cmd.plainProgram() || optioned(wrapperPrefix(cmd.words)) {
		return false
	}
	if len(cmd.stdin) > 0 || readsStdinFile(cmd) || cmd.readsPipe {
		return false // its stdin is text or piped code the line does not fully show
	}
	prog, args := cmd.program()
	base := progBase(prog)
	if len(handsOffCode(base, args, cmd.stdin)) > 0 || runsUnreadCode(cmd, base, args) {
		return false // it hands or runs code
	}
	if envRunsCode(cmd.env) {
		return false // an assignment names a deleter/shell or sets git's config/work-tree
	}
	switch {
	case base == "git":
		return gitFullyRead(args)
	case base == "kubectl":
		return !kubectlRunsCommand(args)
	case printerProg[base]:
		return true // only prints or searches its arguments
	case destroyerNames[base] || base == "find" || strings.HasPrefix(base, "mkfs.") ||
		base == "mv" || base == "rsync":
		return true // the precise rules judge it
	}
	// An unknown program may run its arguments as a command. It is read
	// completely only when no word of it names a deleter, shell or interpreter.
	for _, w := range args {
		if wordRunsCode(w) {
			return false
		}
	}
	return true
}

// gitFullyRead reports a git command whose arguments the rules read: no
// -c/--config/-C alias injection, and no word that names a deleter, shell or
// interpreter past a precise git subcommand (rm, mv, clean).
func gitFullyRead(args []string) bool {
	if hasFlag(args, "-c") || hasFlag(args, "--config") || hasFlag(args, "-C") {
		return false
	}
	for i, w := range operands(args) {
		if i == 0 && (w == "rm" || w == "mv" || w == "clean") {
			continue // a git subcommand the rules judge
		}
		if wordRunsCode(w) {
			return false
		}
	}
	return true
}

// kubectlRunsCommand reports a kubectl subcommand that runs a command in a
// container (exec, run, debug, attach, cp), whose command the rules do not read.
func kubectlRunsCommand(args []string) bool {
	ops := operands(args)
	if len(ops) == 0 {
		return false
	}
	switch ops[0] { // the subcommand is the first operand
	case "exec", "run", "debug", "attach", "cp":
		return true
	}
	return false
}

// envRunsCode reports an assignment before a command that could run code: a
// value naming a deleter, shell or interpreter, or a git variable that injects
// config, an alias or a work tree.
func envRunsCode(env map[string]string) bool {
	for name, v := range env {
		if strings.HasPrefix(name, "GIT_CONFIG") || name == "GIT_WORK_TREE" ||
			name == "GIT_DIR" || name == "GIT_SSH_COMMAND" || name == "GIT_EXTERNAL_DIFF" {
			return true
		}
		if wordRunsCode(v) {
			return true
		}
	}
	return false
}

// readsStdinFile reports a command given /dev/stdin or - to read.
func readsStdinFile(cmd command) bool {
	for _, w := range cmd.words {
		if w == "/dev/stdin" || w == "/proc/self/fd/0" {
			return true
		}
	}
	for _, r := range cmd.reads {
		if r == "/dev/stdin" || r == "-" || strings.Contains(r, "<(") {
			return true
		}
	}
	return false
}

// wordRunsCode reports an argument that could make an unknown program run a
// deleter: a word that is exactly a deleter, shell or interpreter name, or a
// multi-token string (a quoted command) that names a deleter or protected root.
func wordRunsCode(w string) bool {
	u := strings.ReplaceAll(w, `\`, "")
	if strings.ContainsAny(u, " \t\n") {
		return deleterWord.MatchString(u) || shellsOut.MatchString(u) ||
			codeDeletes.MatchString(u) || mentionsProtectedRoot(u)
	}
	name := progBase(u)
	return destroyerNames[name] || shellNames[name] || codeRunners[name] || strings.HasPrefix(name, "python")
}

// printerProg only prints or searches its arguments; it does not run them.
var printerProg = map[string]bool{"echo": true, "printf": true, "grep": true, "egrep": true, "fgrep": true,
	"which": true, "type": true, "cat": true, "head": true, "tail": true, "wc": true, "ls": true}

// containerProg runs a command inside a container, where absolute paths are
// container paths, not host paths, so a one-segment root there is not protected.
func containerProg(base string) bool {
	switch base {
	case "docker", "podman", "nerdctl", "kubectl", "ctr", "lima", "colima":
		return true
	}
	return false
}

// preciseProg reports a program whose destructive forms the rules parse, so a
// non-literal argument need not trigger the stray-operand sweep.
func preciseProg(base string) bool {
	return destroyerNames[base] || base == "find" || strings.HasPrefix(base, "mkfs.") ||
		base == "mv" || base == "rsync" || base == "git" || base == "kubectl"
}

// runsUnreadCode reports a command whose code the check cannot read at all,
// because it is built when the line runs: eval of an expansion, a shell's -c
// given an expansion, or a shell, source or . reading a process substitution.
func runsUnreadCode(cmd command, prog string, args []string) bool {
	// A shell or interpreter reading its script from a process substitution,
	// whether as the operand or redirected onto stdin (bash < <(…)), runs
	// code the line does not show.
	if (shellNames[prog] || readsStdinCode(prog) || prog == "source" || prog == ".") && readsProcSubst(cmd, args) {
		return true
	}
	switch {
	case prog == "eval":
		return !cmd.literal
	case shellNames[prog]:
		if code, hasC := shellDashC(args); hasC && strings.HasPrefix(code, "$") {
			return true // -c given $(…) or $X
		}
	case codeRunners[prog] || strings.HasPrefix(prog, "python"):
		// An interpreter whose code is an expansion is unread, as -c "$X" is.
		return strings.HasPrefix(oneLiner(prog, args), "$")
	}
	return false
}

// readsProcSubst reports a process substitution as the first operand, or
// redirected onto stdin.
func readsProcSubst(cmd command, args []string) bool {
	if firstOperandIsProcSubst(args) {
		return true
	}
	for _, rd := range cmd.reads {
		if strings.Contains(rd, "<(") {
			return true
		}
	}
	return false
}

// firstOperandIsProcSubst reports a first operand that is a process
// substitution, <(…), whose content is code the line does not show.
func firstOperandIsProcSubst(args []string) bool {
	for _, a := range operands(args) {
		return strings.Contains(a, "<(")
	}
	return false
}

// handsOffCode gathers every string a command hands to another to run as code:
// an interpreter's one-liner, a shell's -c argument, eval's operands, and the
// text on the stdin of a shell, an interpreter, or source/. reading stdin.
// cmdFullyRead uses it only to tell whether a command hands off code at all.
func handsOffCode(prog string, args, stdin []string) []string {
	out := handedShellCode(prog, args, stdin)
	if code := oneLiner(prog, args); code != "" {
		out = append(out, code)
	}
	return out
}

// handedShellCode is the code a command hands to a shell or to eval, and the
// text on the stdin of a shell or interpreter — the strings parsed and fully
// judged. It leaves out an interpreter's one-liner, whose literal-path deletes
// destroys judges by path.
func handedShellCode(prog string, args, stdin []string) []string {
	var out []string
	if shellNames[prog] {
		if code, _ := shellDashC(args); code != "" {
			out = append(out, code)
		}
	}
	if prog == "eval" {
		out = append(out, strings.Join(operands(args), " "))
	}
	if readsStdinCode(prog) || (prog == "source" || prog == ".") && readsStdinPath(args) {
		out = append(out, stdin...)
	}
	return out
}

// readsStdinPath reports a source/. whose script is standard input.
func readsStdinPath(args []string) bool {
	for _, a := range operands(args) {
		return a == "/dev/stdin" || a == "-"
	}
	return false
}

// shellDashC is the code a shell runs after -c, and whether -c was given. The
// code is the first operand once the options end: every word starting with -
// or + is an option (a -- ends them; -o, -O, +o, +O take a value), and the
// code is used when any option cluster holds c (-c, -lc, -ec, -c --, -c -e).
func shellDashC(args []string) (string, bool) {
	sawC := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if sawC && i+1 < len(args) {
				return args[i+1], true
			}
			return "", sawC
		case a == "-o" || a == "-O" || a == "+o" || a == "+O":
			i++ // takes an option name
		case a == "--rcfile" || a == "--init-file":
			i++ // takes a file
		case strings.HasPrefix(a, "--"):
			// another long option
		case (strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+")) && len(a) > 1:
			if strings.HasPrefix(a, "-") && strings.Contains(a[1:], "c") {
				sawC = true
			}
		default:
			if sawC {
				return a, true // the first operand is the code
			}
			return "", false
		}
	}
	return "", sawC
}

// readsStdinCode reports a program that runs its standard input as code: a
// shell, or an interpreter in a bare form (python3, node) with no script.
func readsStdinCode(prog string) bool {
	return shellNames[prog] || codeRunners[prog] || strings.HasPrefix(prog, "python")
}

// shellNames are the shells among codeRunners, which run their argument or
// stdin as commands.
var shellNames = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"busybox": true, "ash": true, "mksh": true, "yash": true, "posh": true, "tcsh": true, "csh": true, "su": true}

// awkNames are the awk family, whose first operand is its program.
var awkNames = map[string]bool{"awk": true, "gawk": true, "mawk": true, "nawk": true}

// oneLiner is the code an interpreter is given on its command line. A short
// option cluster that ends in the code flag counts (python3 -Bc, perl -le,
// -ne, -pe, -E), as do node's -p, --print and --eval[=], and awk's program
// operand or -e/--source value.
func oneLiner(prog string, args []string) string {
	if awkNames[prog] {
		return awkProgram(args)
	}
	if prog == "deno" { // deno eval '<code>' / deno run takes a file, not code
		ops := operands(args)
		if len(ops) >= 2 && ops[0] == "eval" {
			return ops[1]
		}
		return ""
	}
	var flags string // the letters that introduce code
	switch {
	case strings.HasPrefix(prog, "python"):
		flags = "c"
	case prog == "pwsh", prog == "powershell":
		flags = "c" // -c / -Command
	case prog == "bun":
		flags = "e"
	case prog == "perl":
		flags = "eE"
	case prog == "node":
		flags = "ep"
	case prog == "ruby", prog == "lua", prog == "Rscript", prog == "osascript":
		flags = "e"
	case prog == "php":
		flags = "r"
	default:
		return ""
	}
	for i, a := range args {
		if prog == "node" {
			switch {
			case a == "--eval" || a == "--print":
				if i+1 < len(args) {
					return args[i+1]
				}
			case strings.HasPrefix(a, "--eval="):
				return strings.TrimPrefix(a, "--eval=")
			case strings.HasPrefix(a, "--print="):
				return strings.TrimPrefix(a, "--print=")
			}
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 1 &&
			strings.ContainsRune(flags, rune(a[len(a)-1])) && i+1 < len(args) {
			return args[i+1] // a cluster ending in the code flag
		}
	}
	return ""
}

// awkProgram is awk's program text: its -e/--source value, or the first
// operand that is not an option. A program read from a file (-f) is not shown.
func awkProgram(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--file":
			return ""
		case a == "-e" || a == "--source":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case a == "-v" || a == "-F":
			i++ // takes a value
		case strings.HasPrefix(a, "-"):
			// another option
		default:
			return a
		}
	}
	return ""
}

// codeDeletes finds the well-known calls that delete or overwrite a file,
// with the literal path they are given, in one line of Python, Node, Perl,
// Ruby or PHP. A textual scan: it finds the common forms, not every one.
var codeDeletes = regexp.MustCompile(`(?:\b(?:rmtree|remove|unlink|rmdir|removedirs|rmSync|removeSync|unlinkSync|rmdirSync|remove_dir_all|rm_rf|rm_r|rm_f|delete|truncate|writeFileSync|write_text|write_bytes)\s*\(?\s*['"]([^'"]+)['"]` +
	`|\bPath\(\s*['"]([^'"]+)['"]\s*\)\s*\.\s*(?:unlink|rmdir|write_text|write_bytes)\b` +
	`|\bopen\(\s*['"]([^'"]+)['"]\s*,\s*['"][wa])`)

func execsRemove(args []string) bool {
	for _, p := range findExecProgs(args) {
		switch p {
		case "rm", "shred", "unlink", "srm":
			return true
		}
	}
	return false
}

// findExecProgs are the programs find runs by -exec, -execdir, -ok and
// -okdir, each resolved as any other program is: past wrappers (sudo rm, env
// rm), base-named and unescaped (/bin/rm, \rm are rm).
func findExecProgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if !isExecFlag(args[i]) {
			continue
		}
		var words []string
		for j := i + 1; j < len(args) && args[j] != ";" && args[j] != "+" && args[j] != `\;`; j++ {
			words = append(words, args[j])
		}
		if p, _ := (command{words: words}).program(); p != "" {
			out = append(out, path.Base(strings.ReplaceAll(p, `\`, "")))
		}
	}
	return out
}

// findExecRunsCode reports a find whose -exec program is a shell, an
// interpreter or xargs, any of which can run a deleting command it is handed.
func findExecRunsCode(args []string) bool {
	for _, p := range findExecProgs(args) {
		if codeRunners[p] || strings.HasPrefix(p, "python") {
			return true
		}
	}
	return false
}

// findStarts are the paths a find command searches: the words after its
// leading options (-H, -L, -P, -D <arg>, -O<n>) and before the first test or
// action. find looks in . when none is given.
func findStarts(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-H" || a == "-L" || a == "-P":
			continue
		case a == "-D":
			i++ // -D takes a debug option as its own word
			continue
		case strings.HasPrefix(a, "-O"):
			continue // -O<n>, an optimisation level
		case strings.HasPrefix(a, "-") || a == "(" || a == "!":
			return out // the first test or action: start paths end here
		}
		out = append(out, a)
	}
	return out
}

// findStartsOwnGround reports whether every path find starts from is own ground.
func (r Rules) findStartsOwnGround(k *given, c Case, args []string) bool {
	starts := findStarts(args)
	if len(starts) == 0 {
		return r.ownGround(k, c, ".")
	}
	for _, a := range starts {
		if !r.ownGround(k, c, a) {
			return false
		}
	}
	return true
}

func isExecFlag(a string) bool {
	return a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir"
}

// findExecCommands is each command find runs by -exec/-execdir/-ok/-okdir: the
// words up to the terminating ; or +, with the {} placeholder dropped.
func findExecCommands(args []string) [][]string {
	var out [][]string
	for i := 0; i < len(args); i++ {
		if !isExecFlag(args[i]) {
			continue
		}
		var words []string
		for j := i + 1; j < len(args) && args[j] != ";" && args[j] != "+" && args[j] != `\;`; j++ {
			if args[j] == "{}" {
				continue
			}
			words = append(words, args[j])
		}
		if len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// moveVerdict judges mv, rsync and git clean, which remove data without an rm.
// A mv source outside own ground is an ask (the data survives at the
// destination) unless the destination is also outside own ground or /dev/null;
// rsync --delete / --remove-source-files and git clean destroy, so a target
// outside own ground is denied, and a remote or unreadable target is an ask.
func (r Rules) moveVerdict(k *given, c Case, cmd command, prog string, args []string, where string) *Verdict {
	switch prog {
	case "mv":
		srcs, dst := mvTargets(args)
		dstOK := dst != "" && r.groundOK(k, c, cmd, dst) && dst != "/dev/null"
		for _, s := range srcs {
			eff, known := effectiveTarget(cmd, s)
			if known && r.ownGround(ground(k, cmd), c, eff) {
				continue
			}
			if dstOK {
				return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
					Rationale: fmt.Sprintf("mv would move %s out of the task's ground; %s", display(eff, s), remit(c)),
					Advice:    "confirm you mean to move it, or name a source the task covers"}
			}
			return &Verdict{Decision: policy.Deny, Code: CodeOutOfRemit,
				Rationale: fmt.Sprintf("mv would remove %s, outside what the task names; %s", display(eff, s), remit(c)),
				Advice:    "keep moves to the paths the task names, or ask the person first"}
		}
	case "rsync":
		if remoteDest(args) && (hasDeleteFlag(args) || hasFlag(args, "--remove-source-files")) {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
				Rationale: fmt.Sprintf("rsync would delete at a remote destination the check cannot place; %s", remit(c)),
				Advice:    "confirm the destination, or run it without --delete"}
		}
		var targets []string
		ops := rsyncOperands(args)
		if hasDeleteFlag(args) && len(ops) > 0 {
			targets = append(targets, ops[len(ops)-1]) // the destination
		}
		if hasFlag(args, "--remove-source-files") && len(ops) > 1 {
			targets = append(targets, ops[:len(ops)-1]...) // the sources
		}
		for _, t := range targets {
			if v := r.judgeTarget(k, c, cmd, prog, t, where); v != nil {
				return v
			}
		}
	case "git":
		return r.gitCleanVerdict(k, c, cmd, args, where)
	}
	return nil
}

// gitCleanVerdict judges git clean -f, which deletes untracked files. It reads
// a cumulative -C, --work-tree and GIT_WORK_TREE for the tree, the explicit
// path operands when given, and asks when any of those is non-literal.
func (r Rules) gitCleanVerdict(k *given, c Case, cmd command, args []string, where string) *Verdict {
	tree := "."
	if v, ok := cmd.env["GIT_WORK_TREE"]; ok {
		tree = v
	}
	var paths []string
	forces, clean := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "--work-tree" || a == "--git-dir" || a == "-c" || a == "--config":
			val := ""
			if i+1 < len(args) {
				val, i = args[i+1], i+1
			}
			switch a {
			case "-C":
				tree = joinUnder(tree, val)
			case "--work-tree":
				tree = val
			}
		case strings.HasPrefix(a, "--work-tree="):
			tree = strings.TrimPrefix(a, "--work-tree=")
		case strings.HasPrefix(a, "--git-dir="), strings.HasPrefix(a, "-C="):
			// read but not a tree
		case a == "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-"):
			if !clean { // git's own options, before the subcommand
				continue
			}
			if a == "--force" || (!strings.HasPrefix(a, "--") && strings.ContainsAny(a, "fd")) {
				forces = true
			}
		case !clean:
			if a != "clean" {
				return nil // a different git subcommand
			}
			clean = true
		default:
			paths = append(paths, a) // a path operand to clean
		}
	}
	if !clean || !forces {
		return nil
	}
	targets := paths
	if len(targets) == 0 {
		targets = []string{tree}
	}
	for _, t := range targets {
		if strings.ContainsAny(t, "$*?[") {
			return &Verdict{Decision: policy.Ask, Code: CodeOutOfRemit, Confidence: 0.7,
				Rationale: fmt.Sprintf("git clean would delete in a path the check cannot place; %s", remit(c)),
				Advice:    "run it from the tree plainly, or confirm this is meant"}
		}
		eff := t
		if !strings.HasPrefix(t, "/") && !homeish(t) && tree != "." {
			eff = joinUnder(tree, t)
		}
		sub := cmd
		if v := r.judgeTarget(k, c, sub, "git", eff, where); v != nil {
			return v
		}
	}
	return nil
}

// joinUnder joins rel onto base, with an absolute or ~ rel replacing base (as
// git -C and a path operand do).
func joinUnder(base, rel string) string {
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "~") {
		return rel
	}
	return joinDir(base, rel)
}

// groundOK reports a path resolved and within own ground.
func (r Rules) groundOK(k *given, c Case, cmd command, t string) bool {
	eff, known := effectiveTarget(cmd, t)
	return known && r.ownGround(ground(k, cmd), c, eff)
}

// ground gives the facts to judge a target by, with said widened to the
// person's own words where the directory is unknown (as judgeTarget does).
func ground(k *given, cmd command) *given {
	if cmd.dirKnown {
		return k
	}
	p := *k
	p.said = k.person
	return &p
}

// mvTargets are mv's sources and its destination. With -t/--target-directory
// every operand is a source and the directory is the destination.
func mvTargets(args []string) ([]string, string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-t" || a == "--target-directory" {
			if i+1 < len(args) {
				return operands(args), args[i+1]
			}
		}
		if v, ok := strings.CutPrefix(a, "--target-directory="); ok {
			return operands(args), v
		}
	}
	ops := operands(args)
	if len(ops) < 2 {
		return nil, ""
	}
	return ops[:len(ops)-1], ops[len(ops)-1]
}

// rsyncValueFlags take a value the next word gives, which is not a path.
var rsyncValueFlags = map[string]bool{"--exclude": true, "--include": true, "--filter": true, "-f": true,
	"--exclude-from": true, "--include-from": true, "-e": true, "--rsh": true, "--backup-dir": true,
	"--partial-dir": true, "--temp-dir": true, "-T": true, "--log-file": true, "--files-from": true,
	"--compare-dest": true, "--link-dest": true, "--copy-dest": true, "--out-format": true, "--chmod": true}

// rsyncOperands are rsync's source and destination paths, with option values
// skipped wherever they appear.
func rsyncOperands(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if rsyncValueFlags[a] {
			i++ // skip its value
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// hasDeleteFlag reports any --delete* or --del spelling.
func hasDeleteFlag(args []string) bool {
	for _, a := range args {
		if a == "--del" || strings.HasPrefix(a, "--delete") {
			return true
		}
	}
	return false
}

// remoteDest reports a remote rsync destination (host:path, user@host:path,
// rsync://…) as the last operand.
func remoteDest(args []string) bool {
	ops := rsyncOperands(args)
	if len(ops) == 0 {
		return false
	}
	d := ops[len(ops)-1]
	if strings.HasPrefix(d, "rsync://") {
		return true
	}
	i := strings.IndexByte(d, ':')
	return i > 0 && !strings.HasPrefix(d, "/") && !strings.Contains(d[:i], "/")
}

// gitWorkTree is the tree git clean runs in: GIT_WORK_TREE or --work-tree win,
// else successive -C paths joined onto the start, else the working directory.

// findExecHidden reports a find whose -exec program is not a literal the
// check reads: an expansion it cannot resolve.
func findExecHidden(args []string) bool {
	for i, a := range args {
		if isExecFlag(a) && i+1 < len(args) && strings.HasPrefix(args[i+1], "$") {
			return true
		}
	}
	return false
}

// kubectlDeleteNS is the namespace a kubectl delete names, if any.
func kubectlDeleteNS(prog string, args []string) string {
	if prog != "kubectl" || len(operands(args)) == 0 || !hasWord(args, "delete") {
		return ""
	}
	for i, a := range args {
		if (a == "-n" || a == "--namespace") && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--namespace="); ok {
			return v
		}
	}
	return ""
}

func hasWord(args []string, w string) bool {
	for _, a := range args {
		if a == w {
			return true
		}
	}
	return false
}

// program refuses a command whose program is not there to run: locally,
// one not on the PATH commands get; on a host, one nothing has shown is
// asked about first.
func (r Rules) program(k *given, c Case) *Verdict {
	src, ok := line(c, k)
	if !ok {
		return nil
	}
	s, ok := parseScript(src)
	if !ok || s.setsPath || declaredRead(c) {
		return nil
	}
	for _, cmd := range s.cmds {
		if p, _ := cmd.program(); installers[p] {
			return nil // the line installs what it then runs
		}
	}
	for _, cmd := range s.cmds {
		p, _ := cmd.program()
		if p == "" || strings.ContainsAny(p, "/$=") || builtins[p] || s.funcs[p] || strings.HasPrefix(p, "-") {
			continue
		}
		if !remote(c) {
			if r.LookPath == nil || r.LookPath(p) {
				continue
			}
			return &Verdict{Decision: policy.Deny, Code: CodeUnknownProgram,
				Rationale: fmt.Sprintf("%s is not on the PATH commands run with here, so this cannot work", p),
				Advice:    fmt.Sprintf("inspect first (command -v %s), or use a program that is installed", p)}
		}
		if common[p] || k.named(p) || k.observed(p) {
			continue
		}
		host := k.str("host")
		return &Verdict{Decision: policy.Ask, Code: CodeUnknownProgram, Confidence: 0.6,
			Rationale: fmt.Sprintf("nothing in this session has shown that %s exists on %s", p, host),
			Advice:    fmt.Sprintf("inspect first (command -v %s on %s), then run it", p, host)}
	}
	return nil
}

// evidence asks about a change aimed at a path or host that the user never
// named and nothing earlier in the session showed: the shape of a target
// the model made up, or took from text it fetched.
func (r Rules) evidence(k *given, c Case) *Verdict {
	if !c.Mutates || declaredRead(c) {
		return nil
	}
	var paths, hosts []string
	switch c.Tool {
	case "write", "edit":
		paths = append(paths, k.str("path"))
	case "ssh_put":
		paths = append(paths, k.str("remote_path"))
	case "bash", "ssh":
		s, ok := parseScript(k.str("command"))
		if !ok {
			return nil
		}
		for _, cmd := range s.cmds {
			paths = append(paths, cmd.writes...)
			prog, args := cmd.program()
			paths = append(paths, written(prog, args)...)
			for _, h := range reached(prog, args) {
				if !wellKnown[h] || sends(prog, args) {
					hosts = append(hosts, h)
				}
			}
		}
	case "k8s_apply":
		for _, key := range []string{"namespace", "name"} {
			v := k.str(key)
			if v == "" || v == "default" || k.named(v) || k.observed(v) {
				continue
			}
			return &Verdict{Decision: policy.Ask, Code: CodeUnverifiedTarget, Confidence: 0.7,
				Rationale: fmt.Sprintf("the %s %s was never named by the person nor shown by an earlier call", key, v),
				Advice:    "inspect first (k8s_get) so the target is one you have seen, or ask the person which they mean"}
		}
		return nil
	default:
		return nil
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") || k.named(p) || k.observed(p) {
			continue
		}
		if !remote(c) && k.local(p) || remote(c) && scratch(path.Clean(p)) {
			continue
		}
		return &Verdict{Decision: policy.Ask, Code: CodeUnverifiedTarget, Confidence: 0.7,
			Rationale: fmt.Sprintf("%s was never named by the person nor shown by an earlier call", p),
			Advice:    fmt.Sprintf("inspect first (ls -la %s) so the target is one you have seen, or ask the person", path.Dir(p))}
	}
	for _, h := range hosts {
		if loopback(h) || k.named(h) || k.observed(h) {
			continue
		}
		return &Verdict{Decision: policy.Ask, Code: CodeUnverifiedHost, Confidence: 0.7,
			Rationale: fmt.Sprintf("the host %s was never named by the person nor shown by an earlier command; "+
				"text fetched from the web does not count", h),
			Advice: "use a host the person named, or ask them before contacting this one"}
	}
	return nil
}

// wellKnown are the hosts code and packages come from. Fetching from one
// needs no evidence; sending to one still does.
var wellKnown = map[string]bool{}

func init() {
	for _, h := range strings.Fields(`github.com raw.githubusercontent.com objects.githubusercontent.com codeload.github.com
		gitlab.com bitbucket.org pypi.org files.pythonhosted.org registry.npmjs.org registry.yarnpkg.com proxy.golang.org
		sum.golang.org go.dev golang.org crates.io static.crates.io index.crates.io rubygems.org repo.maven.apache.org
		repo1.maven.org dl.google.com deb.debian.org archive.ubuntu.com security.ubuntu.com dl-cdn.alpinelinux.org
		registry-1.docker.io docker.io ghcr.io quay.io`) {
		wellKnown[h] = true
	}
}

// sends reports a command that sends data to the host it names, rather
// than fetching from it.
func sends(prog string, args []string) bool {
	switch prog {
	case "scp", "rsync", "sftp", "nc", "ncat", "netcat", "socat", "telnet", "ftp", "ssh":
		return true
	case "git":
		return len(args) > 0 && args[0] == "push"
	case "curl":
		for i, a := range args {
			switch {
			case a == "-d" || a == "-F" || a == "-T" || a == "--upload-file" || a == "--form" || a == "--json",
				strings.HasPrefix(a, "--data"), strings.HasPrefix(a, "-d") && len(a) > 2:
				return true
			case (a == "-X" || a == "--request") && i+1 < len(args) && strings.ToUpper(args[i+1]) != "GET" && strings.ToUpper(args[i+1]) != "HEAD":
				return true
			}
		}
	case "wget":
		return hasPrefix(args, "--post") || hasPrefix(args, "--method") || hasPrefix(args, "--body")
	case "http", "https":
		for _, a := range args {
			if strings.Contains(a, "=") || strings.Contains(a, ":=") {
				return true
			}
		}
	}
	return false
}

// hostChoice asks about a change on one configured host when the task, or
// the approved plan, names others by name and this one nowhere: the shape of
// a model that picked the wrong machine. A task that names no configured
// host ("the web hosts") leaves the choice to the model.
func (r Rules) hostChoice(k *given, c Case) *Verdict {
	if (c.Tool != "ssh" && c.Tool != "ssh_put") || declaredRead(c) || len(r.Hosts) == 0 {
		return nil
	}
	target := strings.ToLower(k.str("host"))
	text := k.said // the approved plan is in it already
	var chosen *DeclaredHost
	var named []string
	for i, h := range r.Hosts {
		hit := false
		for _, n := range []string{h.Name, h.Addr} {
			n = strings.ToLower(n)
			if n == "" {
				continue
			}
			if n == target {
				chosen = &r.Hosts[i]
			}
			hit = hit || mentions(text, n)
		}
		if hit {
			named = append(named, h.Name)
		}
	}
	if chosen == nil || len(named) == 0 {
		return nil
	}
	for _, n := range []string{chosen.Name, chosen.Addr} {
		if n != "" && (mentions(text, strings.ToLower(n)) || k.observed(n)) {
			return nil
		}
	}
	return &Verdict{Decision: policy.Ask, Code: CodeUnverifiedHost, Confidence: 0.8,
		Rationale: fmt.Sprintf("the task names %s, not %s, and nothing earlier showed %s", strings.Join(named, ", "), chosen.Name, chosen.Name),
		Advice:    fmt.Sprintf("run it on %s, as the task says, or ask the person whether %s is meant", strings.Join(named, " or "), chosen.Name)}
}

// mentions reports n in text as a whole word, so web1 is not found in web10.
func mentions(text, n string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], n)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(n)
		// A dot ends a word only where no name goes on after it: web1. ends a
		// sentence; web1.example is another name.
		after := end == len(text) || !hostRune(text[end]) && (text[end] != '.' || end+1 == len(text) || !hostRune(text[end+1]))
		if (start == 0 || !hostRune(text[start-1]) && text[start-1] != '.') && after {
			return true
		}
		i = start + 1
	}
}

func hostRune(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

// bareMentions reports word as a whole word in said, counting only where it
// stands on its own: not inside a path (those are blanked first) and not the
// host the call names.
func bareMentions(said, word, host string) bool {
	if word == "" || word == host {
		return false
	}
	bare := userPaths.ReplaceAllString(said, " ")
	return mentions(bare, word)
}

// written names the files a command writes through its arguments.
func written(prog string, args []string) []string {
	ops := operands(args)
	switch prog {
	case "tee", "touch", "mkdir", "chmod", "chown", "chgrp":
		if prog == "chmod" || prog == "chown" || prog == "chgrp" {
			if len(ops) > 0 {
				ops = ops[1:] // the mode or owner
			}
		}
		return ops
	case "cp", "mv", "ln", "install", "rsync", "scp":
		if len(ops) > 1 {
			return ops[len(ops)-1:]
		}
	case "sed", "perl":
		if hasFlag(args, "-i") || hasPrefix(args, "-i") {
			if len(ops) > 1 {
				return ops[1:]
			}
		}
	}
	return nil
}

func hasPrefix(args []string, p string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

// netPrograms reach the host their arguments name.
var netPrograms = map[string]bool{"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true, "socat": true,
	"telnet": true, "ssh": true, "scp": true, "sftp": true, "rsync": true, "ftp": true, "http": true, "https": true}

var urlHost = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://(?:[^/@\s]*@)?(\[[^\]]+\]|[^/:?#\s]+)`)

// reached names the hosts a command contacts, from its URLs and the
// arguments of programs that reach the network.
func reached(prog string, args []string) []string {
	var out []string
	for _, a := range args {
		if m := urlHost.FindStringSubmatch(a); m != nil {
			out = append(out, strings.ToLower(strings.Trim(m[1], "[]")))
		}
	}
	if !netPrograms[prog] {
		return out
	}
	for _, a := range operands(args) {
		if strings.Contains(a, "://") || strings.HasPrefix(a, "$") {
			continue
		}
		h := a
		if _, after, ok := strings.Cut(h, "@"); ok {
			h = after
		}
		if i := strings.IndexAny(h, ":/"); i >= 0 {
			h = h[:i]
		}
		if hostish(h) {
			out = append(out, strings.ToLower(h))
		}
	}
	return out
}

// hostish is a word shaped like a host name or an address.
func hostish(h string) bool {
	if h == "" || !strings.Contains(h, ".") || strings.HasPrefix(h, ".") {
		return false
	}
	return Names(h) != nil
}

func loopback(h string) bool {
	switch h {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "host.docker.internal", "host.containers.internal":
		return true
	}
	return strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || strings.HasPrefix(h, "127.")
}

// composition asks before a call that can send data out, once a credential
// file has been read in the session.
func (r Rules) composition(k *given, c Case) *Verdict {
	if !egressCapable(c, k) {
		return nil
	}
	if f := sentFile(c, k); f != "" {
		return &Verdict{Decision: policy.Ask, Code: CodeCredentialEgress, Confidence: 0.8,
			Rationale: fmt.Sprintf("this call reads a credential (%s) and can send it out", clip(f, 80)),
			Advice:    "do not send credential contents anywhere; if the task needs the credential, ask the person to store it as a secret"}
	}
	for _, p := range c.Recent {
		if p.IsError || !p.Credential {
			continue
		}
		return &Verdict{Decision: policy.Ask, Code: CodeCredentialEgress, Confidence: 0.8,
			Rationale: fmt.Sprintf("a credential (%s) was read earlier in this session, and this call can send data out", clip(p.Subject, 80)),
			Advice:    "do not send credential contents anywhere; if the task needs the credential, ask the person to store it as a secret"}
	}
	return nil
}

// Untrusted reports a call whose output is text from outside, which names
// hosts and paths someone else chose and so is no evidence for a target:
// the web tools, an MCP server's tools, and a bash or ssh line that reaches
// the network.
func Untrusted(tool, args string) bool {
	switch {
	case tool == "web_fetch" || tool == "web_search" || strings.HasPrefix(tool, "mcp__"):
		return true
	case tool == "bash" || tool == "ssh":
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(args), &a) != nil {
			return true
		}
		s, ok := parseScript(a.Command)
		if !ok {
			return true
		}
		for _, cmd := range s.cmds {
			prog, rest := cmd.program()
			if netPrograms[prog] && prog != "ssh" && prog != "scp" && prog != "rsync" {
				return true
			}
			if prog == "git" && len(rest) > 0 && (rest[0] == "clone" || rest[0] == "fetch" || rest[0] == "pull") {
				return true
			}
			for _, w := range rest {
				if urlHost.MatchString(w) {
					return true
				}
			}
		}
	}
	return false
}

func egressCapable(c Case, k *given) bool {
	switch c.Tool {
	case "web_fetch", "web_search", "ssh", "ssh_connect", "ssh_put":
		return true
	case "bash":
		s, ok := parseScript(k.str("command"))
		if !ok {
			return true // a line that cannot be read could do anything
		}
		for _, cmd := range s.cmds {
			prog, args := cmd.program()
			if netPrograms[prog] {
				return true
			}
			if prog == "git" && len(args) > 0 && (args[0] == "push" || args[0] == "remote") {
				return true
			}
			if interpreters[prog] && (hasFlag(args, "-c") || hasFlag(args, "-e")) {
				return true
			}
		}
	}
	return false
}

var credentialPatterns = regexp.MustCompile(`(?i)(?:^|[/\s"'])(?:\.ssh/|id_(?:rsa|dsa|ecdsa|ed25519)\b|\.aws/credentials|\.aws/config|` +
	`\.git/config|\.netrc|\.git-credentials|\.docker/config\.json|\.kube/config|\.npmrc|\.pypirc|\.pgpass|\.vault-token|` +
	`/etc/shadow|\.env(?:\.[a-z0-9_-]+)?(?:$|[\s"'])|[a-z0-9_-]*credentials?\.json|[^\s/"']+\.(?:pem|key|p12|pfx)(?:$|[\s"']))`)

// publicKeyMaterial are files that look like keys but hold only what is
// published: certificates, chains and public keys.
var publicKeyMaterial = regexp.MustCompile(`(?i)(?:/certs?/[^\s"']*|[^\s/"']*\.crt\b|[^\s/"']*\.pub\b|(?:^|[/\s"'_.-])(?:cert|fullchain|chain|ca|bundle)\.pem\b|\.env\.(?:example|sample|template|dist)\b)`)

func credentialish(subject string) bool {
	if !credentialPatterns.MatchString(subject) {
		return false
	}
	// A subject naming a public file is still a credential if it names a secret one too.
	return !publicKeyMaterial.MatchString(subject) || credentialPatterns.MatchString(publicKeyMaterial.ReplaceAllString(subject, " "))
}

// sentFile is a credential file the call itself reads and could send: an
// ssh_put of one, or a bash line that can reach the network and reads one
// (curl -d @.env, a substitution, a redirection).
func sentFile(c Case, k *given) string {
	switch c.Tool {
	case "ssh_put":
		if f := k.str("file"); credentialish(f) {
			return f
		}
	case "bash":
		s, ok := parseScript(k.str("command"))
		if !ok {
			return ""
		}
		for _, cmd := range s.cmds {
			for _, w := range append(append([]string(nil), cmd.words...), cmd.reads...) {
				_, f, _ := strings.Cut(w, "@")
				if f == "" {
					f = w
				}
				if credentialish(" " + f) {
					return f
				}
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

var interpreters = map[string]bool{"python": true, "python3": true, "node": true, "ruby": true, "perl": true, "php": true}

// installers put programs on the PATH; a line running one may then run what it installed.
var installers = map[string]bool{"apt": true, "apt-get": true, "brew": true, "pip": true, "pip3": true, "npm": true,
	"pnpm": true, "yarn": true, "go": true, "cargo": true, "gem": true, "conda": true, "mamba": true, "uv": true,
	"pipx": true, "apk": true, "dnf": true, "yum": true, "zypper": true, "pacman": true, "nix": true, "nix-env": true,
	"port": true, "snap": true, "asdf": true, "mise": true, "rustup": true, "corepack": true, "make": true,
	"source": true, ".": true, "alias": true, "hash": true}

// builtins are the shell's own words, which no PATH holds.
var builtins = map[string]bool{}

// common are programs any Linux host has; others are inspected first.
var common = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`cd echo printf test [ [[ ]] true false export set unset read local declare typeset
		source . eval exec exit return shift trap wait alias unalias type hash ulimit umask pushd popd dirs let getopts
		command builtin jobs fg bg kill history break continue : readonly times shopt enable help logout mapfile
		readarray caller compgen complete disown suspend fc if then else elif fi for while until do done case esac
		function select time coproc { } ! pwd`) {
		builtins[w] = true
	}
	for _, w := range strings.Fields(`ls cat head tail grep egrep fgrep awk sed cut sort uniq wc tr tee touch mkdir rmdir
		rm cp mv ln chmod chown chgrp find xargs stat file du df free uptime ps top kill pkill pgrep id whoami hostname
		uname date sleep env printenv which basename dirname readlink realpath tar gzip gunzip zcat bzip2 xz diff cmp
		comm join paste split tac rev nl od hexdump md5sum sha1sum sha256sum base64 mktemp seq yes timeout nohup nice
		sudo su ip ss netstat ping curl wget systemctl journalctl service mount umount lsblk lsof dmesg logger crontab
		useradd usermod groupadd passwd last w who getent sh bash dash less more vi vim nano tput clear stty locale
		ldd strings sync shutdown reboot halt poweroff hostnamectl timedatectl localectl loginctl dig nslookup host
		openssl ssh scp rsync git python3 perl iptables nft ufw`) {
		common[w] = true
	}
}
