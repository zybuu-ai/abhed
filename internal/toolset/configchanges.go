package toolset

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// The most of the change log one session start reads, and records.
const (
	configChangesReadMax = 1 << 20
	configChangesMax     = 200
)

// configChangesFiles are the person's change log and the offset of what a
// session has already recorded from it.
func configChangesFiles() (log, mark string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, ".abhed")
	return filepath.Join(dir, "config-changes.jsonl"), filepath.Join(dir, "state", "config-changes.recorded"), nil
}

// RecordConfigChanges records, as config.changed, the entries of
// ~/.abhed/config-changes.jsonl written since a session last did, so a change
// made outside a session (abhed mcp add, at a terminal or from Studio) reaches
// a hash-chained record. The lines are the person's file, so each is recorded
// untrusted and escaped. A log that shrank is read from its start.
func RecordConfigChanges(rec *agent.Recorder) error {
	logPath, markPath, err := configChangesFiles()
	if err != nil {
		return nil
	}
	f, err := os.Open(logPath) // #nosec G304 -- the person's own change log
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	var from int64
	if b, err := os.ReadFile(markPath); err == nil { // #nosec G304 -- beside the change log
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n >= 0 && n <= st.Size() {
			from = n
		}
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
	end := bytes.LastIndexByte(buf, '\n') + 1
	recorded := 0
	for _, line := range bytes.Split(buf[:end], []byte{'\n'}) {
		if len(line) == 0 || recorded >= configChangesMax {
			continue
		}
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
	if err := os.MkdirAll(filepath.Dir(markPath), 0o700); err != nil {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(markPath), "config-changes.recorded.*")
	if err != nil {
		return nil
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, _ = tmp.WriteString(strconv.FormatInt(from+int64(end), 10))
	if tmp.Close() == nil {
		_ = os.Rename(tmp.Name(), markPath)
	}
	return nil
}

// clip is a field of the change log as the record keeps it: escaped and short.
func clip(s string) string {
	s = config.Printable(s)
	if len(s) > 256 {
		s = s[:256] + "…"
	}
	return s
}
