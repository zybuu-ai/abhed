package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sedscript"
)

// ExitStatus is a process's exit code, or for one ended by a signal the
// status a shell reports: 128 plus the signal's number.
func ExitStatus(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

const (
	defaultTimeoutMS = 120_000
	maxTimeoutMS     = 600_000
	maxOutputChars   = 30_000
)

// Bash runs a shell command in the session workspace.
//
// It executes inside the configured sandbox tier (docs §03-security).
// The guards here are defense in depth, not the boundary: they catch the
// obvious footguns early and give the model a useful message, but isolation is
// what actually contains a hostile command.
type Bash struct {
	// Sandbox, when set, wraps the command (e.g. a bubblewrap, container or gVisor exec).
	// Nil means direct execution, which is only appropriate for local dev.
	Sandbox func(ctx context.Context, cwd, command string) *exec.Cmd
	// Home is the HOME Sandbox gives a command, as a host path, and whether
	// the host can read it. Nil with a Sandbox is taken as hidden.
	Home func() (string, bool)
	// Secrets resolves names the model asked for into NAME=value pairs for
	// one command's environment. Policy has allowed each name by the time
	// this runs. Nil means no secrets are available.
	Secrets func(names []string) ([]string, error)
	// SecretNames is what the model may ask for, by name only.
	SecretNames []string
	// Shell, when set, starts a long-lived interactive shell under the same
	// sandbox as Sandbox. Only the workbench terminal uses it.
	Shell func(ctx context.Context, cwd string) *exec.Cmd
	// Isolation says what contains the commands, for a person to read.
	Isolation Isolation
	// RanUnder, when set, is the tier a command ran under, for a sandbox
	// chosen after start-up; Isolation.Tier is then the weakest it can be.
	RanUnder func() string
	// Forward, when set, passes named variables already in a command's Env
	// into what Sandbox built, in place of sandbox.ForwardEnv.
	Forward func(cmd *exec.Cmd, names []string)
	// ShellLine is the line the interactive terminal is judged and recorded
	// as; nil is bash's.
	ShellLine func() string
	// NotFound, when set, says what a command run in dir that could not
	// find program can do about it, such as install the toolchain; "" says nothing.
	NotFound func(dir, program string) string
	// Provides reports whether the toolchains put program first on PATH for
	// a command in dir, inside the sandbox: the supervisor counts such a
	// program as there, as a command finds it. Nil when none are given.
	Provides func(dir, program string) bool
	// NetworkNote, when set, is added to the note a command that could not
	// reach the network gets, such as how to open a package registry.
	NetworkNote string
	// RegistryNote, when set, says what a command refused a package
	// registry can do, naming the language; "" when it names none.
	RegistryNote func(command, output string) string
}

// Isolation describes the sandbox in force: its tier, the mechanism that
// provides it, and whether commands can reach the network.
type Isolation struct {
	Tier    string `json:"tier"`
	Backend string `json:"backend"`
	Network bool   `json:"network"`
	// Allowlist is sandbox.network allowlist: commands reach only what the
	// egress rules allow, through the session's proxy, and Network is false.
	Allowlist bool `json:"allowlist,omitempty"`
	// AllowedHosts are the hosts the allow rules name; DefaultAllow is a
	// default that lets through what no rule denies.
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	DefaultAllow bool     `json:"default_allow,omitempty"`
}

func (Bash) Name() string  { return "bash" }
func (Bash) Mutates() bool { return true }

func (b Bash) Description() string {
	d := "Run a shell command in the session workspace. Use for builds, tests, git, and package managers. Prefer read/glob/grep for file inspection — they are cheaper and safer. Note: a call that is just `cd <folder>` sets the working directory for later calls; shell state (variables, functions) and a cd inside a longer command do not carry over."
	switch b.network() {
	case networkOff:
		d += " The sandbox has no network: commands cannot reach the internet or any other host, so curl, wget, package installs and git fetch fail. To read from the web, use web_search or web_fetch if they are in your tools; otherwise tell the user."
	case networkOn:
		d += " Commands can reach the network."
	case networkAllowlist:
		d += " " + b.allowlistText()
	}
	if b.Isolation.Tier == "fence" {
		d += " Commands run under the fence: they write only the workspace and a private temp folder (HOME and TMPDIR point there), and unix sockets, such as docker's or an ssh agent's, are refused."
	}
	if len(b.SecretNames) > 0 {
		d += " Secrets available by name, as environment variables for one command when listed in `secrets`: " + strings.Join(b.SecretNames, ", ") + ". You never see their values."
	}
	return d
}

func (Bash) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","description":"The shell command to run."},
    "description":{"type":"string","description":"Short human-readable description of what this does, shown in the approval prompt."},
    "timeout_ms":{"type":"integer","description":"Timeout in milliseconds. Default 120000, max 600000."},
    "secrets":{"type":"array","items":{"type":"string"},"description":"Names of stored secrets this command needs as environment variables. Each must be permitted by policy."},
    "run_in_background":{"type":"boolean","description":"Start the command and return at once with a shell id, for a server, a watcher or a long build. Read its output with shell_output, stop it with shell_kill; you are told when it ends. timeout_ms, when given, bounds its life."}
  },
  "required":["command","description"]
}`)
}

type bashArgs struct {
	Command     string   `json:"command"`
	Description string   `json:"description"`
	TimeoutMS   int      `json:"timeout_ms"`
	Secrets     []string `json:"secrets"`
	Background  bool     `json:"run_in_background"`
}

// editorPattern matches full-screen programs; vim in silent Ex mode is exempt.
var editorPattern = regexp.MustCompile(`^\s*(vim?|nano|emacs|less|more|top|htop)\b`)

// Commands that hang forever waiting for a TTY. Rejecting them with guidance
// is far better than a 10-minute timeout.
var interactivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bgit\s+(rebase|add|commit)\s+.*-i\b`),
	regexp.MustCompile(`\bgit\s+(rebase|add)\s+--interactive\b`),
	editorPattern,
	// ssh without -T allocates a TTY and blocks; -T is the non-interactive form.
	regexp.MustCompile(`^\s*ssh\s+(?:-[^T\s]*\s+)*[^-\s]`),
}

// viStart captures a leading vi or vim's arguments, up to the first ; | or &.
var viStart = regexp.MustCompile(`^\s*vim?\s+([^|;&]*)`)

// isInteractive reports whether a command would wait for a terminal. Vim in
// silent Ex mode (-es, -e -s, -Es) runs a script and exits: a batch edit.
func isInteractive(command string) bool {
	for _, re := range interactivePatterns {
		if re.MatchString(command) && (re != editorPattern || !silentEx(command)) {
			return true
		}
	}
	return false
}

func silentEx(command string) bool {
	m := viStart.FindStringSubmatch(command)
	if m == nil {
		return false
	}
	var ex, silent bool
	for _, arg := range strings.Fields(m[1]) {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue
		}
		// Vim reads -s as silent only after -e or -E; before them it names a
		// file of Normal-mode keys, and the editor opens.
		for _, c := range arg[1:] {
			switch {
			case c == 'e' || c == 'E':
				ex = true
			case c == 's' && ex:
				silent = true
			}
		}
	}
	return silent
}

// Patterns that require confirmation in every mode, including the most
// permissive (docs P7). These are the commands with no undo.
var destructivePatterns = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rf]`), "recursive/forced delete"},
	{regexp.MustCompile(`\bgit\s+push\s+.*--force(?:-with-lease)?\b`), "force push"},
	{regexp.MustCompile(`\bgit\b.*\spush\b.*\s--mirror\b`), "mirror push (deletes remote refs)"},
	{regexp.MustCompile(`\bgit\b.*\spush\b.*\s--prune\b`), "prune remote branches"},
	{regexp.MustCompile(`\bgit\b.*\sgc\b.*--prune=(?:now|all)\b|\bgit\b.*\sreflog\s+expire\b.*--expire(?:-unreachable)?=(?:now|all|0)`), "make discarded commits unrecoverable"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "hard reset"},
	{regexp.MustCompile(`\bgit\s+clean\s+.*-[a-zA-Z]*f`), "forced clean"},
	{regexp.MustCompile(`\b(mkfs|fdisk|dd)\b`), "disk operation"},
	{regexp.MustCompile(`\bchmod\s+(-R\s+)?777\b`), "world-writable permissions"},
	{regexp.MustCompile(`:\(\)\s*\{.*\}\s*;?\s*:`), "fork bomb"},
	{regexp.MustCompile(`\b(shutdown|reboot|halt)\b`), "system power command"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|disk)`), "raw device write"},
	// Deletion without `rm`. Found by TestAttack_CommandWrappingBypass, which
	// tried wrapped forms of a denied command: `find . -delete` recursively
	// removes files while containing none of the patterns above.
	{regexp.MustCompile(`\bfind\b.*\s-delete\b`), "recursive delete via find"},
	{regexp.MustCompile(`\bfind\b.*-exec\s+rm\b`), "recursive delete via find -exec"},
	{regexp.MustCompile(`\bxargs\b.*\brm\b`), "delete via xargs"},
	{regexp.MustCompile(`\bshred\b`), "secure delete"},
	{regexp.MustCompile(`\btruncate\s+-s\s*0\b`), "file truncation"},
	{regexp.MustCompile(`\bgit\s+checkout\s+--\s+\.`), "discard all working-tree changes"},
	{regexp.MustCompile(`\bgit\s+branch\s+-D\b`), "force branch delete"},
	{regexp.MustCompile(`\bunlink\s+\S`), "delete via unlink"},
	{regexp.MustCompile(`\brmdir\b.*\s(?:-[a-zA-Z]*p[a-zA-Z]*|--parents?)\b`), "delete parent folders (rmdir -p)"},
}

// segmentPatterns are matched against each simple command of a line on its
// own (the line split on ;, &, |, ( and )), so a word from one command never
// completes a match begun in another. They cover the operations with no undo
// on containers, clusters, data stores and cloud infrastructure.
var segmentPatterns = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\b(?:docker|podman|nerdctl)\b.*\bsystem\s+(?:prune|reset)\b`), "container system prune/reset"},
	{regexp.MustCompile(`\b(?:docker|podman|nerdctl)\b.*\bvolume\s+(?:rm|remove|prune)\b`), "container volume delete"},
	{regexp.MustCompile(`\b(?:docker|podman|nerdctl)\b.*\b(?:container|network)\s+prune\b`), "prune containers or networks"},
	{regexp.MustCompile(`\b(?:docker|podman)\b.*\b(?:stack|service)\s+(?:rm|remove)\b`), "remove a swarm stack or service"},
	{regexp.MustCompile(`\b(?:docker|podman)\b.*\bswarm\s+leave\b.*(?:\s-f\b|\s--force\b)`), "force a node out of the swarm"},
	{regexp.MustCompile(`\b(?:docker|podman|nerdctl)\b.*\bimage\s+prune\b.*(?:\s-[a-zA-Z]*a|\s--all\b)`), "remove all container images"},
	{regexp.MustCompile(`\b(?:docker|podman|nerdctl)\b.*\b(?:rmi|image\s+(?:rm|remove))\b.*(?:\s-[a-zA-Z]*f|\s--force\b)`), "forced image removal"},
	{regexp.MustCompile(`(?i)\b(?:kubectl|oc)\b.*\bdelete\b.*[\s,](?:ns|namespaces?|projects?|pvc|pv|persistentvolumes?|persistentvolumeclaims?|all|crds?|customresourcedefinitions?|storageclass(?:es)?|sc|clusterroles?|clusterrolebindings?)(?:[\s/,.]|$)`), "cluster delete of namespaces, volumes, definitions or every resource"},
	{regexp.MustCompile(`\b(?:kubectl|oc)\b.*\bdelete\b.*\s(?:--all\b|--all-namespaces\b|-A\b)`), "cluster delete across all resources or namespaces"},
	{regexp.MustCompile(`\b(?:kubectl|oc)\b.*\bscale\b.*--replicas[=\s]+0\b`), "scale a workload to zero"},
	{regexp.MustCompile(`\b(?:kubectl|oc)\b.*\s(?:drain|cordon)\b`), "drain or cordon a node"},
	{regexp.MustCompile(`\b(?:kubectl|oc)\b.*\s(?:patch|replace)\b.*(?:--force\b|finalizers)`), "forced patch/replace"},
	{regexp.MustCompile(`\b(?:kubectl|oc)\b(?:.*(?:-n|--namespace|--context|-l|--selector)[=\s]+(?:[\w.]*[-_.=])?prod(?:uction)?(?:[-_.,][\w.,=-]*)?\b.*\s(?:patch|replace)\b|.*\s(?:patch|replace)\b.*(?:-n|--namespace|--context|-l|--selector)[=\s]+(?:[\w.]*[-_.=])?prod(?:uction)?(?:[-_.,][\w.,=-]*)?\b)`), "production patch/replace"},
	{regexp.MustCompile(`\b(?:kubectl|oc)\b.*\bapply\b.*--prune\b`), "apply with prune (deletes resources)"},
	{regexp.MustCompile(`\bhelm\b.*\s(?:uninstall|delete|del|un)(?:\s|$)`), "uninstall a Helm release"},
	// Framework database resets that drop or wipe every table.
	{regexp.MustCompile(`\bprisma(?:@[\w.-]+)?\s+(?:--\s+)?migrate\s+reset\b|\bprisma(?:@[\w.-]+)?\s+(?:--\s+)?db\s+push\b.*--force-reset\b`), "reset a database through an ORM"},
	{regexp.MustCompile(`\b(?:rails|rake)\b.*\bdb:(?:drop|reset|purge|truncate_all|migrate:reset|schema:load|setup)\b`), "reset a database through an ORM"},
	{regexp.MustCompile(`\b(?:manage\.py|django-admin)\s+flush\b`), "reset a database through an ORM"},
	{regexp.MustCompile(`\bartisan\s+(?:migrate:fresh|migrate:reset|migrate:refresh|migrate:rollback|db:wipe)\b`), "reset a database through an ORM"},
	{regexp.MustCompile(`\bdotnet\s+ef\s+database\s+drop\b`), "reset a database through an ORM"},
	{regexp.MustCompile(`\bsequelize(?:-cli)?(?:@[\w.-]+)?\s+(?:--\s+)?db:drop\b`), "reset a database through an ORM"},
	{regexp.MustCompile(`\b(?:dropdb|dropuser)\b`), "drop a database"},
	{regexp.MustCompile(`(?i)\bmysqladmin\b.*\bdrop\b`), "drop a database"},
	// Infrastructure teardown.
	{regexp.MustCompile(`\b(?:terraform|tofu|terragrunt)\b.*\s(?:destroy\b|taint\b|apply\b.*\s-(?:destroy|replace)[=\s]|state\s+rm\b|workspace\s+delete\b)`), "destroy infrastructure"},
	{regexp.MustCompile(`\b(?:pulumi|cdk)(?:@[\w.-]+)?\b.*\s(?:destroy|down)\b|\bpulumi\b.*\sup\b.*--(?:target-)?replace\b`), "destroy infrastructure"},
	{regexp.MustCompile(`\bsam\s+delete\b|\b(?:serverless|sls)(?:@[\w.-]+)?\s+remove\b|\beksctl\s+delete\b`), "destroy infrastructure"},
	{regexp.MustCompile(`\bheroku\b.*\s(?:pg:reset|addons:destroy|apps:destroy)\b`), "reset or destroy a Heroku database, add-on or app"},
	{regexp.MustCompile(`\bssh\b.*(?:[\s'"]:\s*>|['"]\s*>\s*\S|\bcp\s+/dev/null\s)`), "overwrite a remote file"},
	{regexp.MustCompile(`\b(?:gcloud|az|doctl|hcloud)\b.*\s(?:delete|destroy|bulk-delete|delete-[\w-]+)(?:\s|$)`), "delete cloud resources"},
	{regexp.MustCompile(`\baws\b.*\s(?:s3\s+rb\b|s3\s+rm\b.*--recursive\b|s3\s+sync\b.*--delete\b|(?:batch-)?(?:delete|terminate|deregister)-[\w-]+\b|delete-objects\b)`), "delete cloud resources"},
}

// dataStorePatterns are matched against the whole line when any simple
// command in it runs the named client, so a query split across a quoted
// multi-statement string, a heredoc body or a pipe into the client is read.
var dataStorePatterns = []struct {
	clients map[string]bool
	body    *regexp.Regexp
	what    string
}{
	{map[string]bool{"psql": true, "mysql": true, "mariadb": true, "sqlite3": true, "sqlcmd": true,
		"clickhouse-client": true, "cockroach": true, "duckdb": true, "cqlsh": true, "pg:psql": true},
		regexp.MustCompile(`(?im)\bdrop\s+(?:database|schema|table|keyspace)\b|\btruncate\b|\bdelete\s+from\s+[\w."\\` + "`" + `\[\]]+\s*(?:;|$|['"])|\balter\s+table\s+\S+\s+drop\s+(?:column|constraint)\b|\bdelete\s+from\s+\S+\s+where\s+(?:true\b|1\s*=\s*1\b|1\s*(?:;|$|--|limit|['"]))`),
		"drop or empty a database or table"},
	{map[string]bool{"redis-cli": true}, regexp.MustCompile(`(?i)\bflush(?:all|db)\b`), "flush a Redis store"},
	{map[string]bool{"mongosh": true, "mongo": true},
		regexp.MustCompile(`dropDatabase|\.drop\(\s*\)|\.deleteMany\(\s*\{\s*\}\s*\)|\.remove\(\s*\{\s*\}\s*\)`),
		"drop or empty a Mongo database or collection"},
}

// fieldsOutsideQuotes splits on whitespace that sits outside single or double
// quotes, so a quoted argument (a commit message, a query) is one field; the
// quotes are then removed.
func fieldsOutsideQuotes(s string) []string {
	var out []string
	var b strings.Builder
	var q byte
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				b.WriteByte(c)
			}
		case c == '\'' || c == '"':
			q = c
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return out
}

// runsClient reports a line in which one of the given programs is the program
// word of a simple command — its first word, or the word after sudo/doas/su/env
// options, a package runner, or a docker/kubectl exec/run and its --.
func runsClient(command string, clients map[string]bool) bool {
	for _, part := range splitSimple(command) {
		// Split on whitespace outside quotes, so a quoted message or query stays
		// one token; quotes are then stripped from each token.
		words := fieldsOutsideQuotes(part)
		if len(words) == 0 {
			continue
		}
		// A bare echo/printf command prints its arguments, and a searcher's
		// operands are patterns, not programs; a client named there is text.
		first := strings.TrimSuffix(CommandName(path.Base(words[0])), ".exe")
		if first == "echo" || first == "printf" || searcherProg[first] && !searcherRuns(words[1:]) {
			continue
		}
		// A client counts when it is a whole word anywhere in the command,
		// except as the value of a message flag of a git-family program (so a
		// commit, tag or PR message naming a client is not read as running it).
		_, prog := simpleProgram(words)
		msgProg := msgProgram[prog]
		for i := 0; i < len(words); i++ {
			if msgProg && msgFlagWord(words[i]) {
				if !strings.Contains(words[i], "=") && (len(words[i]) <= 2 || !strings.HasPrefix(words[i], "-m")) {
					i++ // the message is the next word
				}
				continue
			}
			w := strings.TrimSuffix(CommandName(path.Base(words[i])), ".exe")
			if clients[w] {
				return true
			}
			// A quoted command line (ssh HOST '…', -c '…', fly -C '…') is read too.
			if strings.ContainsAny(words[i], " \t\n") && runsClient(words[i], clients) {
				return true
			}
		}
	}
	return false
}

// msgFlagWord reports a commit/tag/PR message flag, whose value is prose, not
// a command: -m/--message/--title/--body, their -am/-sm clusters and attached
// (-m"…", --message=…) forms.
func msgFlagWord(w string) bool {
	switch w {
	case "-m", "--message", "--title", "--body", "-am", "-sm", "-cm":
		return true
	}
	if strings.HasPrefix(w, "--message=") || strings.HasPrefix(w, "--title=") || strings.HasPrefix(w, "--body=") {
		return true
	}
	return len(w) > 2 && strings.HasPrefix(w, "-m") // -m"text" attached
}

// splitSimple splits a line into its simple commands on ;, &, |, ( ) and
// newlines that sit outside quotes, so a quoted query is kept whole.
func splitSimple(line string) []string {
	var parts []string
	var b strings.Builder
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quote != '\'' && i+1 < len(line):
			b.WriteByte(c)
			b.WriteByte(line[i+1])
			i++
			continue
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case strings.IndexByte(";&|()\n", c) >= 0:
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	return append(parts, b.String())
}

// segmentDestructive reports a segment pattern matching one simple command,
// or a data-store pattern matching a line that runs that client.
func segmentDestructive(command string) (string, bool) {
	for _, part := range splitSimple(command) {
		for _, d := range segmentPatterns {
			if d.re.MatchString(part) {
				return d.what, true
			}
		}
	}
	for _, d := range dataStorePatterns {
		if runsClient(command, d.clients) && d.body.MatchString(command) {
			return d.what, true
		}
	}
	return "", false
}

// Note on completeness: this list cannot be exhaustive. Shell affords endless
// ways to express deletion, and pattern matching on command text will always
// lag. That is precisely why the sandbox — not this list — is the actual
// boundary (docs/architecture/03-security.md). These patterns exist to make the
// common destructive cases require confirmation, not to be a security control
// anything depends on.

// IsDestructive reports whether a command needs confirmation regardless of
// permission mode. Exported so the policy engine can consult it.
func IsDestructive(command string) (string, bool) {
	return CanonicalCommand(command).Destructive()
}

// Destructive is IsDestructive for a command already canonicalised, so one
// policy decision parses it once.
func (c Canonical) Destructive() (string, bool) {
	return c.DestructiveWith(nil)
}

// DestructiveWith is Destructive with the git extensions the person opted in
// to (permissions.git_extensions) not taken as unreadable subcommands.
func (c Canonical) DestructiveWith(gitExtensions map[string]bool) (string, bool) {
	// Read for the whole command, as each part read alone may not show it.
	if len(gitExtensions) > 0 && aliasCapable(c.command) {
		gitExtensions = nil
	}
	// The command is read as written and as canonicalised; each only adds a match.
	command, canon := c.command, c.Text
	if what, ok := destructiveText(command, gitExtensions); ok {
		return what, true
	}
	if canon != command {
		if what, ok := destructiveText(canon, gitExtensions); ok {
			return what, true
		}
	}
	if c.IFS != "" {
		if what, ok := destructiveText(IFSSplitText(canon, c.IFS), gitExtensions); ok {
			return what, true
		}
	}
	if what := c.Unreadable(); what != "" {
		return what, true
	}
	// The commands the parser found, in function bodies and eval, trap and -c text.
	for _, cmd := range c.shell.commands {
		if what, ok := destructiveText(cmd, gitExtensions); ok {
			return what, true
		}
	}
	return "", false
}

// Unreadable says why the program a command runs, or the words it runs
// with, are known only when it runs, so no rule can read them; "" when they
// are read. Destructive counts each of these as needing a person's yes.
func (c Canonical) Unreadable() string {
	if c.IFSSplit {
		switch c.IFSCause {
		case IFSByUnparsed:
			return "a command that cannot be parsed, so where it sets IFS is not known"
		case IFSByBuilt:
			return "text built from an expansion that could set IFS"
		}
		return "words split by a changed IFS"
	}
	// The program is read in the text as written and as canonicalised, and by the parser.
	if what, ok := hiddenWords(c.command, c.Text); ok {
		return what
	}
	if what, ok := hiddenWords(c.command, c.command); ok {
		return what
	}
	if c.shell.edited || c.shell.expands {
		return "program named by an expansion"
	}
	if c.shell.opaque != "" {
		return c.shell.opaque
	}
	return c.shell.gitBuilt
}

func destructiveText(command string, gitExtensions map[string]bool) (string, bool) {
	raw := command
	// A commit/tag/PR message value and a searcher's patterns are text, not a
	// command: drop them before the patterns read the line.
	command = sanitizeData(command)
	// Where case is ignored, the program names are lowered too; that only adds a match.
	folded := foldProgramNames(command)
	for _, d := range destructivePatterns {
		if d.re.MatchString(command) || (folded != command && d.re.MatchString(folded)) {
			return d.what, true
		}
	}
	if rmForced(command) {
		return "recursive/forced delete", true
	}
	if what, ok := segmentDestructive(command); ok {
		return what, true
	}
	if folded != command {
		if what, ok := segmentDestructive(folded); ok {
			return what, true
		}
	}
	return gitDestructive(raw, gitExtensions)
}

// sanitizeData returns the line with each commit/tag/PR message value (for
// git/gh/glab/hg/jj/svn), each searcher's operands (grep/rg/ag/ack) and each
// inert sed script removed, so text that only names a deleter or a client is
// not matched. Searcher and sed text is data only when it is proven inert: no
// rg --pre or ack/ag --pager, no sed e command, s///e flag or w write, the
// output not redirected to a file and not piped into anything but a reader
// (head, sort, wc …), and no substitution or group in the line.
func sanitizeData(command string) string {
	out := command
	stages := splitStages(command)
	opaque := lineHasSubst(command)
	for k, st := range stages {
		part := st.text
		fields := fieldsOutsideQuotes(part)
		pi, first := simpleProgram(fields)
		if pi < 0 {
			continue
		}
		textOK := !opaque && !redirectsOut(part) && !feedsRunner(stages, k)
		var kept []string
		changed := false
		switch {
		case searcherProg[first]:
			if !textOK || searcherRuns(fields[pi+1:]) {
				continue
			}
			kept, changed = fields[:pi+1], len(fields) > pi+1
		case first == "sed" || first == "gsed":
			if !textOK {
				continue
			}
			kept, changed = sedWithoutScript(fields, pi)
		case msgProgram[first]:
			for i := 0; i < len(fields); i++ {
				if i > pi && msgFlagWord(fields[i]) {
					changed = true
					if !strings.Contains(fields[i], "=") && (len(fields[i]) <= 2 || !strings.HasPrefix(fields[i], "-m")) {
						i++
					}
					continue
				}
				kept = append(kept, fields[i])
			}
		}
		if changed && strings.Contains(out, part) {
			out = strings.Replace(out, part, " "+joinQuoted(kept)+" ", 1)
		}
	}
	return out
}

// msgProgram are the programs whose -m/--message/--title/--body value is prose.
var msgProgram = map[string]bool{"git": true, "gh": true, "glab": true, "hg": true, "svn": true, "jj": true}

// searcherProg are the searchers whose operands are patterns and paths.
var searcherProg = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true}

// pipeReader are programs that only read, filter or show what is piped to them.
var pipeReader = map[string]bool{"head": true, "tail": true, "wc": true, "sort": true, "uniq": true, "less": true,
	"more": true, "cut": true, "tr": true, "column": true, "nl": true, "cat": true}

// simpleProgram is the index and base name of a simple command's program, past
// sudo/doas/env, their options and NAME=value assignments; -1 when there is none.
func simpleProgram(fields []string) (int, string) {
	pi := 0
	for pi < len(fields) {
		b := strings.TrimSuffix(CommandName(path.Base(fields[pi])), ".exe")
		if b == "sudo" || b == "doas" || b == "env" || strings.HasPrefix(fields[pi], "-") || strings.Contains(fields[pi], "=") {
			if (fields[pi] == "-u" || fields[pi] == "-g") && pi+1 < len(fields) {
				pi++
			}
			pi++
			continue
		}
		return pi, b
	}
	return -1, ""
}

// searcherRuns reports a searcher option that runs a command: rg --pre, ack/ag --pager.
func searcherRuns(args []string) bool {
	for _, a := range args {
		if a == "--pre" || a == "--pager" || strings.HasPrefix(a, "--pre=") || strings.HasPrefix(a, "--pager=") {
			return true
		}
	}
	return false
}

// sedWithoutScript drops sed's script words when the script is all on the
// command line and only edits text; otherwise it reports no change.
func sedWithoutScript(fields []string, pi int) ([]string, bool) {
	args := fields[pi+1:]
	src, ok := sedscript.Scripts(args)
	if !ok {
		return nil, false
	}
	if s, err := sedscript.Parse(src); err != nil || !s.Inert() {
		return nil, false
	}
	kept := append([]string(nil), fields[:pi+1]...)
	sawExpr := false
	for _, a := range args {
		if a == "-e" || a == "--expression" || strings.HasPrefix(a, "--expression=") {
			sawExpr = true
		}
	}
	script := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-e" || a == "--expression") && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "--expression="):
		case !sawExpr && !script && !strings.HasPrefix(a, "-"):
			script = true
		default:
			kept = append(kept, a)
		}
	}
	return kept, true
}

// stage is one simple command of a line and whether a pipe sends its output on.
type stage struct {
	text  string
	piped bool
}

// splitStages splits a line like splitSimple, recording for each simple
// command whether a single | or |& (not ||) follows it.
func splitStages(line string) []stage {
	var out []stage
	var b strings.Builder
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quote != '\'' && i+1 < len(line):
			b.WriteByte(c)
			b.WriteByte(line[i+1])
			i++
			continue
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case strings.IndexByte(";&|()\n", c) >= 0:
			piped := false
			if c == '|' {
				if i+1 < len(line) && line[i+1] == '|' {
					i++
				} else {
					piped = true
					if i+1 < len(line) && line[i+1] == '&' {
						i++
					}
				}
			} else if c == '&' && i+1 < len(line) && line[i+1] == '&' {
				i++
			}
			out = append(out, stage{text: b.String(), piped: piped})
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	return append(out, stage{text: b.String()})
}

// feedsRunner reports a stage whose output is piped on to anything but a
// reader (or a searcher that runs nothing).
func feedsRunner(stages []stage, k int) bool {
	for j := k; j+1 < len(stages) && stages[j].piped; j++ {
		fields := fieldsOutsideQuotes(stages[j+1].text)
		pi, prog := simpleProgram(fields)
		if pi < 0 {
			return true
		}
		if pipeReader[prog] {
			continue
		}
		if searcherProg[prog] && !searcherRuns(fields[pi+1:]) {
			continue
		}
		return true
	}
	return false
}

// lineHasSubst reports a command or process substitution, a subshell or a
// brace group anywhere in the line, where a searcher's output could reach a
// runner without a plain pipe.
func lineHasSubst(line string) bool {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quote != '\'' && i+1 < len(line):
			i++
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '`' || (c == '$' && i+1 < len(line) && line[i+1] == '('):
			return true
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == ')' || c == '{' || c == '}':
			return true
		}
	}
	return false
}

// redirectsOut reports a simple command whose output is redirected to a file
// (other than /dev/null or another descriptor).
func redirectsOut(part string) bool {
	var quote byte
	for i := 0; i < len(part); i++ {
		c := part[i]
		switch {
		case c == '\\' && quote != '\'' && i+1 < len(part):
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '>':
			j := i + 1
			for j < len(part) && (part[j] == '>' || part[j] == '|') {
				j++
			}
			if j < len(part) && part[j] == '&' {
				i = j
				continue // >&2: another descriptor
			}
			for j < len(part) && (part[j] == ' ' || part[j] == '\t') {
				j++
			}
			if !strings.HasPrefix(part[j:], "/dev/null") {
				return true
			}
			i = j
		}
	}
	return false
}

// rmForced finds rm's recursive or force flags anywhere among its words, as
// GNU rm reads them: after operands, and long options by any prefix.
func rmForced(command string) bool {
	for _, part := range strings.Split(rmBreaks.Replace(command), "\n") {
		words := strings.Fields(shellQuotes.Replace(part))
		for i, w := range words {
			if strings.TrimSuffix(CommandName(path.Base(w)), ".exe") != "rm" {
				continue
			}
			for _, a := range words[i+1:] {
				if a == "--" {
					break
				}
				if rmArgForces(a) {
					return true
				}
			}
		}
	}
	return false
}

// rmBreaks splits commands but keeps backticks in their word, so a
// substitution among rm's arguments is seen as one.
var rmBreaks = strings.NewReplacer(";", "\n", "&", "\n", "|", "\n", "(", "\n", ")", "\n")

// rmArgForces reports whether one of rm's arguments is, or may expand to, a
// recursive or force flag. A $ or backtick is a value not known until it runs.
func rmArgForces(a string) bool {
	switch {
	case strings.ContainsAny(a, "$`"):
		return true
	case strings.HasPrefix(a, "--"):
		// getopt_long takes any unambiguous prefix: --rec is --recursive.
		name, _, _ := strings.Cut(a, "=")
		if len(name) < 3 {
			return false
		}
		for _, full := range []string{"--recursive", "--force"} {
			if strings.HasPrefix(full, name) {
				return true
			}
		}
		return false
	case strings.HasPrefix(a, "-"):
		return strings.ContainsAny(a[1:], "rRf")
	}
	return false
}

func (b Bash) Run(ctx context.Context, s *Session, raw json.RawMessage) Result {
	res := b.run(ctx, s, raw)
	res.Tier = b.ranTier()
	return res
}

// ranTier is the tier a command ran under, known once the sandbox is chosen.
func (b Bash) ranTier() string {
	if b.Sandbox != nil && b.RanUnder != nil {
		return b.RanUnder()
	}
	return b.tier()
}

// tier is the sandbox tier commands run under, "none" without one, and ""
// when a sandbox is set but not described.
func (b Bash) tier() string {
	if b.Sandbox == nil {
		return "none"
	}
	return b.Isolation.Tier
}

func (b Bash) run(ctx context.Context, s *Session, raw json.RawMessage) Result {
	var a bashArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for bash: %v", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return errf("command is required.")
	}
	if strings.TrimSpace(a.Description) == "" {
		// Derive one rather than failing. The description exists to label the
		// approval prompt, and refusing the call over a missing label was a
		// real dead end: a model omitted it twice, got the same rejection
		// twice, and abandoned the task instead of running the command.
		// A weaker label is far better than no command.
		a.Description = summarizeCommand(a.Command)
	}

	if isInteractive(a.Command) {
		return errf("Refusing to run an interactive command: %s\nInteractive commands wait for a terminal that is not attached and will hang.\nUse a non-interactive equivalent (for example `git rebase --onto` instead of `git rebase -i`, or `cat` instead of `less`).", a.Command)
	}

	if len(a.Secrets) > 0 && b.Secrets == nil {
		return errf("No secrets are configured on this deployment; run the command without `secrets`.")
	}
	if a.Background {
		// Its changes are never checkpointed, which "required" does not allow.
		if s.BashCheckpoints.Mode == BashCheckpointRequired {
			return errf("Refused: tools.bash_checkpoints is \"required\", and a command run in the background is not checkpointed. Run it in the foreground. Nothing was run.")
		}
		return b.background(ctx, s, a)
	}

	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	if a.TimeoutMS <= 0 {
		timeout = defaultTimeoutMS * time.Millisecond
	}
	if timeout > maxTimeoutMS*time.Millisecond {
		timeout = maxTimeoutMS * time.Millisecond
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The command lives on procCtx, which ends with the call unless the
	// person moves the command to the background first.
	detach, host := DetachOf(ctx), ShellHostOf(ctx)
	if host == nil {
		detach = nil
	}
	procCtx, endProc := context.WithCancel(context.WithoutCancel(ctx))
	// One swap decides who owns the command: the timeout ends it, or Ctrl-B moves it.
	const ownedByTimeout, ownedByMove = 1, 2
	var owner atomic.Int32
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		<-runCtx.Done()
		if owner.CompareAndSwap(0, ownedByTimeout) {
			endProc()
		}
	}()
	var move <-chan struct{}
	var started func()
	if detach != nil {
		moved := make(chan struct{})
		move = moved
		go func() {
			select {
			case <-detach.Move:
				if owner.CompareAndSwap(0, ownedByMove) {
					close(moved)
				}
			case <-finished:
			}
		}()
		started = func() { detach.running.Store(true) }
		defer detach.running.Store(false)
	}

	snap, err := s.beforeBash(ctx)
	if err != nil {
		endProc()
		return errf("%v", err)
	}
	cmd, err := b.command(procCtx, s.Cwd, a)
	if err != nil {
		endProc()
		return errf("%v", err)
	}

	output, err := newBashOutput(cmd)
	if err != nil {
		sandbox.Release(cmd)
		endProc()
		return errf("Failed to run command: %v", err)
	}
	start := time.Now()
	content, truncated, held, detached, err := output.run(cmd, bashOutputWait, started, move)
	elapsed := time.Since(start)
	// What it changed up to here; once moved to the background, nothing more.
	snapNote := s.afterBash(snap)
	if detached {
		res := b.moveToBackground(ctx, host, output, cmd, endProc, a, start)
		if snapNote != "" {
			res.Content = withNote(res.Content, snapNote)
		}
		return res
	}
	endProc()
	res := b.finish(ctx, runCtx, s, a, content, truncated, held, err, elapsed, timeout)
	if snapNote != "" {
		res.Content = withNote(res.Content, snapNote)
	}
	return res
}

// finish turns a foreground command's end into its result.
func (b Bash) finish(ctx, runCtx context.Context, s *Session, a bashArgs, content string, truncated, held bool, err error, elapsed, timeout time.Duration) Result {

	// The run's own limit, not this call's: raising timeout_ms would not help.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{Content: "Command stopped: the run's time limit passed.\n" + content, IsError: true, Truncated: truncated}
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return Result{
			Content: fmt.Sprintf("Command timed out after %s.\n%s\n\nIf this command is expected to run long, raise timeout_ms (max %d).",
				timeout, content, maxTimeoutMS),
			IsError:   true,
			Truncated: truncated,
		}
	}
	if ctx.Err() != nil {
		return Result{Content: "Command cancelled.\n" + content, IsError: true}
	}

	exitCode := 0
	var bgNote string
	if held {
		bgNote = "[a process it started is still running; its later output is not shown]"
	}
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			exitCode = ExitStatus(ee)
		} else {
			return errf("Failed to run command: %v", err)
		}
	}

	// A call that is just `cd <folder>` and succeeded moves later calls; the model
	// is told when a cd was not followed, so it knows where the next call starts.
	var cdNote string
	if exitCode == 0 {
		cdNote = s.FollowCd(a.Command)
	}

	noOutput := content == ""
	if noOutput {
		content = "[no output]"
	}
	if bgNote != "" {
		content = withNote(content, bgNote)
	}
	if cdNote != "" {
		content = withNote(content, cdNote)
	}

	// A non-zero exit is a valid observation the model must reason about, not a
	// tool failure. Never convert a failing test run into an error.
	header := fmt.Sprintf("exit %d · %s", exitCode, elapsed.Round(time.Millisecond))
	note := ""
	// On the host the same text is the operating system's refusal, not a sandbox's.
	// Any exit code: a pipeline's last command can succeed after curl failed.
	if missing := b.notFoundNote(s.Cwd, content, exitCode); missing != "" {
		content = withNote(content, missing)
	} else if b.network() == networkOff && (networkFailed(a.Command, content, exitCode) || exitCode != 0 && proxyRefused(content)) {
		// With the network off a 403 is the egress proxy's: no registry is open yet.
		content = withNote(content, b.networkNote(a.Command, content))
	} else if kind, hint := sandboxHint(content); hint != "" && exitCode != 0 && b.ranTier() != "none" {
		// A command that succeeded was not stopped: git warns of .abhed and still exits 0.
		content = withNote(content, hint)
		note = kind
	}
	return Result{
		Content:     header + "\n" + content,
		Truncated:   truncated,
		ExitCode:    &exitCode,
		NoOutput:    noOutput,
		SandboxNote: note,
	}
}

// notFoundRE finds a program the shell could not find: bash's "go: command
// not found", dash's "sh: 1: go: not found", env's "env: 'node': No such file".
var notFoundRE = regexp.MustCompile(`(?m)(?:^|: )'?([A-Za-z0-9_.+-]{1,40})'?: (?:command not found|not found|No such file or directory)\s*$`)

// notFoundNote is what NotFound says of the first program a failed command
// could not find, or "".
func (b Bash) notFoundNote(dir, content string, exitCode int) string {
	if b.NotFound == nil || exitCode == 0 {
		return ""
	}
	for _, m := range notFoundRE.FindAllStringSubmatch(content, 8) {
		if note := b.NotFound(dir, m[1]); note != "" {
			return note
		}
	}
	return ""
}

// withNote puts a note after the output with one blank line between, however the output ends.
func withNote(content, note string) string {
	return strings.TrimRight(content, "\n") + "\n\n" + note
}

// command makes the process for a call: under the sandbox when there is
// one, with the secrets it asked for, ending its whole group when ctx ends.
func (b Bash) command(ctx context.Context, cwd string, a bashArgs) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if b.Sandbox != nil {
		cmd = b.Sandbox(ctx, cwd, a.Command)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", a.Command)
		cmd.Dir = cwd
		// No sandbox: the server's own environment, less the variables hostDropped names.
		cmd.Env = append(sandbox.HostCommandEnv(), "ABHED_SESSION=1")
	}
	// A stopped turn ends what the command started, not only its shell.
	sandbox.EndWithCommand(cmd)

	// git never runs a command a repository's own config names for ext:: or fsmonitor.
	var gitNames []string
	cmd.Env, gitNames = WithGitSafeConfig(cmd.Env)
	b.forward(cmd, gitNames)

	if len(a.Secrets) > 0 {
		env, err := b.Secrets(a.Secrets)
		if err != nil {
			sandbox.Release(cmd)
			return nil, err
		}
		cmd.Env = append(cmd.Env, env...)
		names := make([]string, 0, len(env))
		for _, kv := range env {
			if k, _, ok := strings.Cut(kv, "="); ok {
				names = append(names, k)
			}
		}
		b.forward(cmd, names)
	}
	return cmd, nil
}

// forward gives a container the variables named, which it sees only by name.
func (b Bash) forward(cmd *exec.Cmd, names []string) {
	if b.Forward != nil {
		b.Forward(cmd, names)
	} else {
		sandbox.ForwardEnv(cmd, names)
	}
}

// moveToBackground hands a running foreground command to the session's host
// as a background shell, as the person asked with Ctrl-B, and returns what it
// wrote so far. Refused, the command is ended, as the call ends.
func (b Bash) moveToBackground(ctx context.Context, host ShellHost, o *bashOutput, cmd *exec.Cmd, end context.CancelFunc, a bashArgs, start time.Time) Result {
	proc, out, truncated := o.adopt(cmd, end, 0)
	tier := b.tier()
	if b.Sandbox != nil && b.RanUnder != nil {
		tier = b.RanUnder()
	}
	id, err := host.StartShell(ctx, ShellRequest{Command: a.Command, Description: a.Description, Secrets: a.Secrets,
		Tier: tier, Proc: proc, Started: start})
	if err != nil {
		proc.Stop()
		<-proc.Done()
		return errf("Could not move the command to the background, so it was stopped: %v\n%s", err, out)
	}
	if out == "" {
		out = "[no output yet]"
	}
	return Result{Content: BackgroundStarted(id, a.Description) + "\nThe user moved this command to the background while it ran (Ctrl-B); " +
		"it goes on. Its output so far:\n" + out + "\n\nRead its later output with shell_output, stop it with shell_kill. " +
		"You are told when it ends; do not poll in a loop.", Truncated: truncated, Tier: tier}
}

// background starts the command on the session's host and returns its id at
// once. Policy, approval and the sandbox are the same as in the foreground.
func (b Bash) background(ctx context.Context, s *Session, a bashArgs) Result {
	host := ShellHostOf(ctx)
	if host == nil {
		return errf("Background commands are not available here; run the command without run_in_background.")
	}
	var timeout time.Duration
	if a.TimeoutMS > 0 {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
	}
	var ranUnder func() string
	if b.Sandbox != nil {
		ranUnder = b.RanUnder // read after Build: before it, the tier may not be chosen yet
	}
	cwd := s.Cwd
	id, err := host.StartShell(ctx, ShellRequest{
		Command: a.Command, Description: a.Description, Secrets: a.Secrets, Timeout: timeout, Tier: b.tier(), RanUnder: ranUnder,
		Build: func(sctx context.Context) (*exec.Cmd, error) { return b.command(sctx, cwd, a) },
	})
	if err != nil {
		return errf("Could not start the command in the background: %v", err)
	}
	return Result{Content: BackgroundStarted(id, a.Description) + "\nRead its output with shell_output, stop it with shell_kill. " +
		"You are told when it ends; do not poll in a loop."}
}

// BackgroundStarted is the first line of a background start's result, which
// the surfaces show as the call's one line.
func BackgroundStarted(id, description string) string {
	return "Started in background: " + id + " · " + description
}

type networkState int

const (
	networkUnknown networkState = iota
	networkOff
	networkOn
	networkAllowlist
)

// network is whether commands can reach the network, as far as this bash
// knows: on the host they can, and a sandbox says; one it does not describe
// is not guessed at.
func (b Bash) network() networkState {
	switch t := b.tier(); {
	case t == "none":
		return networkOn
	case t == "":
		return networkUnknown
	case b.Isolation.Allowlist:
		return networkAllowlist
	case b.Isolation.Network:
		return networkOn
	}
	return networkOff
}

// maxNamedHosts bounds the hosts the description names.
const maxNamedHosts = 20

// allowlistText tells the model where its commands may go under the
// allowlist, and how a refusal looks.
func (b Bash) allowlistText() string {
	d := "The network is limited to the destinations your organisation's allowlist permits, reached through the session's egress proxy, which HTTP_PROXY and HTTPS_PROXY already name: tools that honour them (curl, wget, git, pip, npm, go) work; anything that ignores them, or connects to an address directly, has no route out. "
	hosts := b.Isolation.AllowedHosts
	switch {
	case len(hosts) == 0 && !b.Isolation.DefaultAllow:
		d += "No destination is allowed at present. "
	case len(hosts) > 0:
		named := hosts
		if len(named) > maxNamedHosts {
			named = named[:maxNamedHosts]
		}
		d += "Allowed: " + strings.Join(named, ", ")
		if len(hosts) > len(named) {
			d += fmt.Sprintf(" and %d more", len(hosts)-len(named))
		}
		d += " (some only on certain ports, methods or paths). "
	}
	if b.Isolation.DefaultAllow {
		d += "Other hosts are allowed unless a rule denies them. "
	}
	d += "A refused destination answers 403 from the proxy with the rule that refused it; do not try to work around it, tell the user what you needed."
	return d
}

// networkFailures are what common clients print when a name does not resolve
// or no route exists: what a command sees with the network cut. A refused
// connection is left out, since a server that is not running on loopback,
// which the sandbox keeps, says the same.
var networkFailures = regexp.MustCompile(`(?i)could not resolve host|could not resolve proxy|` +
	`temporary failure in name resolution|name or service not known|` +
	`nodename nor servname provided|no address associated with hostname|` +
	`network is unreachable|no route to host|` +
	`getaddrinfo (?:enotfound|eai_again)|\beai_again\b|\benotfound\b|` +
	`dial tcp: lookup [^ ]+|failed to establish a new connection|` +
	`unable to access 'https?://|could not resolve hostname|` +
	`temporary failure resolving|failed to resolve (?:host|address|hostname|name)|unable to resolve host address`)

// failedConnect is curl's message for an address it could not reach, which
// counts unless the address is loopback.
var failedConnect = regexp.MustCompile(`(?i)failed to connect to \[?([^\s\]]+)\]? port`)

func networkFailure(output string) bool {
	if networkFailures.MatchString(output) {
		return true
	}
	for _, m := range failedConnect.FindAllStringSubmatch(output, -1) {
		h := strings.ToLower(m[1])
		if h != "localhost" && !strings.HasPrefix(h, "127.") && h != "::1" {
			return true
		}
	}
	return false
}

// networkClients are programs that reach for the network, where a command
// starts.
var networkClients = regexp.MustCompile(`(?:^|[;|&(]|\$\()\s*(?:sudo\s+|env\s+|command\s+|exec\s+)?` +
	`(?:curl|wget|git\s+(?:fetch|clone|pull|push|ls-remote|submodule)|npm|npx|yarn|pnpm|pip3?|python3?|uv|` +
	`go\s+(?:get|install|mod)|cargo|gem|bundle|composer|apt(?:-get)?|apk|brew|ssh|scp|sftp|rsync|nc|ncat|` +
	`ping|dig|nslookup|host|http|node|deno|bun|docker|podman|helm|kubectl|aws|gcloud|az)\b`)

// versionOnly is a command word followed only by a version or help option,
// up to the end of its command.
var versionOnly = regexp.MustCompile(`\b[\w.-]+\s+(?:--version|-V|--help|-h|version)\s*(?:$|[;|&)\n])`)

// networkFailed reports whether a command failed for want of the network. A
// command that succeeded counts only when a network client ran and the failure
// is in its last lines: a log being read can mention a network error.
func networkFailed(command, output string, exitCode int) bool {
	if exitCode != 0 {
		return networkFailure(output)
	}
	// A client asked only for its version or help reaches nothing: with it
	// last, `cat log; curl --version` put the log's lines among its last.
	if !networkClients.MatchString(versionOnly.ReplaceAllString(command, ";")) {
		return false
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return networkFailure(strings.Join(lines, "\n"))
}

// networkNote is what a command that could not reach the network is told:
// how to open the package registry it was refused, where it names one.
func (b Bash) networkNote(command, output string) string {
	if b.RegistryNote != nil {
		if n := b.RegistryNote(command, output); n != "" {
			return n
		}
	}
	return strings.TrimSpace(networkHint + " " + b.NetworkNote)
}

// proxyPhrases are what clients print only for a proxy's refusal: the
// egress proxy's own text, and curl's, pip's, Go's, npm 9+'s and uv's for a
// refused CONNECT.
var proxyPhrases = regexp.MustCompile(`(?i)abhed egress:|tunnel connection failed: 403|received HTTP code 403 from proxy|` +
	`CONNECT tunnel failed, response 403|proxyconnect tcp|invalid status code .?403.? connecting to proxy|unsuccessful tunnel`)

// forbiddenLine is a 403 a client reports for a URL: Go's `Get "https://…": Forbidden`, npm's `403 Forbidden - GET https://…`,
// Gradle's `Could not GET 'https://…'. Received status code 403 from server: Forbidden`.
var forbiddenLine = regexp.MustCompile(`(?i)(?:": Forbidden\b|\b403 Forbidden\b|\bE403\b|status code 403 from server)`)

// httpsHost is the host of an https URL in a line.
var httpsHost = regexp.MustCompile(`https://([^\s/"':]+)`)

// proxyRefused reports output from the session's egress proxy refusing a
// client: its own phrases, or a 403 on a line naming a remote https URL, as
// the proxy refuses those; a local server's 403 is not it.
func proxyRefused(output string) bool {
	if proxyPhrases.MatchString(output) {
		return true
	}
	for _, line := range strings.Split(output, "\n") {
		if !forbiddenLine.MatchString(line) {
			continue
		}
		for _, m := range httpsHost.FindAllStringSubmatch(line, -1) {
			if h := strings.ToLower(m[1]); h != "localhost" && !strings.HasPrefix(h, "127.") && h != "[" && h != "0.0.0.0" && strings.Contains(h, ".") {
				return true
			}
		}
	}
	return false
}

// networkHint says why a command that reached for the network failed. Without
// it a model that ran curl sees only "could not resolve host", and neither it
// nor the user learns that the sandbox, not the site, is the reason.
const networkHint = "NOTE: this sandbox has no network access, so the command could not reach " +
	"the host. Retrying will fail the same way. To read from the web, use web_search or " +
	"web_fetch if they are in your tools; otherwise tell the user that commands here " +
	"cannot reach the network."

// sandboxHint explains a failure the sandbox caused, and names what to do
// instead.
//
// Without this the model sees only "operation not permitted" from a tool it
// shelled out to, concludes the file is unreadable, and gives up — which is
// what happened when a user approved `oc login` and got nothing: the approval
// let the COMMAND run, and the sandbox separately denied the credential read.
// Two layers, one confusing message.
//
// The failure is worth catching precisely because the agent usually has a
// better route: a native tool that holds the credential outside the sandbox.
//
// kind is the folder named ("kube", "ssh", "aws", "gnupg"), "credentials"
// for another of sandbox.HomeSecrets, or "abhed" for Abhed's own state, which
// the sandbox tiers deny; or "denied" when the output names none of them.
func sandboxHint(output string) (kind, hint string) {
	if !strings.Contains(output, "operation not permitted") &&
		!strings.Contains(output, "Operation not permitted") {
		return "", ""
	}
	for _, c := range []struct{ path, hint string }{
		{".kube", "Abhed's sandbox blocks reads of ~/.kube, so kubectl and oc cannot " +
			"authenticate from inside it. Use the k8s_get tool instead, which holds the " +
			"credential outside the sandbox. If k8s_get is not available, this deployment " +
			"has not enabled cluster access — tell the user to set k8s.enabled in their config."},
		{".ssh", "Abhed's sandbox blocks reads of ~/.ssh, so ssh and git-over-ssh cannot " +
			"authenticate from inside it. Use the ssh tool instead, which uses the agent or " +
			"a configured key. If it is not available, tell the user to configure ssh.hosts."},
		{".aws", "Abhed's sandbox blocks reads of ~/.aws, so the AWS CLI cannot authenticate " +
			"from inside it. Ask the user to run this command themselves, or to expose the " +
			"capability through an MCP server."},
		{".gnupg", "Abhed's sandbox blocks reads of ~/.gnupg. Ask the user to run any " +
			"signing step themselves."},
	} {
		if strings.Contains(output, c.path) {
			return strings.TrimPrefix(c.path, "."), "NOTE: " + c.hint + " Do not retry this command; it will fail the same way."
		}
	}
	kind = "denied"
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, c := range sandbox.HomeSecrets {
			if strings.Contains(output, filepath.Join(home, c)) || strings.Contains(output, "~/"+c) {
				kind = "credentials"
			}
		}
	}
	if strings.Contains(output, ".abhed") {
		kind = "abhed" // Abhed's own state, which every tier denies
	}
	return kind, "NOTE: the sandbox denied this operation. Retrying the same command will " +
		"fail identically — either use a native Abhed tool for this, or tell the user " +
		"what needs to change."
}

// summarizeCommand builds a short label for the approval prompt from the
// command itself, for when the model did not supply one.
func summarizeCommand(command string) string {
	line := strings.TrimSpace(strings.SplitN(command, "\n", 2)[0])
	if len(line) > 80 {
		line = line[:80] + "…"
	}
	return "Run: " + line
}

func asExitError(err error, target **exec.ExitError) bool {
	ee := &exec.ExitError{}
	ok := errors.As(err, &ee)
	if ok {
		*target = ee
	}
	return ok
}

// FollowCd moves the session's working directory the way a run of command
// through the bash tool would, for a runner that executes commands itself.
// It returns why a cd was not followed, or "" when it was or there was none.
func (s *Session) FollowCd(command string) string {
	next, why := detectCd(command, s)
	if next != "" {
		s.mu.Lock()
		s.Cwd = next
		s.mu.Unlock()
	}
	return why
}

// detectCd says where a line that is just `cd <folder>` leaves the shell, so
// the next call starts there. Any other line that could change directory is
// not followed, and says so: guessing at the rest of a line went wrong.
func detectCd(command string, s *Session) (next, why string) {
	line := strings.Trim(strings.TrimSuffix(strings.Trim(command, " \t"), "\n"), " \t")
	stays := "; still in " + s.Rel(s.Cwd)
	unfollowed := "cd: not followed: only a line that is just `cd <folder>` is" + stays
	// A bare cd goes home, and home here is the workspace root.
	if line == "cd" {
		return s.Root, ""
	}
	if !IsCdLine(line) {
		if mayChangeDir(line) {
			return "", unfollowed
		}
		return "", ""
	}
	arg := strings.Trim(line[2:], " \t")
	if arg == "~" {
		return s.Root, ""
	}
	// One word, with nothing after it: shellWord refuses separators and blanks.
	target, ok := shellWord(arg)
	if !ok || target == "" || strings.HasPrefix(target, "-") {
		return "", unfollowed
	}
	if !strings.HasPrefix(target, "/") {
		target = s.Cwd + "/" + target
	}
	resolved, err := s.Resolve(target)
	if err != nil && s.isState(filepath.Clean(target)) {
		return "", "cd: " + arg + " is Abhed's own state, out of reach" + stays
	}
	if err != nil {
		return "", "cd: " + arg + " is outside the workspace" + stays
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", "cd: no such directory: " + arg + stays
	}
	return resolved, ""
}

// changesDir finds a word that could change the shell's directory: cd, pushd,
// popd, eval, source or CDPATH anywhere, or . as a command. It is matched with
// quotes and backslashes taken out, so "cd", 'cd' and c\d count as cd.
var changesDir = regexp.MustCompile("(?:^|[\\s;&|(){}`!])(?:cd|pushd|popd|eval|source)(?:$|[\\s;&|(){}`])|CDPATH|(?:^|[;&|(){}`!])\\s*\\.(?:$|\\s)")

// unquoted drops a line's quotes and backslashes, for changesDir.
var unquoted = strings.NewReplacer(`"`, "", `'`, "", `\`, "")

// mayChangeDir reports a line that runs a command able to change directory, read
// by the parser so that `echo cd` or a comment is not one; a line it cannot read falls back to changesDir.
func mayChangeDir(line string) bool {
	if strings.Contains(line, "CDPATH") {
		return true
	}
	var file *syntax.File
	var err error
	if len(line) <= maxShellBytes {
		file, err = syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(line), "")
	}
	if file == nil || err != nil {
		return changesDir.MatchString(unquoted.Replace(line))
	}
	found := false
	syntax.Walk(file, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || found {
			return !found
		}
		for _, w := range call.Args {
			name, lit := literal(w)
			if !lit || (name != "builtin" && name != "command") {
				found = lit && dirWords[name]
				break
			}
		}
		return !found
	})
	return found
}

// dirWords are the commands that can change the shell's directory.
var dirWords = map[string]bool{"cd": true, "pushd": true, "popd": true, "eval": true, "source": true, ".": true}

// IsCdLine reports a line that is the cd command with an argument: cd, then
// a space or a tab, the only blanks bash splits a line's words on.
func IsCdLine(line string) bool {
	return len(line) > 2 && line[:2] == "cd" && (line[2] == ' ' || line[2] == '\t')
}

// IsCdWord reports a cd line whose argument is one word read exactly, its
// quotes and escapes undone, such as the `cd a\ \$5/` Tab completion writes.
func IsCdWord(line string) bool {
	if !IsCdLine(line) {
		return false
	}
	w, ok := shellWord(strings.Trim(line[2:], " \t"))
	return ok && w != ""
}

// shellWord reads s as one bash word and undoes its quoting: backslashes,
// '...', "..." and $'...'. It reports false for anything it would have to
// guess at: an expansion, a glob, an unfinished quote or several words.
func shellWord(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			// A backslash before a newline joins lines; that is not read here.
			if i++; i == len(s) || s[i] == '\n' {
				return "", false
			}
			b.WriteByte(s[i])
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			// Only the ANSI-C escapes that stand for the character itself; bash
			// keeps the backslash of one it does not know, such as "\ ".
			for i += 2; ; i++ {
				if i == len(s) {
					return "", false
				}
				if s[i] == '\'' {
					break
				}
				if s[i] == '\\' {
					if i+1 == len(s) || !strings.ContainsRune(`\'"?`, rune(s[i+1])) {
						return "", false
					}
					i++
				}
				b.WriteByte(s[i])
			}
		case c == '"':
			for i++; ; i++ {
				if i == len(s) {
					return "", false
				}
				if s[i] == '"' {
					break
				}
				if s[i] == '$' || s[i] == '`' || s[i] == '\\' && i+1 < len(s) && s[i+1] == '\n' {
					return "", false
				}
				if s[i] == '\\' && i+1 < len(s) && strings.ContainsRune("$`\"\\", rune(s[i+1])) {
					i++
				}
				b.WriteByte(s[i])
			}
		case strings.IndexByte(" \t\n\r\v\f$`*?[{;&|<>()!#", c) >= 0, c == '~' && i == 0:
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// CommandHome is the HOME the registry's bash tool gives a command, as the
// policy reads start-up files from it: the host's when commands run directly.
func CommandHome(reg *Registry) func() (string, bool) {
	return func() (string, bool) {
		t, _ := reg.Get("bash")
		b, ok := t.(Bash)
		switch {
		case ok && b.Home != nil:
			return b.Home()
		case ok && b.Sandbox != nil:
			return "", false
		}
		h := os.Getenv("HOME")
		return h, h != ""
	}
}
