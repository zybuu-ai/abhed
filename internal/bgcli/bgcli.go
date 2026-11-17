// Package bgcli is `abhed bg`: background jobs on an Abhed server, started
// and followed from the command line as the profile abhed login saved. A job
// runs as that person, under the server's sandbox and policy, and keeps
// running after this command exits.
package bgcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zybuu-ai/abhed/bgjob"
	"github.com/zybuu-ai/abhed/internal/visible"
	"github.com/zybuu-ai/abhed/profiles"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitFailed   = 1 // the call failed, or the job attached to did not finish done
	ExitUsage    = 2
	ExitAuth     = 3 // not signed in, or not allowed (401, 403)
	ExitNotFound = 4 // no such job (404)
	ExitConflict = 5 // the job is not in a state for this (409), or over a quota (429)
)

// Env is what the command runs with.
type Env struct {
	Ctx          context.Context
	ProfilesPath string
	In           io.Reader
	Out, Err     io.Writer
	// Interactive is set when In and Out are a person's terminal: attach
	// then asks the person to answer what a job waits on.
	Interactive bool
	// InAgent says why this process runs inside an agent's command, or "".
	InAgent func() string
	// Poll is how often attach looks again; zero is two seconds.
	Poll time.Duration
	// Workspace is the folder start snapshots the repository of.
	Workspace string
	// RulesFor gives the person's read rules for a repository's root, as a
	// session there would have them: a file they keep from the agent is
	// never in a snapshot. Nil sends no snapshot.
	RulesFor func(root string) (withheld func(abs string) bool, err error)
	// HoldsStored reports content holding a value from the person's
	// secrets store, which a snapshot never carries either.
	HoldsStored func([]byte) bool
	// SnapshotErr, when set, is why a snapshot cannot be checked, which
	// refuses one rather than send it unchecked.
	SnapshotErr error
}

type command struct {
	name, args, help string
	run              func(r *runner, args []string) error
}

var commands = []command{
	{"start", "[-name N] [-mode M] [-provider P] [-y | -no-snapshot] [-attach] PROMPT", "start a job on the server, as you, on a snapshot of this repository", (*runner).start},
	{"list", "", "your jobs, newest first", (*runner).list},
	{"logs", "ID [-patch]", "a job's record so far, or with -patch what it changed", (*runner).logs},
	{"attach", "ID", "follow a job and answer what it asks until it ends", (*runner).attach},
	{"cancel", "ID", "stop a job", (*runner).cancel},
}

// Commands names the subcommands.
func Commands() []string {
	out := make([]string, len(commands))
	for i, c := range commands {
		out[i] = c.name
	}
	return out
}

// Usage writes the command's help.
func Usage(w io.Writer) {
	fmt.Fprintln(w, `usage: abhed bg COMMAND [flags] [args]

Background jobs on an Abhed server: an agent that runs as you, in its own
sandbox under the server's policy, and keeps going after this command exits.
What it asks waits for you: abhed bg attach answers it (y, n, or a to
always allow for that job alone the rule the ask offers), and an ask left
unanswered past the server's timeout stops the job with nothing approved.
Every command takes -profile NAME or -server URL (default: the current
profile) and -o table|json.`)
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s %s\t%s\n", c.name, c.args, c.help)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, `
Exit status: 0 done, 1 failed, 2 usage, 3 not signed in or not allowed,
4 no such job, 5 not in a state for this or over a quota.
Refused inside an agent's command.`)
}

// Main runs `abhed bg ARGS` and returns the exit status.
func Main(env Env, args []string) int {
	if env.Ctx == nil {
		env.Ctx = context.Background()
	}
	if env.Poll <= 0 {
		env.Poll = 2 * time.Second
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
		fmt.Fprintf(env.Err, "abhed: no bg command %q\n\n", args[0])
		Usage(env.Err)
		return ExitUsage
	}
	// A job runs as the person with their secrets: an agent must not start
	// one, or answer one's asks, from inside its own command.
	if env.InAgent != nil {
		if why := env.InAgent(); why != "" {
			fmt.Fprintf(env.Err, "abhed: abhed bg %s is refused inside an agent's command (%s); run it in your own terminal\n", cmd.name, why)
			return ExitAuth
		}
	}
	r := &runner{Env: env, out: "table"}
	return r.exitFor(cmd.run(r, args[1:]))
}

type runner struct {
	Env
	out, profile, server string
	cl                   *bgjob.Client
	// lines are the person's answers, read by one reader for every
	// question, so a line typed ahead is the next question's, not lost.
	lines   chan lineRead
	linesAt error // the read's end, once it has ended
}

type lineRead struct {
	text string
	err  error
}

// readLine is the person's next line, or the context's end: Ctrl-C leaves a
// question as it leaves anything else.
func (r *runner) readLine() (string, error) {
	if r.linesAt != nil {
		return "", r.linesAt
	}
	if r.lines == nil {
		r.lines = make(chan lineRead)
		go func(in *bufio.Reader, out chan<- lineRead) {
			for {
				text, err := in.ReadString('\n')
				out <- lineRead{text, err}
				if err != nil {
					return
				}
			}
		}(bufio.NewReader(r.In), r.lines)
	}
	select {
	case <-r.Ctx.Done():
		return "", r.Ctx.Err()
	case l := <-r.lines:
		if l.err != nil {
			r.linesAt = l.err
		}
		if l.err != nil && l.text == "" {
			return "", l.err
		}
		return l.text, nil
	}
}

type usageError struct{ error }

// exitStatus is a status a command decided itself.
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
		if ue.error != nil && !errors.Is(ue.error, errPrinted) {
			fmt.Fprintf(r.Err, "abhed: %v\n", ue.error)
		}
		return ExitUsage
	}
	fmt.Fprintf(r.Err, "abhed: %s\n", line(err.Error()))
	switch bgjob.StatusOf(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ExitAuth
	case http.StatusNotFound:
		return ExitNotFound
	case http.StatusConflict, http.StatusTooManyRequests:
		return ExitConflict
	}
	return ExitFailed
}

var errPrinted = errors.New("usage printed")

func (r *runner) flags(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet("abhed bg "+name, flag.ContinueOnError)
	fs.SetOutput(r.Err)
	fs.StringVar(&r.profile, "profile", "", "the server's profile (default: the current one)")
	fs.StringVar(&r.server, "server", "", "the server's URL; its token comes from the profile for it")
	fs.StringVar(&r.out, "o", "table", "output: table or json")
	fs.Usage = func() {
		fmt.Fprintf(r.Err, "usage: abhed bg %s %s\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// parse reads flags wherever they stand among the arguments, up to "--".
func (r *runner) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{errPrinted}
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		if args[0] == "--" {
			pos = append(pos, args[1:]...)
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if r.out != "table" && r.out != "json" {
		return nil, usageError{fmt.Errorf("-o %q: use -o table or -o json", r.out)}
	}
	return pos, r.open()
}

// open resolves the profile: -profile, the one for -server, or the current.
func (r *runner) open() error {
	reg, err := profiles.Load(r.ProfilesPath)
	if err != nil {
		return err
	}
	name := r.profile
	if r.server != "" {
		u, err := profiles.NormalizeURL(r.server, true)
		if err != nil {
			return usageError{err}
		}
		found := ""
		for _, n := range reg.Names() {
			if reg.Profiles[n].URL == u && (name == "" || n == name) {
				found = n
				break
			}
		}
		if found == "" {
			return &bgjob.Error{Status: http.StatusUnauthorized, Message: fmt.Sprintf("no profile for %s; run abhed login %s", u, u)}
		}
		name = found
	}
	n, p, err := reg.Get(name)
	if err != nil {
		return &bgjob.Error{Status: http.StatusUnauthorized, Message: err.Error()}
	}
	if !p.SignedIn(time.Now()) && p.RefreshToken == "" {
		return &bgjob.Error{Status: http.StatusUnauthorized,
			Message: fmt.Sprintf("the profile %s (%s) is not signed in; run abhed login %s", n, p.URL, p.URL)}
	}
	hc, err := p.HTTPClient()
	if err != nil {
		return err
	}
	r.cl = &bgjob.Client{Profile: p, HTTP: hc, Saved: func(*profiles.Profile) error { return profiles.Save(r.ProfilesPath, reg) }}
	return nil
}

func (r *runner) json(v any) error {
	enc := json.NewEncoder(r.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// line is untrusted text shown on one line, every hidden character escaped.
func line(s string) string { return visible.Text(s, false, true) }

// short is line, cut to n runes.
func short(s string, n int) string {
	s = line(s)
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

func (r *runner) start(args []string) error {
	fs := r.flags("start", "[-name N] [-mode M] [-provider P] [-y | -no-snapshot] [-attach] PROMPT")
	var req bgjob.StartRequest
	fs.StringVar(&req.Name, "name", "", "a label for the job")
	fs.StringVar(&req.Mode, "mode", "", "plan, or the server's own mode (default: the server's)")
	fs.StringVar(&req.Provider, "provider", "", "a model provider the server offers")
	attach := fs.Bool("attach", false, "follow the job once it starts, as attach does")
	bare := fs.Bool("no-snapshot", false, "start in an empty folder, sending none of this repository")
	yes := fs.Bool("y", false, "send the repository's snapshot without asking")
	pos, err := r.parse(fs, args)
	if err != nil {
		return err
	}
	req.Prompt = strings.TrimSpace(strings.Join(pos, " "))
	if req.Prompt == "" {
		return usageError{errors.New("abhed bg start needs a prompt")}
	}
	if !*bare {
		snap, err := r.snapshot(*yes)
		if err != nil {
			return err
		}
		req.Workspace = bgjob.Workspace{Kind: bgjob.WorkspaceSnapshot, Snapshot: snap}
	}
	j, err := r.cl.Start(r.Ctx, req)
	if err != nil {
		return err
	}
	if r.out == "json" && !*attach {
		return r.json(j)
	}
	fmt.Fprintf(r.Out, "started %s (session %s)\n", line(j.ID), line(j.SessionID))
	if !*attach {
		fmt.Fprintf(r.Out, "follow it: abhed bg attach %s\n", line(j.ID))
		return nil
	}
	return r.follow(j.ID)
}

func (r *runner) list(args []string) error {
	pos, err := r.parse(r.flags("list", ""), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{errors.New("abhed bg list takes no arguments")}
	}
	jobs, err := r.cl.List(r.Ctx)
	if err != nil {
		return err
	}
	if r.out == "json" {
		return r.json(bgjob.JobList{Jobs: jobs})
	}
	if len(jobs) == 0 {
		fmt.Fprintln(r.Out, "no jobs")
		return nil
	}
	tw := tabwriter.NewWriter(r.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tSTARTED\tNAME")
	for _, j := range jobs {
		state := string(j.State)
		switch {
		case j.Waiting != nil:
			state += " (" + short(j.Waiting.Tool, 20) + ")"
		case j.State.Final() && j.State != bgjob.StateDone && j.Reason != "":
			// Why it ended, as -o json has it: max_budget, owner_revoked…
			state += " (" + short(j.Reason, 30) + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", line(j.ID), line(state), j.Created.Local().Format("Jan 2 15:04"), short(j.Name, 60))
	}
	return tw.Flush()
}

// one takes the single ID a command needs.
func one(name string, pos []string) (string, error) {
	if len(pos) != 1 || pos[0] == "" {
		return "", usageError{fmt.Errorf("abhed bg %s needs one job ID", name)}
	}
	return pos[0], nil
}

func (r *runner) logs(args []string) error {
	fs := r.flags("logs", "ID [-patch]")
	patch := fs.Bool("patch", false, "print what the job changed, as a unified diff")
	pos, err := r.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one("logs", pos)
	if err != nil {
		return err
	}
	j, err := r.cl.Get(r.Ctx, id)
	if err != nil {
		return err
	}
	if *patch {
		var p string
		ok := true
		if j.Result != nil {
			// Kept when the job ended; its folder is gone.
			p, err = r.cl.JobPatch(r.Ctx, j.ID)
		} else {
			p, ok, err = r.cl.Patch(r.Ctx, j.SessionID)
		}
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the server no longer holds this job's originals, so it cannot diff its changes; the record still has every edit (abhed bg logs " + id + ")")
		}
		if r.out == "json" {
			return r.json(map[string]string{"job": j.ID, "patch": p})
		}
		_, err = io.WriteString(r.Out, p)
		return err
	}
	evs, err := r.cl.Events(r.Ctx, j.SessionID)
	if err != nil {
		return err
	}
	if r.out == "json" {
		return r.json(evs)
	}
	for _, e := range evs {
		r.printEvent(e)
	}
	return nil
}

// streamed are the fragments of a reply or of reasoning as they arrive. The
// whole text follows in agent.message or agent.reasoning, so the table
// shows that once instead of a line per fragment; -o json keeps them.
var streamed = map[string]bool{"agent.delta": true, "agent.reasoning.delta": true}

// printEvent writes one event as a line: its type and what it says. A
// reply is shown whole, below its line, as the command line shows it.
func (r *runner) printEvent(e bgjob.Event) {
	if streamed[e.Type] {
		return
	}
	var p map[string]any
	_ = json.Unmarshal(e.Payload, &p)
	at := e.CreatedAt.Local().Format("15:04:05")
	if text, _ := p["text"].(string); e.Type == "agent.message" && strings.TrimSpace(text) != "" {
		fmt.Fprintf(r.Out, "%s  %s\n", at, line(e.Type))
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			fmt.Fprintf(r.Out, "  %s\n", visible.Text(l, false, true))
		}
		return
	}
	fmt.Fprintf(r.Out, "%s  %-22s %s\n", at, line(e.Type), short(eventSays(p), 160))
}

// eventSays is what an event says in a word or a line: a budget stop's
// message rather than its kind, and a run's end with its detail.
func eventSays(p map[string]any) string {
	str := func(k string) string { s, _ := p[k].(string); return s }
	for _, k := range []string{"text", "message", "command", "tool"} {
		if s := str(k); s != "" {
			return s
		}
	}
	if s := str("reason"); s != "" {
		if d := str("detail"); d != "" {
			return s + ": " + d
		}
		return s
	}
	return str("error")
}

func (r *runner) cancel(args []string) error {
	pos, err := r.parse(r.flags("cancel", "ID"), args)
	if err != nil {
		return err
	}
	id, err := one("cancel", pos)
	if err != nil {
		return err
	}
	j, err := r.cl.Cancel(r.Ctx, id)
	if err != nil {
		return err
	}
	if r.out == "json" {
		return r.json(j)
	}
	fmt.Fprintf(r.Out, "%s: %s\n", line(j.ID), line(string(j.State)))
	return nil
}

func (r *runner) attach(args []string) error {
	pos, err := r.parse(r.flags("attach", "ID"), args)
	if err != nil {
		return err
	}
	id, err := one("attach", pos)
	if err != nil {
		return err
	}
	return r.follow(id)
}

// follow prints a job's record as it grows and puts each ask it waits on to
// the person, until the job ends. It exits 0 only for a job that ended done.
func (r *runner) follow(id string) error {
	interactive := r.Interactive && r.In != nil
	seen := int64(-1)
	asked := ""
	// stopped is what the job's last budget stop said, for its last line.
	stopped := ""
	detached := func() error {
		// Leaving does not stop the job; it runs on.
		fmt.Fprintf(r.Err, "abhed: detached; job %s runs on (abhed bg attach %s)\n", line(id), line(id))
		return nil
	}
	for {
		j, err := r.cl.Get(r.Ctx, id)
		if err != nil {
			if r.Ctx.Err() != nil {
				return detached()
			}
			return err
		}
		if j.SessionID != "" {
			evs, err := r.cl.Events(r.Ctx, j.SessionID)
			if err != nil {
				if r.Ctx.Err() != nil {
					return detached()
				}
				return err
			}
			for _, e := range evs {
				if e.Seq > seen {
					seen = e.Seq
					if e.Type == "budget.stopped" {
						var p map[string]any
						_ = json.Unmarshal(e.Payload, &p)
						stopped = eventSays(p)
					}
					if r.out == "json" {
						_ = json.NewEncoder(r.Out).Encode(e)
					} else {
						r.printEvent(e)
					}
				}
			}
		}
		if j.State.Final() {
			msg := string(j.State)
			if j.Error != "" {
				msg += ": " + j.Error
			}
			if j.State != bgjob.StateDone && stopped != "" {
				msg += " (" + stopped + ")"
			}
			fmt.Fprintf(r.Err, "abhed: job %s %s\n", line(j.ID), line(msg))
			if j.State != bgjob.StateDone {
				return exitStatus(ExitFailed)
			}
			return nil
		}
		if w := j.Waiting; w != nil && w.RequestID+w.SessionID != asked {
			asked = w.RequestID + w.SessionID
			if err := r.ask(interactive, *w); err != nil {
				if errors.Is(err, errDetached) {
					return detached()
				}
				return err
			}
		}
		select {
		case <-r.Ctx.Done():
			return detached()
		case <-time.After(r.Poll):
		}
	}
}

// errDetached is the person leaving while an ask waits on them.
var errDetached = errors.New("detached")

// ask puts one ask to the person: the tool, the reason and the call's
// arguments, each escaped. Without a terminal it only says what waits and
// until when. Only y allows; anything else, and an empty line, is a no.
func (r *runner) ask(interactive bool, w bgjob.Approval) error {
	what := short(w.Tool, 60)
	if w.Reason != "" {
		what += " — " + short(w.Reason, 160)
	}
	if !interactive {
		fmt.Fprintf(r.Err, "abhed: the job waits on you: %s; answer by %s with abhed bg attach in a terminal\n",
			what, w.Deadline.Local().Format("15:04"))
		return nil
	}
	if w.RequestID == "" {
		// An answer naming no request lands on whichever is waiting, which
		// may not be the one shown.
		fmt.Fprintf(r.Err, "abhed: the job waits on you (%s), but the server did not say which request; answer it in the console\n", what)
		return nil
	}
	fmt.Fprintf(r.Err, "The job asks: %s\n", what)
	for _, l := range argLines(w.Args) {
		fmt.Fprintf(r.Err, "  %s\n", l)
	}
	if w.Scope != "" {
		// Remembered on this job's session only, never for the person's other jobs.
		fmt.Fprintf(r.Err, "  a = always for this job: %s\n", short(w.Scope, 200))
		fmt.Fprint(r.Err, "Allow it? [y/N/a] ")
	} else {
		fmt.Fprint(r.Err, "Allow it? [y/N] ")
	}
	a, err := r.answer()
	if err != nil {
		return err
	}
	always := w.Scope != "" && (a == "a" || a == "A")
	ok := always || a == "y" || a == "Y"
	if always {
		err = r.cl.AnswerAlways(r.Ctx, w)
		if err != nil && r.Ctx.Err() == nil && bgjob.StatusOf(err) == http.StatusForbidden {
			// A workspace that needs other approvers refuses "always": this
			// call alone may still be answered.
			fmt.Fprintf(r.Err, "abhed: always for this job was refused: %s\nAnswer this call only. Allow it? [y/N] ", line(err.Error()))
			if a, err = r.answer(); err != nil {
				return err
			}
			always, ok = false, a == "y" || a == "Y"
			err = r.cl.Answer(r.Ctx, w, ok)
		}
	} else {
		err = r.cl.Answer(r.Ctx, w, ok)
	}
	if err != nil {
		if r.Ctx.Err() != nil {
			return errDetached
		}
		if bgjob.StatusOf(err) == http.StatusForbidden {
			// Refused, not failed: the ask stays pending for whoever may answer it.
			fmt.Fprintf(r.Err, "abhed: the answer was refused: %s; the ask still waits\n", line(err.Error()))
			return nil
		}
		return err
	}
	switch {
	case always:
		fmt.Fprintf(r.Err, "allowed, and always for this job: %s\n", short(w.Scope, 200))
	case ok:
		fmt.Fprintln(r.Err, "allowed")
	default:
		fmt.Fprintln(r.Err, "denied")
	}
	return nil
}

// answer is the person's answer to a question, trimmed; no answer is "",
// a no. Ctrl-C at the question is errDetached.
func (r *runner) answer() (string, error) {
	ans, err := r.readLine()
	if r.Ctx.Err() != nil {
		fmt.Fprintln(r.Err)
		return "", errDetached
	}
	if err != nil {
		ans = ""
	}
	return strings.TrimSpace(ans), nil
}

// maxArg bounds what one argument shows; the record has it whole.
const maxArg = 2000

// argLines are a call's arguments, one per line, every hidden character and
// newline escaped and each value cut to maxArg runes.
func argLines(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return []string{short(string(raw), maxArg)}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v, isText := m[k].(string)
		if !isText {
			b, _ := json.Marshal(m[k])
			v = string(b)
		}
		out = append(out, short(k, 40)+": "+short(v, maxArg))
	}
	return out
}

// snapshot packs the repository the workspace is in and uploads it. What it
// leaves out, and why, is said by name, so the person knows what the job
// does not have.
func (r *runner) snapshot(yes bool) (string, error) {
	if r.SnapshotErr != nil {
		return "", fmt.Errorf("no snapshot was sent: %w; pass -no-snapshot to start in an empty folder", r.SnapshotErr)
	}
	root, err := repoRoot(r.Ctx, r.Workspace)
	if err != nil {
		return "", usageError{fmt.Errorf("%s is not in a git repository; run abhed bg start in one, or pass -no-snapshot to start in an empty folder", line(r.Workspace))}
	}
	if !yes && !r.Interactive {
		return "", usageError{errors.New("abhed bg start sends a snapshot of this repository to the server: pass -y to send it, or -no-snapshot to start in an empty folder")}
	}
	if r.RulesFor == nil {
		return "", errors.New("no snapshot was sent: the read rules that say what it may carry are not known here")
	}
	// The repository's own rules, from its root, apply to every file packed,
	// wherever in it the command was run.
	withheld, err := r.RulesFor(root)
	if err != nil {
		return "", fmt.Errorf("no snapshot was sent: %w", err)
	}
	p, err := pack(r.Ctx, root, withheld, r.HoldsStored)
	if err != nil {
		return "", fmt.Errorf("the snapshot: %w", err)
	}
	say := func(what string, names []string) {
		if len(names) == 0 {
			return
		}
		shown := names
		if len(shown) > 10 {
			shown = shown[:10]
		}
		for i := range shown {
			shown[i] = line(shown[i])
		}
		more := ""
		if len(names) > len(shown) {
			more = fmt.Sprintf(" and %d more", len(names)-len(shown))
		}
		fmt.Fprintf(r.Err, "abhed: left out %d %s: %s%s\n", len(names), what, strings.Join(shown, ", "), more)
	}
	say("file(s) your read rules withhold", p.withheld)
	say("file(s) named like a key or holding a secret", p.secret)
	say("other path(s)", p.skipped)
	if !yes {
		fmt.Fprintf(r.Err, "Send %d tracked file(s) of %s, %d KiB, to %s for the job? [y/N] ",
			p.files, line(root), (p.bytes+1023)/1024, line(r.cl.Profile.URL))
		ans, err := r.readLine()
		if a := strings.TrimSpace(ans); err != nil || (a != "y" && a != "Y") {
			if r.Ctx.Err() != nil {
				fmt.Fprintln(r.Err)
			}
			return "", errors.New("nothing was sent; the job was not started")
		}
	}
	snap, err := r.cl.UploadSnapshot(r.Ctx, p.tgz)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(r.Err, "abhed: sent %d tracked file(s) of %s, %d KiB, as they are in the working tree\n",
		p.files, line(root), (p.bytes+1023)/1024)
	return snap.ID, nil
}
