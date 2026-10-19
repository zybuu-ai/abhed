package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// OCSFVersions are the OCSF schema versions record.ocsf.version may name,
// as package ocsf writes them.
var OCSFVersions = []string{"1.8.0", "1.5.0", "1.3.0", "1.1.0"}

// OCSFConfig is the JSONL file of OCSF events derived from the local record.
// Off by default.
type OCSFConfig struct {
	// Enabled turns the file on.
	Enabled bool `json:"enabled,omitempty"`
	// Path replaces <record dir>/<tenant>/ocsf/events.ocsf.jsonl. Managed only:
	// it decides where the record goes.
	Path string `json:"path,omitempty"`
	// Version is the OCSF version written; "" is 1.8.0.
	Version string `json:"version,omitempty"`
	// MaxMB rotates the file past this size; 0 is 64.
	MaxMB int `json:"max_mb,omitempty"`
	// Keep is how many rotated files are kept; 0 is 5.
	Keep int `json:"keep,omitempty"`
	// Locked, in the managed file, makes the managed record.ocsf the whole of
	// it: what any other file sets under record.ocsf is ignored.
	Locked bool `json:"locked,omitempty"`
}

func (o OCSFConfig) validate() error {
	if o.Version != "" && !slices.Contains(OCSFVersions, o.Version) {
		return fmt.Errorf("record.ocsf.version is %q; use one of %s", o.Version, strings.Join(OCSFVersions, ", "))
	}
	if o.MaxMB < 0 {
		return fmt.Errorf("record.ocsf.max_mb is %d; use a size in MiB, or 0 for 64", o.MaxMB)
	}
	if o.Keep < 0 {
		return fmt.Errorf("record.ocsf.keep is %d; use a number of files, or 0 for 5", o.Keep)
	}
	return nil
}

// lockOCSF applies a managed record.ocsf.locked: the section is then what the
// managed file says and nothing else, so no lower file turns it off or tunes it.
func lockOCSF(c *Config, managed []byte) {
	if !c.Record.OCSF.Locked || !c.ManagedSets("record.ocsf.locked") {
		return
	}
	var m struct {
		Record struct {
			OCSF OCSFConfig `json:"ocsf"`
		} `json:"record"`
	}
	_ = json.Unmarshal(managed, &m)
	c.Record.OCSF = m.Record.OCSF
}
