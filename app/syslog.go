package app

import (
	"encoding/json"
	"strconv"
	"strings"
)

// sysLogTag names Abhed's entries in the system log.
const sysLogTag = "abhed-admin"

// sysLogWrite sends one entry to the operating system's log: the unified log
// on macOS, the journal or syslog on Linux. Replaced in tests, which must
// never write the real log.
var sysLogWrite = writeSysLog

// sysLogMessage is e as one line of fixed fields, every value quoted with
// newlines and other control characters escaped, so no value can end the
// entry or start one of its own. It never holds a key.
func sysLogMessage(e adminEntry) string {
	var b strings.Builder
	b.WriteString(sysLogTag)
	field := func(k, v string) {
		b.WriteString(" " + k + "=" + strconv.QuoteToASCII(v))
	}
	field("time", e.Time)
	field("uid", e.UID)
	field("user", e.User)
	field("sudo_user", e.SudoUser)
	field("action", e.Action)
	field("from", sectionLine(e.From))
	field("to", sectionLine(e.To))
	field("result", e.Result)
	field("reason", e.Reason)
	if e.Agent != "" {
		field("agent_command", e.Agent)
	}
	return b.String()
}

// sectionLine is a logged web_search section as compact JSON; shownSection
// has already put whether a key is set in place of the key.
func sectionLine(s map[string]any) string {
	if s == nil {
		return ""
	}
	out := map[string]any{}
	for k, v := range s {
		if k == "api_key" {
			continue
		}
		out[k] = v
	}
	data, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(data)
}
