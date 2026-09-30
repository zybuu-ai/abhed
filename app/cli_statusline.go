package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// statuslineTimeout bounds one run of the statusline command: it runs on
// every redraw of the status and must never hold the session.
var statuslineTimeout = 300 * time.Millisecond

// statuslineMax bounds what is read of its output.
const statuslineMax = 4 << 10

// processSandbox builds the statusline's backend; a test replaces it.
var processSandbox = func(p sandbox.Policy) sandbox.Sandbox { return sandbox.NewProcess(p) }

// statuslineSandbox is where the statusline command runs: the process tier
// with the network off, whatever the session's tier and network setting.
// Where that tier is missing it is refused rather than run unsandboxed. It
// returns the command to run, with a script named by path replaced by the
// file pinned now, and that pin, whose Info is nil for an inline command.
func statuslineSandbox(cfg config.Config, workspace string) (sandbox.Sandbox, string, sandbox.ReadableFile, error) {
	command := cfg.Statusline.Command
	p, err := sandboxconfig.Policy(cfg, workspace)
	if err != nil {
		return nil, "", sandbox.ReadableFile{}, err
	}
	p.MinTier = sandbox.TierProcess
	p.AllowNetwork = false
	pin, rest, err := statuslineScript(command, workspace, p.StatePaths)
	if err != nil {
		return nil, "", pin, err
	}
	if pin.Info != nil {
		p.ReadableFiles = []sandbox.ReadableFile{pin}
		command = shellQuote(pin.Path) + rest
	}
	sb := processSandbox(p)
	if ok, why := sb.Available(); !ok {
		return nil, "", pin, fmt.Errorf("not run: it runs only under the process sandbox, which is not available here (%s)", why)
	}
	return sb, command, pin, nil
}

// statuslineScript pins the script a statusline command starts with, when it
// names one by path (~/ or absolute) that is an executable file, and returns
// the rest of the command. The agent must not be able to change what runs,
// nor the script reach Abhed's state, so a script in the workspace, a temp
// or cache area the sandbox writes, ~/.abhed, the workspace's .abhed or a
// state path is refused, by where it is named and where it resolves.
func statuslineScript(command, workspace string, state []string) (sandbox.ReadableFile, string, error) {
	trimmed := strings.TrimLeft(command, " \t")
	first, rest, _ := strings.Cut(trimmed, " ")
	if rest != "" {
		rest = " " + rest
	}
	path := first
	home, homeErr := os.UserHomeDir()
	if tail, ok := strings.CutPrefix(path, "~/"); ok {
		if homeErr != nil {
			return sandbox.ReadableFile{}, "", nil
		}
		path = filepath.Join(home, tail)
	}
	if !filepath.IsAbs(path) {
		return sandbox.ReadableFile{}, "", nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return sandbox.ReadableFile{}, "", nil
	}
	pin, err := sandbox.PinReadable(path)
	if err != nil {
		return pin, "", fmt.Errorf("not run: %w", err)
	}
	stateDirs := append([]string{filepath.Join(workspace, tools.StateDir)}, state...)
	if homeErr == nil {
		stateDirs = append(stateDirs, filepath.Join(home, tools.StateDir))
	}
	writable := append([]string{workspace}, sandbox.WritableAreas()...)
	for _, p := range []string{path, pin.Path} {
		if under(p, stateDirs) {
			return sandbox.ReadableFile{}, "", fmt.Errorf("not run: %s is in Abhed's own state, which a statusline script may not be", config.Printable(first))
		}
		if under(p, writable) {
			return sandbox.ReadableFile{}, "", fmt.Errorf("not run: %s is where the agent can change it (the workspace, a temp or cache area); put the script elsewhere, such as ~/bin", config.Printable(first))
		}
	}
	return pin, rest, nil
}

// under reports whether p is one of dirs or inside one, by its own
// spelling and each dir's, as given and resolved.
func under(p string, dirs []string) bool {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		spellings := []string{filepath.Clean(d)}
		if r, err := filepath.EvalSymlinks(d); err == nil {
			spellings = append(spellings, r)
		}
		for _, s := range spellings {
			if rel, err := filepath.Rel(s, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				return true
			}
		}
	}
	return false
}

// shellQuote quotes s for bash.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runStatusline runs the configured statusline command with the status as
// JSON on stdin and returns its first line, sanitized. A workspace's
// statusline reaches here only once the workspace is trusted.
func runStatusline(ctx context.Context, sb sandbox.Sandbox, cwd, command string, m ui.StatusModel) (string, error) {
	if strings.TrimSpace(command) == "" || sb == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, statuslineTimeout)
	defer cancel()
	in, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	cmd := sb.Command(ctx, cwd, command)
	if cmd.Err != nil {
		return "", cmd.Err
	}
	cmd.Stdin = bytes.NewReader(in)
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("the statusline command took longer than %d ms", statuslineTimeout.Milliseconds())
		}
		return "", err
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	return sanitizeStatus(line), nil
}

// limitedBuffer keeps the first statuslineMax bytes written to it.
type limitedBuffer struct{ b bytes.Buffer }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := statuslineMax - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return l.b.String() }

// sanitizeStatus keeps printable text and SGR styles (ESC [ digits ; m) and
// drops every other control character and escape sequence: a status line
// must not move the cursor, clear the screen, set the title or write the
// clipboard. The visible text is cut to 200 characters.
func sanitizeStatus(s string) string {
	var b strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				k := j + 1
				for k < len(s) && (s[k] >= '0' && s[k] <= '9' || s[k] == ';') {
					k++
				}
				if k < len(s) && s[k] == 'm' {
					b.WriteString(s[i : k+1])
					i = k + 1
					continue
				}
				// Another CSI sequence: skip to its final byte.
				for k < len(s) && (s[k] < 0x40 || s[k] > 0x7e) {
					k++
				}
				i = min(k+1, len(s))
				continue
			}
			if j < len(s) && s[j] == ']' {
				// OSC: skip to BEL or ST.
				k := j + 1
				for k < len(s) && s[k] != 0x07 && (s[k] != 0x1b || k+1 >= len(s) || s[k+1] != '\\') {
					k++
				}
				if k < len(s) && s[k] == 0x1b {
					k++
				}
				i = min(k+1, len(s))
				continue
			}
			i = min(j+1, len(s))
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r == '\t' {
			r = ' '
		}
		if !unicode.IsPrint(r) && r != ' ' {
			continue
		}
		if visible >= 200 {
			continue
		}
		visible++
		b.WriteRune(r)
	}
	out := b.String()
	if strings.Contains(out, "\x1b[") {
		out += "\x1b[0m"
	}
	return out
}

// statusLine is the configured statusline's output for the session now, or
// "" with no command or when it failed; a failure is said once.
func (c *cliState) statusLine(ctx context.Context, mode string) string {
	cmd := c.appCfg.Statusline.Command
	if cmd == "" {
		return ""
	}
	c.statuslineOnce.Do(func() {
		c.statuslineSB, c.statuslineCmd, c.statuslinePin, c.statuslineErr = statuslineSandbox(c.appCfg, c.workspace)
	})
	line, err := "", c.statuslineErr
	if err == nil && c.statuslinePin.Info != nil && !c.statuslinePin.Same() {
		err = fmt.Errorf("not run: %s is no longer the file checked at the start of the session", config.Printable(c.statuslinePin.Path))
	}
	if err == nil {
		line, err = runStatusline(ctx, c.statuslineSB, c.workspace, c.statuslineCmd, c.statusModel(mode))
	}
	if err != nil {
		if !c.statuslineWarned {
			c.statuslineWarned = true
			return "statusline: " + err.Error()
		}
		return ""
	}
	return line
}
