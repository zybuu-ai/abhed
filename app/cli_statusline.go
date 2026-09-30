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
// Where that tier is missing it is refused rather than run unsandboxed.
func statuslineSandbox(cfg config.Config, workspace string) (sandbox.Sandbox, error) {
	p, err := sandboxconfig.Policy(cfg, workspace)
	if err != nil {
		return nil, err
	}
	p.MinTier = sandbox.TierProcess
	p.AllowNetwork = false
	if f := statuslineScript(cfg.Statusline.Command, p.StatePaths); f != "" {
		p.ReadableFiles = []string{f}
	}
	sb := processSandbox(p)
	if ok, why := sb.Available(); !ok {
		return nil, fmt.Errorf("not run: it runs only under the process sandbox, which is not available here (%s)", why)
	}
	return sb, nil
}

// statuslineScript is the script a statusline command starts with, when it
// names one by path (~/ or absolute): an executable regular file, which the
// sandbox then shows read-only wherever it lives. A state file never is.
func statuslineScript(command string, state []string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	path := fields[0]
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(path) {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	for _, sp := range state {
		if rs, err := filepath.EvalSymlinks(sp); err == nil && rs == real {
			return ""
		}
	}
	return path
}

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
	c.statuslineOnce.Do(func() { c.statuslineSB, c.statuslineErr = statuslineSandbox(c.appCfg, c.workspace) })
	line, err := "", c.statuslineErr
	if err == nil {
		line, err = runStatusline(ctx, c.statuslineSB, c.workspace, cmd, c.statusModel(mode))
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
