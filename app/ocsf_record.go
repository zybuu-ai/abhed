package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/ocsf"
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
			s.Add(ocsf.Line{Event: l.Event, Prev: l.Prev, Hash: l.Hash})
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
			Map: ocsfOptions(rec.Tenant()), Read: recordLines(rec), Sessions: recordHeads(rec),
		})
		if err != nil {
			// The record is what counts; the file catches up from it next time.
			fmt.Fprintf(os.Stderr, "abhed: record.ocsf: %v\n", err)
			return
		}
		sink.Store(s)
	}
}

// recordLines reads a session's lines after seq, chain links included.
func recordLines(rec *local.Store) func(string, int64) ([]ocsf.Line, error) {
	return func(id string, after int64) ([]ocsf.Line, error) {
		sealed, err := rec.SealedSince(id, after)
		out := make([]ocsf.Line, len(sealed))
		for i, s := range sealed {
			out[i] = ocsf.Line{Event: s.Event, Prev: s.Prev, Hash: s.Hash}
		}
		return out, err
	}
}

// recordHeads is each session's last seq.
func recordHeads(rec *local.Store) func() (map[string]int64, error) {
	return func() (map[string]int64, error) {
		ids, err := rec.SessionIDs()
		if err != nil {
			return nil, err
		}
		out := make(map[string]int64, len(ids))
		for _, id := range ids {
			if seq, _, err := rec.Index().Head(id); err == nil {
				out[id] = seq
			}
		}
		return out, nil
	}
}
