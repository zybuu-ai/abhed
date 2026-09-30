package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/ui"
)

// statuslineTimeout bounds one run of the statusline command: it runs on
// every redraw of the status and must never hold the session.
const statuslineTimeout = 300 * time.Millisecond

// statuslineMax bounds what is read of its output.
const statuslineMax = 4 << 10

// runStatusline runs the configured statusline command with the status as
// JSON on stdin and returns its first line, sanitized. It runs under the
// session's sandbox: the same tier, the same network setting and the same
// workspace-scoped writes as the agent's own commands. A workspace's
// statusline reaches here only once the workspace is trusted.
func runStatusline(ctx context.Context, sb *lazySandbox, cwd, command string, m ui.StatusModel) (string, error) {
	if strings.TrimSpace(command) == "" || sb == nil {
		return "", nil
	}
	// Commands wait for the sandbox to be chosen; a status line waits a
	// second at most, apart from its own time limit.
	select {
	case <-sb.done:
	case <-time.After(time.Second):
		return "", errors.New("the sandbox is still being chosen")
	case <-ctx.Done():
		return "", ctx.Err()
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
			return "", errors.New("the statusline command took longer than 300 ms")
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
	line, err := runStatusline(ctx, c.sandbox, c.workspace, cmd, c.statusModel(mode))
	if err != nil {
		if !c.statuslineWarned {
			c.statuslineWarned = true
			return "statusline: " + err.Error()
		}
		return ""
	}
	return line
}
