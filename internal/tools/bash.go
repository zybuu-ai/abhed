package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
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
// In production this executes inside the session microVM (docs §03-security).
// The guards here are defense in depth, not the boundary: they catch the
// obvious footguns early and give the model a useful message, but isolation is
// what actually contains a hostile command.
type Bash struct {
	// Sandbox, when set, wraps the command (e.g. a microVM or container exec).
	// Nil means direct execution, which is only appropriate for local dev.
	Sandbox func(ctx context.Context, cwd, command string) *exec.Cmd
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
}

// Isolation describes the sandbox in force: its tier, the mechanism that
// provides it, and whether commands can reach the network.
type Isolation struct {
	Tier    string `json:"tier"`
	Backend string `json:"backend"`
	Network bool   `json:"network"`
}

func (Bash) Name() string  { return "bash" }
func (Bash) Mutates() bool { return true }

func (b Bash) Description() string {
	d := "Run a shell command in the session workspace. Use for builds, tests, git, and package managers. Prefer read/glob/grep for file inspection — they are cheaper and safer. Note: a call that is just `cd <folder>` sets the working directory for later calls; shell state (variables, functions) and a cd inside a longer command do not carry over."
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
    "secrets":{"type":"array","items":{"type":"string"},"description":"Names of stored secrets this command needs as environment variables. Each must be permitted by policy."}
  },
  "required":["command","description"]
}`)
}

type bashArgs struct {
	Command     string   `json:"command"`
	Description string   `json:"description"`
	TimeoutMS   int      `json:"timeout_ms"`
	Secrets     []string `json:"secrets"`
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
	for _, d := range destructivePatterns {
		if d.re.MatchString(command) {
			return d.what, true
		}
	}
	return "", false
}

func (b Bash) Run(ctx context.Context, s *Session, raw json.RawMessage) Result {
	res := b.run(ctx, s, raw)
	res.Tier = b.tier()
	return res
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

	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	if a.TimeoutMS <= 0 {
		timeout = defaultTimeoutMS * time.Millisecond
	}
	if timeout > maxTimeoutMS*time.Millisecond {
		timeout = maxTimeoutMS * time.Millisecond
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if b.Sandbox != nil {
		cmd = b.Sandbox(runCtx, s.Cwd, a.Command)
	} else {
		cmd = exec.CommandContext(runCtx, "bash", "-c", a.Command)
		cmd.Dir = s.Cwd
		// No sandbox: the server's own environment, less BASH_ENV and CDPATH.
		cmd.Env = append(sandbox.HostCommandEnv(), "ABHED_SESSION=1")
	}
	// A stopped turn ends what the command started, not only its shell.
	sandbox.EndWithCommand(cmd)

	if len(a.Secrets) > 0 {
		if b.Secrets == nil {
			return errf("No secrets are configured on this deployment; run the command without `secrets`.")
		}
		env, err := b.Secrets(a.Secrets)
		if err != nil {
			return errf("%v", err)
		}
		cmd.Env = append(cmd.Env, env...)
	}

	output, err := newBashOutput(cmd)
	if err != nil {
		return errf("Failed to run command: %v", err)
	}
	start := time.Now()
	content, held, err := output.run(cmd, bashOutputWait)
	elapsed := time.Since(start)

	full := len(content)
	truncated := false
	if len(content) > maxOutputChars {
		// Keep head and tail: the command's intent is at the start, the error
		// is almost always at the end.
		head := content[:maxOutputChars/2]
		tail := content[len(content)-maxOutputChars/2:]
		content = fmt.Sprintf("%s\n\n[... %d characters truncated ...]\n\n%s",
			head, full-maxOutputChars, tail)
		truncated = true
	}

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

	if content == "" {
		content = "[no output]"
	}
	if bgNote != "" {
		content += "\n\n" + bgNote
	}
	if cdNote != "" {
		content += "\n\n" + cdNote
	}

	// A non-zero exit is a valid observation the model must reason about, not a
	// tool failure. Never convert a failing test run into an error.
	header := fmt.Sprintf("exit %d · %s", exitCode, elapsed.Round(time.Millisecond))
	// On the host the same text is the operating system's refusal, not a sandbox's.
	if hint := sandboxHint(content); hint != "" && b.tier() != "none" {
		content += "\n\n" + hint
	}
	return Result{
		Content:   header + "\n" + content,
		Truncated: truncated,
		ExitCode:  &exitCode,
	}
}

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
func sandboxHint(output string) string {
	if !strings.Contains(output, "operation not permitted") &&
		!strings.Contains(output, "Operation not permitted") {
		return ""
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
			return "NOTE: " + c.hint + " Do not retry this command; it will fail the same way."
		}
	}
	return "NOTE: the sandbox denied this operation. Retrying the same command will " +
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
		s.Cwd = next
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
		if changesDir.MatchString(unquoted.Replace(line)) {
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

// IsCdLine reports a line that is the cd command with an argument: cd, then
// a space or a tab, the only blanks bash splits a line's words on.
func IsCdLine(line string) bool {
	return len(line) > 2 && line[:2] == "cd" && (line[2] == ' ' || line[2] == '\t')
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
