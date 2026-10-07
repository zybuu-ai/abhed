package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// A refused trust names the _meta key the client sent, current or legacy.
func TestRequestedTrustNamesTheKeySent(t *testing.T) {
	c := &acpConn{}
	for key, want := range map[string]string{acpMetaKey: acpMetaKey, acpLegacyMetaKey: acpLegacyMetaKey} {
		raw := map[string]json.RawMessage{key: json.RawMessage(`{"trust":"trusted"}`)}
		_, e := c.requestedTrust(raw)
		if e == nil || !strings.Contains(e.Message, `_meta["`+want+`"]`) {
			t.Errorf("%s: %+v", key, e)
		}
	}
}
