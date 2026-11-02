// Package recordship sends the local record to an org server as it is
// written: each session's lines exactly as the record holds them, so the
// server checks the same hash chain `abhed record verify` does.
//
// The record itself is the queue. The shipper keeps, under Abhed's state,
// how far each session has reached the server; after a time offline it goes
// on from there. Lines are already redacted when they are written, and a
// line the secrets store would still change is not sent.
//
// It is an experimental API until 2.0, and may change in a minor release.
package recordship

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Paths on the org server.
const (
	// LinesPath + session + "/lines" takes a batch; + session + "/head" says
	// how far the server holds the session.
	LinesPath  = "/v1/org/records/"
	EventsPath = "/v1/org/records/events"
)

// ContentType is a batch's body: a Header line, then the record's lines.
const ContentType = "application/x-ndjson"

// WireVersion is the batch format.
const WireVersion = 1

// MaxBatchBytes and MaxBatchLines bound a batch of several lines; one line
// alone may be up to MaxLineBytes, and a longer one is not shipped.
const (
	MaxBatchBytes = 1 << 20
	MaxBatchLines = 1000
	MaxLineBytes  = 8 << 20
)

// MaxBodyBytes is the most a batch body may be: one line at its longest, and its header.
const MaxBodyBytes = MaxLineBytes + 64<<10

// Header is a batch's first line: whose lines follow and where they go.
type Header struct {
	V       int    `json:"abhed_ship"`
	Session string `json:"session"`
	// From is how many lines the server already holds; the first line sent
	// is line From+1 and links to Before (64 zeros for line 1).
	From   int64  `json:"from"`
	Before string `json:"before"`
	Host   string `json:"host,omitempty"`
}

// Ack is the server's answer: how many of the session's lines it holds and
// the hash of the last. On a conflict (409) it says where the server is.
type Ack struct {
	Lines int64  `json:"lines"`
	Hash  string `json:"hash"`
	Error string `json:"error,omitempty"`
}

// Event is shipping state the client reports and the server records.
type Event struct {
	// Kind is blocked, diverged, gap, backlog or resumed.
	Kind    string    `json:"kind"`
	Session string    `json:"session,omitempty"`
	Lines   int64     `json:"lines,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Host    string    `json:"host,omitempty"`
	At      time.Time `json:"at"`
}

// Event kinds.
const (
	KindBlocked  = "blocked"  // a line still holds a stored secret; the session stops
	KindDiverged = "diverged" // the local record no longer holds lines already shipped
	KindGap      = "gap"      // a session was pruned before all of it was shipped
	KindBacklog  = "backlog"  // more is waiting than record.ship.max_backlog_mb
	KindResumed  = "resumed"  // shipping reached the server again after failing
	KindRefused  = "refused"  // the server refused a batch
	KindOversize = "oversize" // a line is longer than MaxLineBytes; the session stops
)

// EncodeBatch writes h and raws as a batch body.
func EncodeBatch(h Header, raws [][]byte) ([]byte, error) {
	h.V = WireVersion
	var b bytes.Buffer
	hb, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	b.Write(hb)
	b.WriteByte('\n')
	for _, r := range raws {
		if bytes.IndexByte(r, '\n') >= 0 {
			return nil, errors.New("a record line holds a newline")
		}
		b.Write(r)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// DecodeBatch reads a batch body of at most max bytes.
func DecodeBatch(r io.Reader, max int64) (Header, [][]byte, error) {
	var h Header
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return h, nil, fmt.Errorf("the batch does not read: %w", err)
	}
	if int64(len(data)) > max {
		return h, nil, fmt.Errorf("the batch is over %d bytes", max)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return h, nil, errors.New("the batch is empty or its last line is cut short")
	}
	lines := bytes.Split(data[:len(data)-1], []byte("\n"))
	dec := json.NewDecoder(bytes.NewReader(lines[0]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil || h.V != WireVersion {
		return h, nil, fmt.Errorf("the batch's first line is not a version %d header", WireVersion)
	}
	raws := lines[1:]
	for _, l := range raws {
		if len(l) == 0 {
			return h, nil, errors.New("the batch holds an empty line")
		}
	}
	if len(raws) > MaxBatchLines {
		return h, nil, fmt.Errorf("the batch holds more than %d lines", MaxBatchLines)
	}
	if total := len(data) - len(lines[0]) - 1; len(raws) > 1 && total > MaxBatchBytes {
		return h, nil, fmt.Errorf("a batch of several lines is over %d bytes", MaxBatchBytes)
	}
	return h, raws, nil
}
