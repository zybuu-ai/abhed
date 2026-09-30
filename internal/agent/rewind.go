package agent

import "encoding/json"

// NewEventID returns a new event id, for a store that records an event of
// its own, such as record.repaired.
func NewEventID() string { return newID() }

// EvCheckpoint records a file's content just before the agent changed it,
// kept in the record's blob store by its sha256, so the change can be
// undone or rewound after the process that made it has gone.
const EvCheckpoint EventType = "checkpoint.saved"

// CheckpointSaved is the payload of checkpoint.saved.
type CheckpointSaved struct {
	Path string `json:"path"`
	// SHA256 names the blob holding the content; "" when the file did not exist.
	SHA256 string `json:"sha256,omitempty"`
	Turn   int    `json:"turn"`
}

// BlobRefs lists the blobs a session's events name: its checkpoints and the
// contents its restores replaced and wrote.
func BlobRefs(events []Event) []string {
	var out []string
	for _, ev := range events {
		switch ev.Type {
		case EvCheckpoint:
			var c CheckpointSaved
			if json.Unmarshal(ev.Payload, &c) == nil && c.SHA256 != "" {
				out = append(out, c.SHA256)
			}
		case EvFileRestored:
			var r FileRestored
			if json.Unmarshal(ev.Payload, &r) == nil {
				for _, h := range []string{r.BeforeSHA256, r.AfterSHA256} {
					if h != "" {
						out = append(out, h)
					}
				}
			}
		}
	}
	return out
}
