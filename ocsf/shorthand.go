package ocsf

import (
	"fmt"
	"strings"
	"time"
)

// classShort are the shorthand's names for the classes Map writes.
var classShort = map[int]string{
	0: "BASE", 1001: "FILE", 1007: "PROC", 2004: "FIND", 4001: "NET", 4002: "HTTP", 4007: "SSH",
	5019: "CONF", 6002: "LIFE", 6003: "API",
}

var severityShort = map[int]string{0: "UNK", 1: "INFO", 2: "LOW", 3: "MED", 4: "HIGH", 5: "CRIT", 6: "FATAL"}

// Shorthand is ev on one line for a person reading a terminal:
//
//	2026-10-19T11:58:02.123Z s-1#12 API:Decision MED DENIED denied at deny
//
// time (UTC, milliseconds), session#seq, class:activity, severity, the
// security control's action when there is one, then the message. It is a
// view: the JSON event is what a reader should parse.
func Shorthand(ev Event) string {
	n, err := normal(ev)
	if err != nil {
		return ""
	}
	meta, _ := n["metadata"].(map[string]any)
	ms, _ := intOf(n["time"])
	class, _ := intOf(n["class_uid"])
	sev, _ := intOf(n["severity_id"])
	seq, _ := intOf(meta["sequence"])
	session, _ := meta["correlation_uid"].(string)
	act, _ := n["activity_name"].(string)
	short := classShort[class]
	if short == "" {
		short = fmt.Sprint(class)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s#%d %s:%s %s", time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05.000Z"),
		session, seq, short, act, severityShort[sev])
	if a, _ := n["action"].(string); a != "" {
		b.WriteString(" " + strings.ToUpper(a))
	}
	if m, _ := n["message"].(string); m != "" {
		b.WriteString(" " + oneLine(m))
	}
	return b.String()
}

// oneLine keeps a message on its line: control characters become spaces.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}
