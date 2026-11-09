package toolset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// The most of the change log one session start reads, and records.
const (
	configChangesReadMax = 1 << 20
	configChangesMax     = 200
)

// configChangesMark is how far a session has recorded the change log, and
// the hash of the log's first line then, so a log replaced by hand (rotated,
// edited) is read from its start.
type configChangesMark struct {
	Offset int64  `json:"offset"`
	First  string `json:"first"`
}

// configChangesFiles are the person's change log and the mark of what a
// session has already recorded from it.
func configChangesFiles() (log, mark string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, ".abhed")
	return filepath.Join(dir, "config-changes.jsonl"), filepath.Join(dir, "state", "config-changes.recorded"), nil
}

// firstLineHash is the SHA-256 of the log's first line (at most 4 KiB).
func firstLineHash(f *os.File) string {
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, 0)
	buf = buf[:n]
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		buf = buf[:i]
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// RecordConfigChanges records, as config.changed, the entries of
// ~/.abhed/config-changes.jsonl written since a session last did, so a change
// made outside a session (abhed mcp add, at a terminal or from Studio) reaches
// a hash-chained record. The lines are the person's file, so each is recorded
// untrusted and escaped. A session takes at most 200; the rest wait for the
// next, and one entry says how many. The mark is read and written under a
// lock, so sessions starting together record each line once.
func RecordConfigChanges(rec *agent.Recorder) error {
	logPath, markPath, err := configChangesFiles()
	if err != nil {
		return nil
	}
	f, err := os.Open(logPath) // #nosec G304 -- the person's own change log
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(markPath), 0o700); err != nil {
		return nil
	}
	unlock, err := config.LockFile(markPath + ".lock")
	if err != nil {
		return nil
	}
	defer unlock()
	first := firstLineHash(f)
	var mark configChangesMark
	if b, err := os.ReadFile(markPath); err == nil { // #nosec G304 -- beside the change log
		_ = json.Unmarshal(b, &mark)
	}
	from := mark.Offset
	if mark.First != first || from < 0 || from > st.Size() {
		from = 0
	}
	if from == st.Size() {
		return nil
	}
	buf := make([]byte, min(st.Size()-from, configChangesReadMax))
	n, err := f.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	buf = buf[:n]
	// Only whole lines: one still being written is read next time.
	whole := buf[:bytes.LastIndexByte(buf, '\n')+1]
	pos, recorded := 0, 0
	for pos < len(whole) && recorded < configChangesMax {
		end := pos + bytes.IndexByte(whole[pos:], '\n') + 1
		line := whole[pos : end-1]
		pos = end
		var c agent.ConfigChanged
		if json.Unmarshal(line, &c) != nil || c.Key == "" {
			continue
		}
		c = agent.ConfigChanged{Time: clip(c.Time), By: clip(c.By), Key: clip(c.Key), Name: clip(c.Name), Kind: clip(c.Kind), SHA256: clip(c.SHA256), Refused: clip(c.Refused)}
		if _, err := rec.Record(agent.EvConfigChanged, agent.ActorSystem, agent.Untrusted, c); err != nil {
			return err
		}
		recorded++
	}
	if more := bytes.Count(whole[pos:], []byte{'\n'}); more > 0 {
		if _, err := rec.Record(agent.EvConfigChanged, agent.ActorSystem, agent.Trusted,
			agent.ConfigChanged{By: "abhed", Key: "config-changes.jsonl", Pending: more}); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(configChangesMark{Offset: from + int64(pos), First: first})
	tmp, err := os.CreateTemp(filepath.Dir(markPath), "config-changes.recorded.*")
	if err != nil {
		return nil
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, _ = tmp.Write(b)
	if tmp.Close() == nil {
		_ = os.Rename(tmp.Name(), markPath)
	}
	return nil
}

// clip is a field of the change log as the record keeps it: escaped, and cut
// at 256 bytes on a character boundary.
func clip(s string) string {
	s = config.Printable(s)
	if len(s) <= 256 {
		return s
	}
	cut := 256
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
