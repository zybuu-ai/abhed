package local

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Genesis is the prev of a chain's first line.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// line is one event as the record holds it. Its fields are written in this
// order, so the encoding is canonical once the payload is.
type line struct {
	Seq       int64           `json:"seq"`
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	ParentID  string          `json:"parent_id,omitempty"`
	Type      string          `json:"type"`
	Actor     string          `json:"actor"`
	Trust     string          `json:"trust"`
	CreatedAt string          `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
	Prev      string          `json:"prev"`
	Hash      string          `json:"hash,omitempty"`
}

// timeFormat is how created_at is written: UTC, nanoseconds, trailing zeros cut.
const timeFormat = time.RFC3339Nano

// seal fills in the line's hash, the sha256 of its encoding without one, and
// returns the bytes to write, newline not included.
func (l *line) seal() ([]byte, error) {
	l.Hash = ""
	body, err := encode(l)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	l.Hash = hex.EncodeToString(sum[:])
	return encode(l)
}

// lineFor builds the line an event becomes; the payload must be canonical.
func lineFor(ev agent.Event, prev string) line {
	return line{
		Seq: ev.Seq, ID: ev.ID, SessionID: ev.SessionID, ParentID: ev.ParentID,
		Type: string(ev.Type), Actor: string(ev.Actor), Trust: string(ev.Trust),
		CreatedAt: ev.CreatedAt.UTC().Format(timeFormat),
		Payload:   ev.Payload, Prev: prev,
	}
}

// event is the agent event a line holds.
func (l line) event() agent.Event {
	at, _ := time.Parse(timeFormat, l.CreatedAt)
	return agent.Event{
		ID: l.ID, SessionID: l.SessionID, ParentID: l.ParentID, Seq: l.Seq,
		Type: agent.EventType(l.Type), Payload: append(json.RawMessage(nil), l.Payload...),
		Actor: agent.Actor(l.Actor), Trust: agent.Trust(l.Trust), CreatedAt: at,
	}
}

// jsonStrict decodes one value and refuses fields the record does not write.
func jsonStrict(raw []byte) *json.Decoder {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec
}

// parseLine decodes one line without checking it.
func parseLine(raw []byte) (line, error) {
	var l line
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return l, err
	}
	if dec.More() {
		return l, errors.New("more than one value on the line")
	}
	return l, nil
}

// checkLine reports why raw is not a sealed line, "" when it is: it must
// decode, be written exactly as the record writes it, and carry its own hash.
func checkLine(raw []byte) (line, string) {
	l, err := parseLine(raw)
	if err != nil {
		return l, "the line is not a record entry: " + err.Error()
	}
	if canon, err := canonical(l.Payload); err != nil || !bytes.Equal(canon, l.Payload) {
		return l, "the payload is not as the record writes it"
	}
	if _, err := time.Parse(timeFormat, l.CreatedAt); err != nil {
		return l, "the time is not as the record writes it"
	}
	stated := l.Hash
	again := l
	sealed, err := again.seal()
	if err != nil {
		return l, "the line cannot be encoded: " + err.Error()
	}
	if again.Hash != stated {
		return l, "its hash does not match its content"
	}
	if !bytes.Equal(sealed, raw) {
		return l, "the line is not written as the record writes it"
	}
	return l, ""
}

// validID accepts the session and tenant names the record uses as file
// names: letters, digits, - and _, starting with a letter or digit.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func checkID(kind, id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("%s %q cannot name a record file", kind, id)
	}
	return nil
}

// isHash reports whether s is a sha256 in lowercase hex.
func isHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

// hashBytes is the sha256 of b in lowercase hex.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
