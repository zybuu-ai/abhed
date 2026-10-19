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
}

// DeclaredHost is one configured ssh host.
type DeclaredHost struct{ Name, Addr string }

// Judge applies the rules: a deny for destructive work outside the remit or
// a program that is not there, an ask for a target nothing showed or a
// credential followed by a way out, and ok otherwise.
func (r Rules) Judge(_ context.Context, c Case) (Verdict, error) {
	k := facts(c)
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
	said  string // everything the user said, lower case
	roots []string
	seen  func(string) bool
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

// named reports whether the user said the text, or a folder holding it.
func (k *given) named(s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	if strings.Contains(k.said, low) || slices.Contains(k.declared, low) {
		return true
	}
	if strings.HasPrefix(s, "/") {
		for _, m := range userPaths.FindAllStringSubmatch(k.said, -1) {
			p := strings.TrimRight(m[1], "/.,;:")
			if len(p) > 1 && within(low, p) {
				return true
			}
		}
	}
	return false
}

// observed reports whether an earlier observation or successful call showed
// the text, or the folder that will hold it.
func (k *given) observed(s string) bool {
	if s == "" {
		return false
	}
	for _, d := range k.done {
		if strings.Contains(d, s) {
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

var userPaths = regexp.MustCompile(`(?:^|[\s"'(=:])(/[^\s"'()<>|;&` + "`" + `]+)`)

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
	s, ok := parseScript(src)
	if !ok {
		return nil
	}
	where := "here"
	if remote(c) {
		where = "on " + k.str("host")
	}
	for _, cmd := range s.cmds {
		prog, args := cmd.program()
		for _, t := range destroys(prog, args) {
			if t == "" || strings.HasPrefix(t, "$") && !homeish(t) {
				continue // a target only running the line would show
			}
			if r.ownGround(k, c, t) {
				continue
			}
			return &Verdict{Decision: policy.Deny, Code: CodeOutOfRemit,
				Rationale: fmt.Sprintf("%s would destroy %s %s, outside what the task names; %s", prog, t, where, remit(c)),
				Advice:    "keep destructive commands to the paths the task names, or ask the person first"}
		}
		if ns := kubectlDeleteNS(prog, args); ns != "" && ns != "default" && !k.named(ns) {
			return &Verdict{Decision: policy.Deny, Code: CodeOutOfRemit,
				Rationale: fmt.Sprintf("kubectl would delete in namespace %s, which the task never named; %s", ns, remit(c)),
				Advice:    "stay within the namespaces the task names, or ask the person before deleting anything else"}
		}
	}
	return nil
}

// ownGround reports whether a destructive target is within the remit.
func (Rules) ownGround(k *given, c Case, t string) bool {
	if homeish(t) {
		return k.named(t)
	}
	if !strings.HasPrefix(t, "/") {
		if remote(c) {
			return true // relative to the login's home; the host was named or configured
		}
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

func homeish(t string) bool {
	return t == "~" || strings.HasPrefix(t, "~/") || strings.HasPrefix(t, "$HOME") || strings.HasPrefix(t, "${HOME}")
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
		var out []string
		for _, a := range args {
			if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
				break
			}
			out = append(out, a)
		}
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

// oneLiner is the code an interpreter is given on its command line.
func oneLiner(prog string, args []string) string {
	flag := ""
	switch {
	case strings.HasPrefix(prog, "python"):
		flag = "-c"
	case prog == "node", prog == "perl", prog == "ruby":
		flag = "-e"
	case prog == "php":
		flag = "-r"
	default:
		return ""
	}
	for i, a := range args {
		if (a == flag || prog == "node" && a == "--eval") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// codeDeletes finds the well-known calls that delete or overwrite a file,
// with the literal path they are given, in one line of Python, Node, Perl,
// Ruby or PHP. A textual scan: it finds the common forms, not every one.
var codeDeletes = regexp.MustCompile(`(?:\b(?:rmtree|remove|unlink|rmdir|removedirs|rmSync|unlinkSync|rmdirSync|rm_rf|rm_r|rm_f|delete|truncate|writeFileSync|write_text|write_bytes)\s*\(?\s*['"]([^'"]+)['"]` +
	`|\bPath\(\s*['"]([^'"]+)['"]\s*\)\s*\.\s*(?:unlink|rmdir|write_text|write_bytes)\b` +
	`|\bopen\(\s*['"]([^'"]+)['"]\s*,\s*['"][wa])`)

func execsRemove(args []string) bool {
	for i, a := range args {
		if (a == "-exec" || a == "-execdir" || a == "-ok") && i+1 < len(args) {
			switch args[i+1] {
			case "rm", "shred", "unlink":
				return true
			}
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
	text := k.said + "\n" + strings.ToLower(c.Plan)
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
		if (start == 0 || !hostRune(text[start-1])) && (end == len(text) || !hostRune(text[end])) {
			return true
		}
		i = start + 1
	}
}

func hostRune(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_'
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
		if p.IsError || !credentialish(p.Subject) {
			continue
		}
		switch p.Tool {
		case "read", "grep", "bash", "ssh":
		default:
			continue
		}
		return &Verdict{Decision: policy.Ask, Code: CodeCredentialEgress, Confidence: 0.8,
			Rationale: fmt.Sprintf("a credential (%s) was read earlier in this session, and this call can send data out", clip(p.Subject, 80)),
			Advice:    "do not send credential contents anywhere; if the task needs the credential, ask the person to store it as a secret"}
	}
	return nil
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
