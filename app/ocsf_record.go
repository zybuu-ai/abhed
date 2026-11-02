package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// productVersion is this binary's version, for the OCSF events it writes.
var productVersion atomic.Value

func ocsfOptions(tenant string) ocsf.Options {
	host, _ := os.Hostname()
	v, _ := productVersion.Load().(string)
	return ocsf.Options{ProductVersion: v, Hostname: host, Tenant: tenant}
}

// ocsfPath is where record.ocsf writes: the managed record.ocsf.path, or
// beside the tenant's sessions, where the agent can neither read nor write.
func ocsfPath(cfg config.Config, rec *local.Store) string {
	if p := cfg.Record.OCSF.Path; p != "" {
		return p
	}
	return filepath.Join(rec.Dir(), rec.Tenant(), "ocsf", "events.ocsf.jsonl")
}

// withOCSF has the record feed its OCSF file when record.ocsf.enabled is set:
// it hooks opts, and the returned start opens the file once the record is open.
func withOCSF(cfg config.Config, opts *local.Options) (start func(rec *local.Store)) {
	if !cfg.Record.OCSF.Enabled {
		return func(*local.Store) {}
	}
	var sink atomic.Pointer[ocsf.Sink]
	opts.OnAppend = func(l local.Sealed) {
		if s := sink.Load(); s != nil {
			s.Add(ocsf.Line{Event: l.Event, Prev: l.Prev, Hash: l.Hash, Pos: l.End})
		}
	}
	closeBefore := opts.OnClose
	opts.OnClose = func() {
		if s := sink.Swap(nil); s != nil {
			_ = s.Close()
		}
		if closeBefore != nil {
			closeBefore()
		}
	}
	return func(rec *local.Store) {
		o := cfg.Record.OCSF
		s, err := ocsf.OpenSink(ocsf.SinkOptions{
			Path: ocsfPath(cfg, rec), Version: o.Version, MaxBytes: int64(o.MaxMB) << 20, Keep: o.Keep,
			Map: ocsfOptions(rec.Tenant()), Read: recordLines(rec), Sessions: recordEnds(rec),
		})
		if err != nil {
			// The record is what counts; the file catches up from it next time.
			fmt.Fprintf(os.Stderr, "abhed: record.ocsf: %v\n", err)
			return
		}
		sink.Store(s)
	}
}

// recordLines reads a session's lines past a cursor, from the file offset
// the cursor kept rather than from the start.
func recordLines(rec *local.Store) func(string, ocsf.Cursor) ([]ocsf.Line, error) {
	return func(id string, from ocsf.Cursor) ([]ocsf.Line, error) {
		sealed, _, err := rec.LinesFrom(id, from.Pos)
		if err != nil {
			return nil, err
		}
		out := make([]ocsf.Line, 0, len(sealed))
		for _, s := range sealed {
			if s.Seq > from.Seq {
				out = append(out, ocsf.Line{Event: s.Event, Prev: s.Prev, Hash: s.Hash, Pos: s.End})
			}
		}
		return out, nil
	}
}

// recordEnds is where each session's file ends: its size, read from the
// file itself, never from a head a crash may have left behind.
func recordEnds(rec *local.Store) func() (map[string]ocsf.Cursor, error) {
	return func() (map[string]ocsf.Cursor, error) {
		ids, err := rec.SessionIDs()
		if err != nil {
			return nil, err
		}
		out := make(map[string]ocsf.Cursor, len(ids))
		for _, id := range ids {
			if size, err := rec.Size(id); err == nil && size > 0 {
				out[id] = ocsf.Cursor{Pos: size}
			}
		}
		return out, nil
	}
}

// serveOCSF has abhed serve write its store to the OCSF file when
// record.ocsf.enabled is set. With Postgres, one server at a time writes,
// holding an advisory lock, reading the events in commit order from where
// the last poll stopped; the cursors are rows every server reads, so one
// that takes over goes on where the last stopped. With the memory store,
// the one process writes, with its cursor beside the file.
func serveOCSF(cfg config.Config, st server.EventStore) (stop func(), err error) {
	if !cfg.Record.OCSF.Enabled {
		return func() {}, nil
	}
	o := cfg.Record.OCSF
	tenant := orDefault(cfg.Storage.Tenant, "default")
	path := o.Path
	if path == "" {
		dir := cfg.Record.Dir
		if dir == "" {
			if dir, err = local.DefaultDir(); err != nil {
				return nil, err
			}
		}
		path = filepath.Join(dir, tenant, "ocsf", "serve.ocsf.jsonl")
	}
	opts := ocsf.SinkOptions{Path: path, Version: o.Version, MaxBytes: int64(o.MaxMB) << 20, Keep: o.Keep,
		Map: ocsfOptions(tenant), Poll: time.Second}
	release := func() {}
	if pg, ok := store.PostgresOf(st); ok {
		w := pg.OCSFWriter("serve")
		release = w.Close
		opts.Cursors = pgCursors{w}
		opts.Read = storeLines(pg)
		// Nothing is listed: the feed goes on from the place in commit order
		// kept with the cursors, which a sink's first start sets to the end.
		opts.Sessions = func() (map[string]ocsf.Cursor, error) {
			saved, err := w.LoadPos()
			if err != nil || saved != "" {
				return map[string]ocsf.Cursor{}, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			end, err := pg.CommitEnd(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]ocsf.Cursor{}, w.SavePos(end.String())
		}
		opts.Feed = commitFeed(pg, w)
		st = nil
	}
	switch s := st.(type) {
	case nil:
	case *agent.MemStore:
		opts.Read = storeLines(s)
		opts.Sessions = func() (map[string]ocsf.Cursor, error) { return seqCursors(s.Ends()), nil }
	default:
		return nil, fmt.Errorf("record.ocsf: this event store cannot be written as OCSF")
	}
	sink, err := ocsf.OpenSink(opts)
	if err != nil {
		release()
		return nil, fmt.Errorf("record.ocsf: %w", err)
	}
	return func() {
		_ = sink.Close()
		release()
	}, nil
}

// storeLines reads a session's events after a cursor's seq.
func storeLines(st interface {
	Since(string, int64) ([]agent.Event, error)
}) func(string, ocsf.Cursor) ([]ocsf.Line, error) {
	return func(id string, from ocsf.Cursor) ([]ocsf.Line, error) {
		evs, err := st.Since(id, from.Seq)
		out := make([]ocsf.Line, len(evs))
		for i, ev := range evs {
			out[i] = ocsf.Line{Event: ev}
		}
		return out, err
	}
}

func seqCursors(seqs map[string]int64) map[string]ocsf.Cursor {
	out := make(map[string]ocsf.Cursor, len(seqs))
	for id, seq := range seqs {
		out[id] = ocsf.Cursor{Seq: seq}
	}
	return out
}

// pgCursors keeps a sink's cursors in Postgres, behind the writer's lock.
type pgCursors struct{ w *store.OCSFWriter }

func (c pgCursors) Lock() (func(), error) {
	unlock, err := c.w.Lock()
	if errors.Is(err, store.ErrOCSFStandby) {
		return nil, ocsf.ErrStandby
	}
	return unlock, err
}

func (c pgCursors) Load(ids []string) (map[string]ocsf.Cursor, bool, error) {
	rows, begun, err := c.w.Load(ids)
	out := make(map[string]ocsf.Cursor, len(rows))
	for id, r := range rows {
		out[id] = ocsf.Cursor{Seq: r.Seq, Provider: r.Provider, Primed: r.Primed}
	}
	return out, begun, err
}

func (c pgCursors) Save(moved map[string]ocsf.Cursor) error {
	rows := make(map[string]store.OCSFCursor, len(moved))
	for id, m := range moved {
		rows[id] = store.OCSFCursor{Seq: m.Seq, Provider: m.Provider, Primed: m.Primed}
	}
	return c.w.Save(rows)
}

// commitFeed gives the sink the events committed since the place kept with
// the cursors, at most 10,000 a poll; the place moves only once they are in
// the file, so a writer that stops before goes on from before them. Any
// session's events after the place are read whatever its cursor says, which
// the cursor's seq then filters: a server taking over reads from the place,
// not every session.
func commitFeed(pg *store.Postgres, w *store.OCSFWriter) func() ([]ocsf.Line, func() error, error) {
	return func() ([]ocsf.Line, func() error, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		saved, err := w.LoadPos()
		if err != nil || saved == "" {
			return nil, nil, err // the first scan has not set it yet
		}
		pos, err := store.ParseCommitPos(saved)
		if err != nil {
			// A place that cannot be read counts as older than any: the feed goes
			// on from the end, as a sink's first start does, rather than stop.
			end, eerr := pg.CommitEnd(ctx)
			if eerr != nil {
				return nil, nil, eerr
			}
			slog.Warn("OCSF file: the saved place in commit order cannot be read; going on from the end, events since the last good place are not exported",
				"error", err, "from", end.String())
			return nil, nil, w.SavePos(end.String())
		}
		const batch = 500
		var lines []ocsf.Line
		for range 20 {
			n := 0
			err := pg.ScanCommitted(ctx, pos, batch, func(ev agent.Event, at store.CommitPos) error {
				lines = append(lines, ocsf.Line{Event: ev})
				pos, n = at, n+1
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			if n < batch {
				break
			}
		}
		if p := pos.String(); p != saved {
			return lines, func() error { return w.SavePos(p) }, nil
		}
		return lines, nil, nil
	}
}
