package app

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store/local"
	"github.com/zybuu-ai/abhed/store/witness"
)

// openWitness opens the managed record.witness, or nil when none is set. A
// witness that is set and cannot be opened stops the start: a control the
// organisation asked for does not silently go missing.
func openWitness(cfg config.Config) (*openedWitness, error) {
	wc := cfg.Record.Witness
	if !wc.On() || !cfg.ManagedSets("record.witness") {
		return nil, nil
	}
	keyFile, err := witnessKeyFile(cfg)
	if err != nil {
		return nil, err
	}
	token := ""
	if wc.TokenSecret != "" {
		v, err := openVault().Value(wc.TokenSecret)
		if err != nil {
			return nil, fmt.Errorf("record.witness: the token secret %s: %w", wc.TokenSecret, err)
		}
		token = v
	}
	rep := newWitnessReporter(slog.Default(), time.Minute)
	w, err := witness.Open(witness.Config{File: wc.File, URL: wc.URL, Token: token, KeyFile: keyFile, OnError: rep.report})
	if err != nil {
		return nil, fmt.Errorf("record.witness: %w", err)
	}
	return &openedWitness{Witness: w, rep: rep}, nil
}

// openedWitness is a witness with the reporter its failures go to.
type openedWitness struct {
	*witness.Witness
	rep *witnessReporter
}

// witnessKeyFile is where the installation keeps its witness signing key.
func witnessKeyFile(cfg config.Config) (string, error) {
	dir := cfg.Record.Dir
	if dir == "" {
		d, err := local.DefaultDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	return filepath.Join(dir, "witness.key"), nil
}

// witnessKey is the key a witness's statements must be signed with: a given
// public key (base64) or key file, or else this installation's own.
func witnessKey(cfg config.Config, given string) (ed25519.PublicKey, error) {
	if given != "" {
		if k, err := witness.ParsePublicKey(given); err == nil {
			return k, nil
		}
		return witness.PublicKey(given)
	}
	f, err := witnessKeyFile(cfg)
	if err != nil {
		return nil, err
	}
	return witness.PublicKey(f)
}

// closeWitness waits for heads still being sent and says which failed,
// including those the reporter held back.
func closeWitness(w *openedWitness) {
	w.Close(5 * time.Second)
	for {
		select {
		case err := <-w.Errors():
			fmt.Fprintf(os.Stderr, "abhed: a head of the record did not reach the witness: %v\n", err)
		default:
			w.rep.flush()
			return
		}
	}
}

// witnessReporter logs a head that did not reach the witness at once, then at
// most one line per every, counting the failures it held back.
type witnessReporter struct {
	log   *slog.Logger
	every time.Duration
	mu    sync.Mutex
	last  time.Time
	quiet int
}

func newWitnessReporter(log *slog.Logger, every time.Duration) *witnessReporter {
	return &witnessReporter{log: log, every: every}
}

func (r *witnessReporter) report(err error) {
	r.mu.Lock()
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < r.every {
		r.quiet++
		r.mu.Unlock()
		return
	}
	missed := r.quiet
	r.last, r.quiet = now, 0
	r.mu.Unlock()
	r.log.Warn("a head of the record did not reach the witness", "error", err, "failures_not_logged", missed)
}

// flush logs the count of failures held back since the last line, at close.
func (r *witnessReporter) flush() {
	r.mu.Lock()
	missed := r.quiet
	r.quiet = 0
	r.mu.Unlock()
	if missed > 0 {
		r.log.Warn("heads of the record did not reach the witness and were not logged", "failures_not_logged", missed)
	}
}
