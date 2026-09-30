// Package local is the durable record on the person's own machine: the store
// the CLI and the desktop app use when storage.driver is not postgres, so
// sessions survive the process and can be listed, resumed, branched,
// rewound and verified without a database.
//
// This file is the contract. The implementation follows it.
//
// # Layout
//
// Under the records directory (~/.abhed/records, or the managed record.dir):
//
//	<tenant>/<session>.jsonl        the session's events, one per line
//	<tenant>/index.jsonl            append-only index; a later entry for a
//	                                session (a rename, an end) wins
//	<tenant>/head/<session>         the last seq and hash, for a quick check
//	<tenant>/blobs/sha256/<ab>/<hash>  checkpoint pre-images, by content
//
// Directories are 0700 and files 0600. The directory is in the state class:
// the agent can neither read nor write it, and @ mentions and ! commands
// cannot reach it.
//
// # Chain
//
// Each line is canonical JSON carrying seq, prev (the sha256 of the previous
// line) and hash. Verify walks the chain. The record is tamper-evident
// against the agent, not against the machine's owner.
//
// # Writes
//
// One writer per session, held with an exclusive lock; a second opener is
// refused and offered a fork. Events are redacted before they are written
// (the Recorder's redactor) and synced at turn boundaries. A torn last line
// found on open is cut off and a record.repaired event appended.
//
// # Retention
//
// Nothing is deleted automatically. There is no SessionDeleter: pruning is
// an explicit command that writes a tombstone (id, head hash, time) to the
// index, and a managed record.retention_days can require it.
package local

import (
	"errors"
	"time"

	"github.com/zybuu-ai/abhed/server"
)

// Record is the whole store: the event store every surface already writes
// to, session rows, resume claims, and what only a local record offers.
type Record interface {
	server.EventStore
	server.SessionRecorder
	server.SessionResumer
	// Index lists and finds sessions.
	Index() SessionIndex
	// Verify checks a session's chain end to end.
	Verify(id string) (Report, error)
	// Blobs holds checkpoint pre-images.
	Blobs() Blobs
}

// SessionIndex finds sessions without reading their events.
type SessionIndex interface {
	// List returns the sessions the filter selects, most recently updated first.
	List(f Filter) ([]Entry, error)
	// Get returns one session by id.
	Get(id string) (Entry, error)
	// Resolve finds a session by name, id, id prefix, or the path of its file.
	// A name or prefix that matches more than one is ErrAmbiguous.
	Resolve(nameOrIDOrPath string) (Entry, error)
	// Head is the session's last seq and the hash of that line.
	Head(id string) (seq int64, hash string, err error)
}

// Filter selects sessions for List. The zero value is this workspace's.
type Filter struct {
	// Cwd limits the list to sessions started in this directory; "" with
	// All unset means the caller's workspace.
	Cwd string
	// Repo widens Cwd to every worktree of the same git repository.
	Repo bool
	// All lists every workspace's sessions.
	All bool
	// Limit caps the result; zero means no cap.
	Limit int
}

// Entry is one session in the index.
type Entry struct {
	ID        string
	Name      string // set by -n or /rename; "" until then
	Cwd       string
	GitBranch string
	Created   time.Time
	Updated   time.Time
	// Parent and ForkSeq name the session this one was branched or forked
	// from and the last seq it took from it.
	Parent  string
	ForkSeq int64
	// Title is the first prompt, redacted and cut to 80 characters.
	Title string
	// Ended is the terminal reason, "" while the session is open.
	Ended string
	Head  Head
	// Repo is the git repository the session's directory belongs to, the
	// same for every worktree of it; "" outside one.
	Repo string
	// User is who the session was recorded for.
	User string
	// Subagent marks a subagent's own session, which lists do not show.
	Subagent bool
	// Pruned marks a session removed by prune; only its tombstone remains.
	Pruned bool
}

// Head is the last line of a session's chain: how many lines it has, and
// the seq and hash of the last. Seqs are unique but may be written out of
// order by concurrent writers, so the line count is what shows lines missing.
type Head struct {
	Lines int64  `json:"lines"`
	Seq   int64  `json:"seq"`
	Hash  string `json:"hash"`
}

// Report is what Verify found.
type Report struct {
	ID     string
	Events int64
	Head   Head
	// OK is set when every line's hash and prev link check out and the head
	// file agrees with the last line.
	OK bool
	// FirstBad is the seq of the first line that failed, and Reason why.
	// Line is its line number in the file, from 1, and EventID its id when
	// the line could be read.
	FirstBad int64
	Reason   string
	Line     int
	EventID  string
	// Repaired is set when a torn last line was cut off on open.
	Repaired bool
	// Torn is the length of an unfinished last line, which a crash leaves
	// and the next writer cuts off; it is noted, not a failure.
	Torn int64
	// Notes are what verification found that is not a failure.
	Notes []string
}

// Blobs is content-addressed storage for checkpoint pre-images. They hold
// the person's own file contents unredacted, since a redacted copy could not
// restore the file, and are pruned with their session.
type Blobs interface {
	// Put stores data and returns its sha256 in hex.
	Put(data []byte) (sha string, err error)
	// Get returns the data for sha, checking its hash on the way out.
	Get(sha string) ([]byte, error)
}

var (
	// ErrNotFound is a session, or a blob, the record does not hold.
	ErrNotFound = errors.New("not in the record")
	// ErrAmbiguous is a name or id prefix that matches more than one session.
	ErrAmbiguous = errors.New("matches more than one session")
	// ErrHeldElsewhere is a session another Abhed process is writing.
	ErrHeldElsewhere = errors.New("open in another Abhed process")
)
