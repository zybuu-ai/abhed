package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/store/local"
)

// logsPoll is how often -f looks for new lines; the record is written by
// other processes, so it is read again rather than subscribed to.
var logsPoll = 500 * time.Millisecond

const logsUsage = `usage: abhed logs [session] [flags]
  The record as a log: one session (its id, prefix, name or file; the latest
  in this workspace when none is named), or every session with -all.
  -f                  follow: keep printing lines as they are recorded
  -since D|TIME       only lines from the last D (10m, 2h) or after TIME (RFC 3339)
  -after SEQ          only lines after this seq (one session)
  -cursor FILE        start after the seqs FILE holds, and keep them there as
                      lines are printed, so a restarted reader goes on where it stopped
  -format F           shorthand (the default), ocsf (JSON lines) or record (the chained lines)
  -ocsf-version V     the OCSF version for -format ocsf: 1.8.0 (the default), 1.5.0, 1.3.0, 1.1.0
  -type T[,T]         only these record types
  -all                every session in the record
`

// logsCmd is `abhed logs`: the local record as shorthand, OCSF or its own
// lines, with a cursor that survives a restart.
func logsCmd(workspace string, args []string, trust config.TrustChoice, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runLogs(ctx, workspace, args, trust, stdout, stderr)
}

type logsOpts struct {
	follow     bool
	since      time.Time
	after      int64
	cursorFile string
	format     string
	version    string
	types      map[string]bool
	all        bool
	session    string
}

func parseLogs(args []string) (logsOpts, error) {
	var o logsOpts
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&o.follow, "f", false, "")
	fs.BoolVar(&o.follow, "follow", false, "")
	since := fs.String("since", "", "")
	fs.Int64Var(&o.after, "after", 0, "")
	fs.StringVar(&o.cursorFile, "cursor", "", "")
	fs.StringVar(&o.format, "format", "shorthand", "")
	fs.StringVar(&o.version, "ocsf-version", ocsf.Version, "")
	types := fs.String("type", "", "")
	fs.BoolVar(&o.all, "all", false, "")
	// Flags may come after the session as well as before it.
	var rest []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return o, err
		}
		if fs.NArg() == 0 {
			break
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(rest) > 1 {
		return o, fmt.Errorf("one session at a time; -all reads every one")
	}
	if len(rest) == 1 {
		o.session = rest[0]
	}
	switch o.format {
	case "shorthand", "ocsf", "record":
	default:
		return o, fmt.Errorf("-format is %q; use shorthand, ocsf or record", o.format)
	}
	if err := ocsf.CheckVersion(o.version); err != nil {
		return o, err
	}
	if *since != "" {
		if d, err := time.ParseDuration(*since); err == nil {
			o.since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, *since); err == nil {
			o.since = t
		} else {
			return o, fmt.Errorf("-since is %q; use a duration such as 10m or a time such as 2026-10-19T09:00:00Z", *since)
		}
	}
	if *types != "" {
		o.types = map[string]bool{}
		for _, t := range strings.Split(*types, ",") {
			o.types[strings.TrimSpace(t)] = true
		}
	}
	if o.after < 0 {
		return o, fmt.Errorf("-after is %d; use a seq, or 0 for the start", o.after)
	}
	if o.all && (o.session != "" || o.after > 0) {
		return o, fmt.Errorf("-all reads every session; name none, and use -cursor rather than -after")
	}
	return o, nil
}

func runLogs(ctx context.Context, workspace string, args []string, trust config.TrustChoice, stdout, stderr io.Writer) int {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Fprint(stdout, logsUsage)
		return 0
	}
	o, err := parseLogs(args)
	if err != nil {
		fmt.Fprintf(stderr, "abhed: logs: %v\n%s", err, logsUsage)
		return 2
	}
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: true})
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 1
	}
	if cfg.Storage.Driver == "postgres" {
		fmt.Fprintln(stderr, "abhed: logs reads the local record; a server's sessions are at GET /v1/sessions/{id}/logs")
		return 1
	}
	registerState(cfg, workspace)
	rec, err := local.Open(local.Options{Dir: cfg.Record.Dir, Tenant: cliTenant(cfg), User: cliUser()})
	if err != nil {
		fmt.Fprintf(stderr, "abhed: open the local record: %v\n", err)
		return 1
	}
	defer func() { _ = rec.Close() }()
	r := &logsReader{o: o, rec: rec, out: stdout, mapper: ocsf.NewMapper(ocsfOptions(rec.Tenant())),
		cursor: map[string]int64{}, primed: map[string]bool{}}
	if o.cursorFile != "" {
		if r.cursor, err = readLogsCursor(o.cursorFile); err != nil {
			fmt.Fprintf(stderr, "abhed: logs: %v\n", err)
			return 1
		}
	}
	ids, err := r.sessions(workspace)
	if err != nil {
		fmt.Fprintf(stderr, "abhed: logs: %v\n", err)
		return 1
	}
	for {
		for _, id := range ids {
			if err := r.read(id); err != nil {
				fmt.Fprintf(stderr, "abhed: logs: %v\n", err)
				return 1
			}
		}
		if !o.follow {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(logsPoll):
		}
		if o.all {
			if ids, err = r.sessions(workspace); err != nil {
				fmt.Fprintf(stderr, "abhed: logs: %v\n", err)
				return 1
			}
		}
	}
}

type logsReader struct {
	o      logsOpts
	rec    *local.Store
	out    io.Writer
	mapper *ocsf.Mapper
	cursor map[string]int64
	primed map[string]bool
}

// sessions are what the command reads: one, or every session in the record.
func (r *logsReader) sessions(workspace string) ([]string, error) {
	if r.o.all {
		return r.rec.SessionIDs()
	}
	if r.o.session != "" {
		e, err := r.rec.Index().Resolve(r.o.session)
		if errors.Is(err, local.ErrAmbiguous) {
			return nil, fmt.Errorf("%q matches more than one session; use more of its id", r.o.session)
		}
		if err != nil {
			return nil, err
		}
		return []string{e.ID}, nil
	}
	entries, err := r.rec.Index().List(local.Filter{Cwd: workspace, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("no session is recorded in this workspace; name one, or use -all")
	}
	return []string{entries[0].ID}, nil
}

// read prints a session's lines past its cursor and moves the cursor.
func (r *logsReader) read(id string) error {
	after, ok := r.cursor[id]
	if !ok && !r.o.all {
		after = r.o.after
	}
	lines, err := r.rec.SealedSince(id, after)
	if err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	if len(lines) == 0 {
		return nil
	}
	if r.o.format == "ocsf" || r.o.format == "shorthand" {
		r.prime(id, after)
	}
	enc := json.NewEncoder(r.out)
	enc.SetEscapeHTML(false)
	for _, s := range lines {
		l := ocsf.Line{Event: s.Event, Prev: s.Prev, Hash: s.Hash}
		r.cursor[id] = s.Seq
		if r.o.format != "record" {
			r.mapper.Observe(l)
		}
		if (r.o.types != nil && !r.o.types[string(s.Type)]) || s.CreatedAt.Before(r.o.since) {
			continue
		}
		switch r.o.format {
		case "record":
			if err := enc.Encode(recordLine(s)); err != nil {
				return err
			}
		default:
			ev, ok, err := r.mapper.Map(l)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if r.o.format == "shorthand" {
				fmt.Fprintln(r.out, ocsf.Shorthand(ev))
				continue
			}
			if r.o.version != ocsf.Version {
				if ev, err = ocsf.Downgrade(ev, r.o.version); err != nil {
					return err
				}
			}
			if err := enc.Encode(ev); err != nil {
				return err
			}
		}
	}
	if r.o.cursorFile != "" {
		return writeLogsCursor(r.o.cursorFile, r.cursor)
	}
	return nil
}

// prime gives the mapper what a session's lines before after say, once.
func (r *logsReader) prime(id string, after int64) {
	if r.primed[id] {
		return
	}
	r.primed[id] = true
	if after <= 0 {
		return
	}
	lines, err := r.rec.SealedSince(id, 0)
	if err != nil {
		return
	}
	for _, s := range lines {
		if s.Seq > after {
			return
		}
		r.mapper.Observe(ocsf.Line{Event: s.Event})
	}
}

// recordLine is a line as the record holds it, chain links included.
func recordLine(s local.Sealed) map[string]any {
	m := map[string]any{"seq": s.Seq, "id": s.ID, "session_id": s.SessionID, "type": s.Type, "actor": s.Actor,
		"trust": s.Trust, "created_at": s.CreatedAt.UTC().Format(time.RFC3339Nano), "payload": s.Payload,
		"prev": s.Prev, "hash": s.Hash}
	if s.ParentID != "" {
		m["parent_id"] = s.ParentID
	}
	return m
}

// readLogsCursor reads the seqs a cursor file holds; a file not there yet is empty.
func readLogsCursor(path string) (map[string]int64, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c struct {
		Sessions map[string]int64 `json:"sessions"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("the cursor file %s is not one abhed logs wrote: %w", path, err)
	}
	if c.Sessions == nil {
		c.Sessions = map[string]int64{}
	}
	return c.Sessions, nil
}

// writeLogsCursor replaces the cursor file whole, so a reader stopped part
// way finds the last one it wrote.
func writeLogsCursor(path string, cur map[string]int64) error {
	ids := make([]string, 0, len(cur))
	for id := range cur {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString(`{"sessions":{`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(id)
		b.Write(k)
		b.WriteString(":" + strconv.FormatInt(cur[id], 10))
	}
	b.WriteString("}}\n")
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
