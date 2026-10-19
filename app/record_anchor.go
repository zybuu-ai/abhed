package app

import (
	"errors"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store/local"
)

// RecordHead is one head of the record handed to an anchor: a local
// session's as it is synced, or a Postgres session's at the end of each run.
// Store is "local" or "postgres"; Lines is how many lines (local) or chained
// events (postgres) it holds, and Hash the hash of the last.
type RecordHead struct {
	Store, Tenant, Session string
	Lines, Seq             int64
	Hash                   string
}

// AnchorBuilder builds an anchor for the record's heads, beside the managed
// record.witness, or returns a nil anchor when the configuration asks for
// none. The anchor is called on the writing goroutine and must not block;
// stop, when not nil, runs as the record closes, so the last heads can be
// sent. An error stops the command or the start that opens the record.
type AnchorBuilder func(cfg config.Config) (anchor func(RecordHead), stop func(), err error)

// WithRecordAnchor adds an anchor for the record's heads, as an edition that
// signs or keeps them elsewhere needs.
func WithRecordAnchor(b AnchorBuilder) Option {
	return func(a *App) { a.anchors = append(a.anchors, b) }
}

// recordAnchors are the App's, set by Main: the record is opened from many
// commands that have only the configuration.
var recordAnchors []AnchorBuilder

// recordAnchor joins the managed witness and the registered anchors into one
// anchor and one stop; both are nil when there is none.
func recordAnchor(cfg config.Config) (func(RecordHead), func(), error) {
	var anchors []func(RecordHead)
	var stops []func()
	stopAll := func() {
		for _, s := range stops {
			s()
		}
	}
	w, err := openWitness(cfg)
	if err != nil {
		return nil, nil, err
	}
	if w != nil {
		anchors = append(anchors, func(h RecordHead) {
			if h.Store == "postgres" {
				w.Postgres(h.Tenant, h.Session, h.Lines, h.Hash)
				return
			}
			w.Local(h.Tenant, h.Session, h.Lines, h.Seq, h.Hash)
		})
		stops = append(stops, func() { closeWitness(w) })
	}
	for _, b := range recordAnchors {
		a, stop, err := b(cfg)
		if err != nil {
			stopAll()
			return nil, nil, errors.Join(errors.New("an anchor for the record's heads"), err)
		}
		if a != nil {
			anchors = append(anchors, a)
		}
		if stop != nil {
			stops = append(stops, stop)
		}
	}
	if len(anchors) == 0 {
		stopAll()
		return nil, nil, nil
	}
	return func(h RecordHead) {
		for _, a := range anchors {
			a(h)
		}
	}, stopAll, nil
}

// localAnchor adapts an anchor to the local record's.
func localAnchor(a func(RecordHead)) func(tenant, session string, h local.Head) {
	return func(tenant, session string, h local.Head) {
		a(RecordHead{Store: "local", Tenant: tenant, Session: session, Lines: h.Lines, Seq: h.Seq, Hash: h.Hash})
	}
}
