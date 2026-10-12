package app

import (
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
func openWitness(cfg config.Config) (*witness.Witness, error) {
	wc := cfg.Record.Witness
	if !wc.On() || !cfg.ManagedSets("record.witness") {
		return nil, nil
	}
	dir := cfg.Record.Dir
	if dir == "" {
		d, err := local.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	token := ""
	if wc.TokenSecret != "" {
		v, err := openVault().Value(wc.TokenSecret)
		if err != nil {
			return nil, fmt.Errorf("record.witness: the token secret %s: %w", wc.TokenSecret, err)
		}
		token = v
	}
	w, err := witness.Open(witness.Config{File: wc.File, URL: wc.URL, Token: token, KeyFile: filepath.Join(dir, "witness.key"),
		OnError: witnessReporter(slog.Default(), time.Minute)})
	if err != nil {
		return nil, fmt.Errorf("record.witness: %w", err)
	}
	return w, nil
}

// closeWitness waits for heads still being sent and says which failed.
func closeWitness(w *witness.Witness) {
	w.Close(5 * time.Second)
	for {
		select {
		case err := <-w.Errors():
			fmt.Fprintf(os.Stderr, "abhed: a head of the record did not reach the witness: %v\n", err)
		default:
			return
		}
	}
}

// witnessReporter logs a head that did not reach the witness as it fails, so
// a long-running serve says so at once; after the first, at most one line per
// every, counting the failures it did not log.
func witnessReporter(log *slog.Logger, every time.Duration) func(error) {
	var mu sync.Mutex
	var last time.Time
	quiet := 0
	return func(err error) {
		mu.Lock()
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < every {
			quiet++
			mu.Unlock()
			return
		}
		missed := quiet
		last, quiet = now, 0
		mu.Unlock()
		log.Warn("a head of the record did not reach the witness", "error", err, "failures_not_logged", missed)
	}
}
