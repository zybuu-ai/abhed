// Package sandboxcli is `abhed sandbox`: a server's sandboxes and their
// templates from the command line, as the profile it signed in with.
package sandboxcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// Exit codes every subcommand uses. exec, connect and ssh exit with the
// remote command's status instead, and 255 when it did not run to one.
const (
	ExitOK       = 0
	ExitFailed   = 1 // the call failed: the server refused it or could not be reached
	ExitUsage    = 2 // the command was called wrongly
	ExitAuth     = 3 // not signed in, or the sign-in may not do this (401, 403)
	ExitNotFound = 4 // no such sandbox or template (404)
	ExitConflict = 5 // the object is not in a state for this, or changed (409, 412, 428)
	ExitRemote   = 255
)

// Env is what the command runs with.
type Env struct {
	Ctx context.Context
	// ProfilesPath is the profiles file; KnownHosts the SSH known_hosts
	// file the ssh and ssh-config commands keep.
	ProfilesPath string
	KnownHosts   string
	In           io.Reader
	Out, Err     io.Writer
	// Term is the terminal stdin and stdout are on, nil when they are not.
	Term Terminal
	// InAgent says why this process runs inside an agent's command, or "".
	InAgent func() string
	// Executable is this program, for the ssh ProxyCommand; SSH runs the
	// system ssh with its arguments.
	Executable string
	SSH        func(ctx context.Context, args []string) (int, error)
	// Listening, when set, is told where forward listens.
	Listening func(net.Addr)
}

// Terminal is the person's terminal.
type Terminal interface {
	// MakeRaw puts the terminal in raw mode and returns how to restore it.
	MakeRaw() (restore func(), err error)
	// Size is the terminal's rows and columns.
	Size() (rows, cols uint16, ok bool)
	// Resized signals each change of size until stop is called.
	Resized() (ch <-chan struct{}, stop func())
}

type command struct {
	name, args, help string
	run              func(e *runner, args []string) error
}

// runner carries one invocation.
type runner struct {
	Env
	out string // table or json
	cl  *client
	// profile flags, set by addCommon.
	profile, server string
}

var commands []command

func init() {
	commands = []command{
		{"create", "[flags]", "create a sandbox from a template", (*runner).create},
		{"list", "[flags]", "list sandboxes, by label selector and state", (*runner).list},
		{"get", "ID", "show one sandbox", (*runner).get},
		{"update", "ID [flags]", "change a sandbox's labels, settings or template (If-Match)", (*runner).update},
		{"exec", "[-i] ID [--] COMMAND [ARG...]", "run a command; its exit status is passed on", (*runner).exec},
		{"connect", "ID | --stdio ID", "an interactive shell (Ctrl-] leaves), or stdio for ProxyCommand", (*runner).connect},
		{"ssh", "ID [-- SSH ARGS]", "run ssh to the sandbox through the server's relay", (*runner).ssh},
		{"ssh-config", "[ID...] [-l SELECTOR]", "print a Host block per sandbox for ~/.ssh/config", (*runner).sshConfig},
		{"cp", "SRC DST", "copy into (ID:PATH as DST) or out of (ID:PATH as SRC) a sandbox", (*runner).cp},
		{"forward", "ID [LOCAL:]REMOTE", "forward a local port to a port on the sandbox's loopback", (*runner).forward},
		{"expose", "ID PORT", "publish a sandbox port at a URL for you alone", (*runner).expose},
		{"logs", "ID", "the sandbox's recorded events", (*runner).logs},
		{"stop", "ID", "stop a sandbox", (*runner).stop},
		{"start", "ID", "start a stopped sandbox", (*runner).start},
		{"delete", "ID [-y]", "delete a sandbox (asks first unless -y)", (*runner).delete},
		{"snapshot", "ID [-name NAME]", "snapshot a sandbox's workspace", (*runner).snapshot},
		{"restore", "ID (-f FILE | SNAPSHOT_ID -digest DIGEST)", "restore a stopped sandbox from a snapshot", (*runner).restore},
		{"template", "list|get|create|rm", "the server's sandbox templates", (*runner).template},
	}
}

// Usage writes the command's help.
func Usage(w io.Writer) {
	fmt.Fprintln(w, `usage: abhed sandbox COMMAND [flags] [args]

Sandboxes on an Abhed server, as the profile abhed login saved. Every
command takes -profile NAME or -server URL (default: the current profile)
and -o table|json.`)
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s %s\t%s\n", c.name, c.args, c.help)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, `
Exit status: 0 done, 1 the call failed, 2 usage, 3 not signed in or not
allowed, 4 not found, 5 conflict (state or version). exec, connect and ssh
exit with the remote command's status, and 255 when it did not run to one.

Refused inside an agent's command. Run abhed sandbox COMMAND -h for flags.`)
}

// Main runs `abhed sandbox ARGS` and returns the exit status.
func Main(env Env, args []string) int {
	if env.Ctx == nil {
		env.Ctx = context.Background()
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		Usage(env.Err)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	var cmd *command
	for i := range commands {
		if commands[i].name == args[0] {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		fmt.Fprintf(env.Err, "abhed: no sandbox command %q\n\n", args[0])
		Usage(env.Err)
		return ExitUsage
	}
	if env.InAgent != nil {
		if why := env.InAgent(); why != "" {
			fmt.Fprintf(env.Err, "abhed: abhed sandbox %s is refused inside an agent's command (%s); run it in your own terminal\n", cmd.name, why)
			return ExitFailed
		}
	}
	r := &runner{Env: env, out: "table"}
	err := cmd.run(r, args[1:])
	return r.exitFor(err)
}

// exitStatus is a status a command decided itself, such as a remote one.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (r *runner) exitFor(err error) int {
	if err == nil {
		return ExitOK
	}
	var st exitStatus
	if errors.As(err, &st) {
		return int(st)
	}
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	var ue usageError
	if errors.As(err, &ue) {
		if ue.err != nil && !errors.Is(ue.err, errUsagePrinted) {
			fmt.Fprintf(r.Err, "abhed: %v\n", ue.err)
		}
		return ExitUsage
	}
	fmt.Fprintf(r.Err, "abhed: %s\n", clean(err.Error(), 0))
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return ExitAuth
		case http.StatusNotFound:
			return ExitNotFound
		case http.StatusConflict, http.StatusPreconditionFailed, http.StatusPreconditionRequired:
			return ExitConflict
		}
	}
	return ExitFailed
}

var errUsagePrinted = errors.New("usage printed")

// flags is a command's flag set with the flags every command shares.
func (r *runner) flags(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet("abhed sandbox "+name, flag.ContinueOnError)
	fs.SetOutput(r.Err)
	fs.StringVar(&r.profile, "profile", "", "the server's profile (default: the current one)")
	fs.StringVar(&r.server, "server", "", "the server's URL; its token comes from the profile for it")
	fs.StringVar(&r.out, "o", "table", "output: table or json")
	fs.Usage = func() {
		fmt.Fprintf(r.Err, "usage: abhed sandbox %s %s\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// parse reads flags wherever they are among the arguments, up to "--" or,
// when stopAfter is above zero, the first argument that is not a flag after
// the stopAfter-th; what follows is returned as it was given.
func (r *runner) parse(fs *flag.FlagSet, args []string, stopAfter int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{errUsagePrinted}
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		if args[0] == "--" {
			pos = append(pos, args[1:]...)
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
		if stopAfter > 0 && len(pos) == stopAfter {
			// Flags may still follow; the first word after them, or after
			// "--", starts what is passed on as given.
			if err := fs.Parse(args); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return nil, err
				}
				return nil, usageError{errUsagePrinted}
			}
			pos = append(pos, fs.Args()...)
			break
		}
	}
	switch r.out {
	case "table", "json":
	case "yaml":
		return nil, usageError{errors.New("-o yaml is not available: Abhed has no YAML writer; use -o json")}
	default:
		return nil, usageError{fmt.Errorf("-o %q: use table or json", r.out)}
	}
	return pos, nil
}

// connect opens the client for the profile the flags chose.
func (r *runner) connectClient() error {
	if r.cl != nil {
		return nil
	}
	cl, err := openClient(r.ProfilesPath, r.profile, r.server)
	if err != nil {
		return err
	}
	r.cl = cl
	return nil
}

// emit writes v as JSON, or table as a table.
func (r *runner) emit(v any, table func(w *tabwriter.Writer)) error {
	if r.out == "json" {
		enc := json.NewEncoder(r.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	tw := tabwriter.NewWriter(r.Out, 2, 4, 2, ' ', 0)
	table(tw)
	return tw.Flush()
}

// multi is a repeatable flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// pairs reads k=v flags into a map.
func pairs(flagName string, vals []string) (map[string]string, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, v := range vals {
		for _, kv := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, usageError{fmt.Errorf("-%s %q: use key=value", flagName, kv)}
			}
			out[k] = val
		}
	}
	return out, nil
}

// clean makes text from the server safe to print on a terminal: control
// characters, which could move the cursor or retitle the window, are
// written as escapes. max, when above zero, bounds its length.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if max > 0 && n >= max {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// joinMap writes a map as k=v,k=v in key order, each value cleaned.
func joinMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = clean(k, 0) + "=" + clean(m[k], 80)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

// age is how long ago t was, briefly.
func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// confirm asks a yes-or-no question on the terminal; without one, the
// command must be given -y.
func (r *runner) confirm(q string) error {
	if r.Term == nil {
		return usageError{fmt.Errorf("%s needs -y when not run at a terminal", q)}
	}
	fmt.Fprintf(r.Err, "%s [y/N] ", q)
	line, err := bufio.NewReader(r.In).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("not confirmed")
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("not confirmed")
	}
	return nil
}
