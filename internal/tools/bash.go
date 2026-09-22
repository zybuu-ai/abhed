package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

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
}

func (Bash) Name() string  { return "bash" }
func (Bash) Mutates() bool { return true }

func (b Bash) Description() string {
	d := "Run a shell command in the session workspace. Use for builds, tests, git, and package managers. Prefer read/glob/grep for file inspection — they are cheaper and safer. Note: the working directory persists between calls, but shell state (variables, functions) does not."
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

// Commands that hang forever waiting for a TTY. Rejecting them with guidance
// is far better than a 10-minute timeout.
var interactivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bgit\s+(rebase|add|commit)\s+.*-i\b`),
	regexp.MustCompile(`\bgit\s+(rebase|add)\s+--interactive\b`),
	regexp.MustCompile(`^\s*(vim?|nano|emacs|less|more|top|htop)\b`),
	// ssh without -T allocates a TTY and blocks; -T is the non-interactive form.
	regexp.MustCompile(`^\s*ssh\s+(?:-[^T\s]*\s+)*[^-\s]`),
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

	for _, re := range interactivePatterns {
		if re.MatchString(a.Command) {
			return errf("Refusing to run an interactive command: %s\nInteractive commands wait for a terminal that is not attached and will hang.\nUse a non-interactive equivalent (for example `git rebase --onto` instead of `git rebase -i`, or `cat` instead of `less`).", a.Command)
		}
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
		// Minimal environment: the agent should not inherit the operator's
		// credentials by accident.
		cmd.Env = append(os.Environ(), "ABHED_SESSION=1")
	}

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

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	content := out.String()
	truncated := false
	if len(content) > maxOutputChars {
		// Keep head and tail: the command's intent is at the start, the error
		// is almost always at the end.
		head := content[:maxOutputChars/2]
		tail := content[len(content)-maxOutputChars/2:]
		content = fmt.Sprintf("%s\n\n[... %d characters truncated ...]\n\n%s",
			head, len(out.String())-maxOutputChars, tail)
		truncated = true
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
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			exitCode = ee.ExitCode()
		} else {
			return errf("Failed to run command: %v", err)
		}
	}

	// Track cd so the working directory persists across calls, matching the
	// documented contract. Shell state deliberately does not persist.
	if newCwd := detectCd(a.Command, s); newCwd != "" {
		s.Cwd = newCwd
	}

	if content == "" {
		content = "[no output]"
	}

	// A non-zero exit is a valid observation the model must reason about, not a
	// tool failure. Never convert a failing test run into an error.
	header := fmt.Sprintf("exit %d · %s", exitCode, elapsed.Round(time.Millisecond))
	if hint := sandboxHint(content); hint != "" {
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

// detectCd resolves a trailing `cd` so the next call starts where this one
// ended. Only handles the simple leading/trailing forms models actually emit.
// FollowCd moves the session's working directory the way a run of command
// through the bash tool would, for a runner that executes commands itself.
func (s *Session) FollowCd(command string) {
	if next := detectCd(command, s); next != "" {
		s.Cwd = next
	}
}

func detectCd(command string, s *Session) string {
	parts := strings.Split(command, "&&")
	last := strings.TrimSpace(parts[len(parts)-1])
	if !strings.HasPrefix(last, "cd ") {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(last, "cd "))
	target = strings.Trim(target, `"'`)
	if target == "" || target == "~" {
		return s.Root
	}
	if !strings.HasPrefix(target, "/") {
		target = s.Cwd + "/" + target
	}
	resolved, err := s.Resolve(target)
	if err != nil {
		return "" // refuse to cd outside the workspace
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}
