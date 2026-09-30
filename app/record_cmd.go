package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store/local"
)

// recordCmd is `abhed record`: list, show, verify, export and prune the local
// record. It reads the user's and the managed configuration for where the
// record is; the workspace's file cannot move it.
func recordCmd(workspace string, args []string, trust config.TrustChoice, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprint(stderr, `usage: abhed record <command>
  list    [-all|-repo] [-n N] [-json]   sessions in this workspace, newest first
  show    <session> [-json]            a session's events
  verify  [session|file ...]           check the chain; no argument checks everything
  export  <session> [-o path] [-format jsonl|html|txt] [-unverified]
                                       jsonl carries its head for checking elsewhere
  prune   <session> | -older-than 90d [-yes]
                                       remove sessions, leaving a tombstone in the index
A session is its id, a unique id prefix, its name, or the path of its file.
`)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: true})
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 1
	}
	// A moved record is state here too, so no export is written into it.
	registerState(cfg, workspace)
	rec, err := openRecord(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 1
	}
	defer func() { _ = rec.Close() }()
	c := recordCtx{rec: rec, cfg: cfg, workspace: workspace, in: stdin, out: stdout, err: stderr}
	switch args[0] {
	case "list", "ls":
		return c.list(args[1:])
	case "show":
		return c.show(args[1:])
	case "verify":
		return c.verify(args[1:])
	case "export":
		return c.export(args[1:])
	case "prune":
		return c.prune(args[1:])
	}
	return usage()
}

type recordCtx struct {
	rec       *local.Store
	cfg       config.Config
	workspace string
	in        io.Reader
	out, err  io.Writer
}

func (c recordCtx) fail(format string, args ...any) int {
	fmt.Fprintf(c.err, "abhed: "+format+"\n", args...)
	return 1
}

func (c recordCtx) list(args []string) int {
	fs := flag.NewFlagSet("record list", flag.ContinueOnError)
	fs.SetOutput(c.err)
	all := fs.Bool("all", false, "every workspace's sessions")
	repo := fs.Bool("repo", false, "every worktree of this repository")
	n := fs.Int("n", 20, "how many")
	asJSON := fs.Bool("json", false, "one JSON object per line")
	if fs.Parse(args) != nil {
		return 2
	}
	entries, err := c.rec.Index().List(local.Filter{Cwd: c.workspace, Repo: *repo, All: *all, Limit: *n})
	if err != nil {
		return c.fail("%v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(c.out)
		for _, e := range entries {
			_ = enc.Encode(e)
		}
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(c.out, "no sessions recorded here; -all lists every workspace's")
		return 0
	}
	for _, e := range entries {
		fmt.Fprintln(c.out, entryLine(e, *all))
	}
	return 0
}

// entryLine is one session in a list: id, name, state, age, branch, title.
func entryLine(e local.Entry, withCwd bool) string {
	state := e.Ended
	if state == "" {
		state = "open"
	}
	label := e.Title
	if e.Name != "" {
		label = e.Name + " · " + label
	}
	line := fmt.Sprintf("%-27s %-10s %-9s %-12s %s", e.ID, state, age(e.Updated), orDefault(e.GitBranch, "-"), label)
	if withCwd {
		line += "  (" + e.Cwd + ")"
	}
	return line
}

// age is how long ago t was, briefly.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// resolve finds the session a person named.
func (c recordCtx) resolve(key string) (local.Entry, error) {
	e, err := c.rec.Index().Resolve(key)
	if errors.Is(err, local.ErrAmbiguous) {
		return e, fmt.Errorf("%q matches more than one session; use more of its id", key)
	}
	return e, err
}

func (c recordCtx) show(args []string) int {
	fs := flag.NewFlagSet("record show", flag.ContinueOnError)
	fs.SetOutput(c.err)
	asJSON := fs.Bool("json", false, "the events as JSON, one per line")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fmt.Fprintln(c.err, "usage: abhed record show <session> [-json]")
		return 2
	}
	e, err := c.resolve(fs.Arg(0))
	if err != nil {
		return c.fail("%v", err)
	}
	events, err := c.rec.Events(e.ID)
	if err != nil {
		return c.fail("%v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(c.out)
		for _, ev := range events {
			_ = enc.Encode(ev)
		}
		return 0
	}
	fmt.Fprint(c.out, transcriptText(e, events))
	return 0
}

// transcriptText is a session as plain lines: step, time, who, what.
func transcriptText(e local.Entry, events []agent.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s", e.ID)
	if e.Name != "" {
		fmt.Fprintf(&b, " (%s)", e.Name)
	}
	fmt.Fprintf(&b, " · %s · %s\n", e.Cwd, e.Created.Local().Format("2006-01-02 15:04"))
	for _, ev := range events {
		if ev.Type == agent.EvAgentDelta || ev.Type == agent.EvAgentReasoningDelta {
			continue
		}
		fmt.Fprintf(&b, "%5d  %s  %-6s %-22s %s\n", ev.Seq, ev.CreatedAt.Local().Format("15:04:05"),
			ev.Actor, ev.Type, eventSummary(ev))
	}
	return b.String()
}

// eventSummary is one line about an event.
func eventSummary(ev agent.Event) string {
	switch ev.Type {
	case agent.EvUserMessage, agent.EvAgentMessage:
		var m agent.Message
		_ = json.Unmarshal(ev.Payload, &m)
		return firstLine(m.Text, 100)
	case agent.EvActionRequested:
		var a agent.ActionRequested
		_ = json.Unmarshal(ev.Payload, &a)
		return a.Tool + " " + firstLine(string(a.Args), 90)
	case agent.EvObservation:
		var o agent.Observation
		_ = json.Unmarshal(ev.Payload, &o)
		return firstLine(o.Content, 100)
	}
	return firstLine(string(ev.Payload), 100)
}

func (c recordCtx) verify(args []string) int {
	if len(args) == 0 {
		idx, reps, err := c.rec.VerifyAll()
		if err != nil {
			return c.fail("%v", err)
		}
		bad := printReport(c.out, "index", idx)
		for _, r := range reps {
			bad = printReport(c.out, r.ID, r) || bad
		}
		fmt.Fprintf(c.out, "%d session(s) checked in %s\n", len(reps), c.rec.Dir())
		fmt.Fprintln(c.out, verifyScope)
		if bad {
			return 1
		}
		return 0
	}
	bad := false
	for _, a := range args {
		var rep local.Report
		var err error
		if _, statErr := os.Stat(a); statErr == nil && !c.inRecord(a) {
			rep, err = local.VerifyFile(a) // an export, or a file from elsewhere
		} else {
			var e local.Entry
			if e, err = c.resolve(a); err == nil {
				rep, err = c.rec.Verify(e.ID)
			}
		}
		if err != nil {
			fmt.Fprintf(c.err, "abhed: %s: %v\n", a, err)
			bad = true
			continue
		}
		bad = printReport(c.out, a, rep) || bad
	}
	fmt.Fprintln(c.out, verifyScope)
	if bad {
		return 1
	}
	return 0
}

// verifyScope says what a passing verify does and does not show.
const verifyScope = "A passing check shows the record was not edited, reordered or cut short by the agent or by accident. It is not proof against the machine's owner, who can rewrite and re-chain it."

// inRecord reports whether path is a session file in this records directory.
func (c recordCtx) inRecord(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	_, err = c.rec.Index().Resolve(abs)
	return err == nil
}

// printReport prints one verify result and reports whether it failed.
func printReport(w io.Writer, name string, r local.Report) bool {
	if r.OK {
		fmt.Fprintf(w, "ok      %s: %d lines, head seq %d %s\n", name, r.Events, r.Head.Seq, short(r.Head.Hash))
	} else {
		where := fmt.Sprintf("at seq %d", r.FirstBad)
		if r.Line > 0 {
			where += fmt.Sprintf(" (line %d", r.Line)
			if r.EventID != "" {
				where += ", event " + r.EventID
			}
			where += ")"
		}
		fmt.Fprintf(w, "FAILED  %s: %s: %s\n", name, where, r.Reason)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "        note: %s\n", n)
	}
	return !r.OK
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func (c recordCtx) export(args []string) int {
	fs := flag.NewFlagSet("record export", flag.ContinueOnError)
	fs.SetOutput(c.err)
	out := fs.String("o", "", "where to write it (default ~/.abhed/exports/<session>.<format>; - for stdout)")
	format := fs.String("format", "jsonl", "jsonl (verifiable), html or txt")
	unverified := fs.Bool("unverified", false, "export a record that fails verification, marked as such")
	if fs.Parse(reorderFlags(args)) != nil || fs.NArg() != 1 {
		fmt.Fprintln(c.err, "usage: abhed record export <session> [-o path] [-format jsonl|html|txt] [-unverified]")
		return 2
	}
	e, err := c.resolve(fs.Arg(0))
	if err != nil {
		return c.fail("%v", err)
	}
	path, err := exportPath(*out, e.ID, *format)
	if err != nil {
		return c.fail("%v", err)
	}
	n, err := exportSession(c.rec, e, *format, path, c.out, *unverified, c.workspace)
	if err != nil {
		return c.fail("%v", err)
	}
	if path != "-" {
		fmt.Fprintf(c.err, "wrote %d events to %s\n", n, path)
	}
	return 0
}

// exportPath is where an export goes: the path given, or a file in
// ~/.abhed/exports, never the workspace, where it would join the repository.
func exportPath(given, id, format string) (string, error) {
	switch format {
	case "jsonl", "json", "html", "txt":
	default:
		return "", fmt.Errorf("unknown export format %q; use jsonl, html or txt", format)
	}
	if given != "" {
		return given, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".abhed", "exports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, id+"."+format), nil
}

// exportSession writes session e in format to path, "-" for stdout, and
// returns how many events it holds. The record is already redacted.
func exportSession(rec *local.Store, e local.Entry, format, path string, stdout io.Writer, unverified bool, roots ...string) (int, error) {
	// Checked before anything is written: a record that fails goes out only
	// when asked for, and marked.
	rep, err := rec.Verify(e.ID)
	if err != nil {
		return 0, err
	}
	if !rep.OK && !unverified {
		return 0, fmt.Errorf("%w; abhed record export -unverified writes it marked as such", &local.UnverifiedError{Report: rep})
	}
	events, err := rec.Events(e.ID)
	if err != nil {
		return 0, err
	}
	banner := ""
	if !rep.OK {
		banner = fmt.Sprintf("UNVERIFIED RECORD: at seq %d, %s\n", rep.FirstBad, rep.Reason)
	}
	w := stdout
	var f *os.File
	if path != "-" {
		if f, err = openExport(path, roots...); err != nil {
			return 0, err
		}
		w = f
	}
	switch format {
	case "jsonl":
		_, err = rec.Export(e.ID, w, local.ExportOptions{Unverified: unverified})
	case "json":
		var data []byte
		if data, err = json.MarshalIndent(events, "", "  "); err == nil {
			if banner != "" {
				data = []byte(fmt.Sprintf("{\"unverified\":%q,\"events\":%s}", strings.TrimSpace(banner), data))
			}
			_, err = w.Write(append(data, '\n'))
		}
	case "html":
		_, err = io.WriteString(w, htmlBanner(banner)+agent.ExportHTML(e.ID, events))
	default:
		_, err = io.WriteString(w, banner+transcriptText(e, events))
	}
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	return len(events), err
}

// htmlBanner marks an HTML export of an unverified record.
func htmlBanner(text string) string {
	if text == "" {
		return ""
	}
	return "<p><strong>" + html.EscapeString(strings.TrimSpace(text)) + "</strong></p>\n"
}

func (c recordCtx) prune(args []string) int {
	fs := flag.NewFlagSet("record prune", flag.ContinueOnError)
	fs.SetOutput(c.err)
	older := fs.String("older-than", "", "prune sessions last used longer ago than this, e.g. 90d")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if fs.Parse(reorderFlags(args)) != nil {
		return 2
	}
	both := fs.NArg() > 0 && *older != ""
	neither := fs.NArg() == 0 && *older == "" && c.cfg.Record.RetentionDays == 0
	if both || neither {
		fmt.Fprintln(c.err, "usage: abhed record prune <session> | -older-than 90d [-yes]")
		return 2
	}
	if fs.NArg() > 0 {
		e, err := c.resolve(fs.Arg(0))
		if err != nil {
			return c.fail("%v", err)
		}
		if !c.confirm(*yes, fmt.Sprintf("Remove session %s (%s)? Only a tombstone with its head stays in the index.", e.ID, orDefault(e.Name, e.Title))) {
			return c.fail("not pruned")
		}
		pruned, err := c.rec.Prune(e.ID, "user", "abhed record prune")
		if err != nil {
			return c.fail("%v", err)
		}
		printPruned(c.out, pruned)
		return 0
	}
	days := c.cfg.Record.RetentionDays
	if *older != "" {
		d, err := parseAge(*older)
		if err != nil {
			return c.fail("%v", err)
		}
		days = int(d.Hours() / 24)
		if d < 24*time.Hour {
			return c.fail("-older-than %s is under a day; give days, as 90d", *older)
		}
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	if !c.confirm(*yes, fmt.Sprintf("Remove every session last used more than %d days ago? Tombstones stay in the index.", days)) {
		return c.fail("not pruned")
	}
	pruned, skipped, err := c.rec.PruneOlder(cutoff, "user", fmt.Sprintf("older than %d days", days))
	printPruned(c.out, pruned)
	for _, id := range skipped {
		fmt.Fprintf(c.out, "skipped %s: open in another Abhed process\n", id)
	}
	if err != nil {
		return c.fail("%v", err)
	}
	return 0
}

func printPruned(w io.Writer, pruned []local.Pruned) {
	for _, p := range pruned {
		fmt.Fprintf(w, "pruned %s: tombstone keeps head seq %d %s\n", p.ID, p.Head.Seq, short(p.Head.Hash))
	}
	if len(pruned) == 0 {
		fmt.Fprintln(w, "nothing to prune")
	}
}

// confirm asks on a terminal; without one, only -yes goes ahead.
func (c recordCtx) confirm(yes bool, question string) bool {
	if yes {
		return true
	}
	if f, ok := c.in.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		fmt.Fprintln(c.err, "abhed: prune asks for confirmation; with no terminal, pass -yes")
		return false
	}
	fmt.Fprintf(c.err, "%s [y/N] ", question)
	line, _ := bufio.NewReader(c.in).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
}

// parseAge reads 90d, 12h or any Go duration.
func parseAge(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("%q is not a number of days", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// reorderFlags moves flags ahead of the positional arguments, so
// `export s-1 -o x` parses as `export -o x s-1` does.
func reorderFlags(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && (a == "-o" || a == "--o" || a == "-format" || a == "--format" || a == "-older-than" || a == "--older-than") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}
