package clitest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

var errNoRecord = errors.New("the run kept no local record (store/local writes it under ~/.abhed/records)")

// readRecord reads the most recently written session under home's record
// directory, laid out as store/local's contract says:
// <tenant>/<session>.jsonl, one event per line carrying seq, prev and hash.
//
// Verified here means the chain links: seq counts up from the first line,
// every line has a hash, each prev is the hash of the line before, and a
// head file, when there is one, names the last line. Recomputing each
// hash is the record's own verify, which a test runs as a command.
func readRecord(home string) (Record, error) {
	dir := filepath.Join(home, ".abhed", "records")
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") && filepath.Base(p) != "index.jsonl" {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		return Record{}, errNoRecord
	}
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a.ModTime().Before(b.ModTime())
	})
	return ReadRecordFile(files[len(files)-1])
}

// ReadRecordFile reads and link-checks one session file.
func ReadRecordFile(path string) (Record, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a record file under the run's own HOME
	if err != nil {
		return Record{}, err
	}
	var rec Record
	ok := true
	var prevHash string
	var lastSeq int64
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 0; sc.Scan(); n++ {
		line := sc.Bytes()
		var ev agent.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return rec, fmt.Errorf("%s line %d: %w", path, n+1, err)
		}
		var link struct {
			Prev string `json:"prev"`
			Hash string `json:"hash"`
		}
		_ = json.Unmarshal(line, &link)
		if link.Hash == "" || (n > 0 && link.Prev != prevHash) || (n > 0 && ev.Seq != lastSeq+1) {
			ok = false
		}
		prevHash, lastSeq = link.Hash, ev.Seq
		rec.Events = append(rec.Events, ev)
	}
	if err := sc.Err(); err != nil {
		return rec, err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if head, err := os.ReadFile(filepath.Join(filepath.Dir(path), "head", id)); err == nil { // #nosec G304 -- a head file beside the record the test reads
		// The head is JSON ({"lines","seq","hash"}); an older one was "seq hash".
		var h struct {
			Seq  int64  `json:"seq"`
			Hash string `json:"hash"`
		}
		if json.Unmarshal(head, &h) != nil {
			if f := strings.Fields(string(head)); len(f) >= 2 {
				h.Seq, _ = strconv.ParseInt(f[0], 10, 64)
				h.Hash = f[1]
			}
		}
		if h.Seq != lastSeq || h.Hash != prevHash {
			ok = false
		}
	}
	rec.Verified = ok && len(rec.Events) > 0
	return rec, nil
}

// ParseEvents reads events printed one per line, as -output-format json
// and stream-json do, skipping lines that are not events.
func ParseEvents(out string) []agent.Event {
	var evs []agent.Event
	for _, line := range strings.Split(out, "\n") {
		var ev agent.Event
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type != "" {
			evs = append(evs, ev)
		}
	}
	return evs
}
