package hawkeye

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// SupervisorStats is what the supervisor did in the session.
type SupervisorStats struct {
	Reviewed int `json:"reviewed"`
	// Unreviewed are the calls the turn budget kept from the judge.
	Unreviewed  int            `json:"unreviewed,omitempty"`
	Tightened   int            `json:"tightened"`
	Unavailable int            `json:"unavailable"`
	Corrections int            `json:"corrections"`
	Replans     int            `json:"replans"`
	ByCode      map[string]int `json:"by_code,omitempty"`
	Judges      []string       `json:"judges,omitempty"`
	// MaxLatencyMS is the slowest judgement.
	MaxLatencyMS int64 `json:"max_latency_ms"`
	// Concerns are the verdicts that named a concern, in order.
	Concerns []SupervisorVerdict `json:"concerns,omitempty"`
}

// SupervisorVerdict is one monitor.verdict with a concern.
type SupervisorVerdict struct {
	Seq       int64  `json:"seq"`
	CallID    string `json:"call_id"`
	Tool      string `json:"tool,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Before    string `json:"before"`
	After     string `json:"after"`
	Code      string `json:"code"`
	Rationale string `json:"rationale,omitempty"`
}

// supervisor reads the supervisor's events; nil when the record has none.
func supervisor(evs []agent.Event, calls []Call) *SupervisorStats {
	var s SupervisorStats
	byID := map[string]Call{}
	for _, c := range calls {
		byID[c.CallID] = c
	}
	judges := map[string]bool{}
	seen := false
	for _, e := range evs {
		switch e.Type {
		case agent.EvMonitorVerdict:
			var v agent.MonitorVerdict
			if json.Unmarshal(e.Payload, &v) != nil {
				continue
			}
			seen = true
			if v.Skipped != "" {
				s.Unreviewed++
				continue
			}
			s.Reviewed++
			if v.After != v.Before {
				s.Tightened++
			}
			if v.Unavailable {
				s.Unavailable++
			}
			if v.LatencyMS > s.MaxLatencyMS {
				s.MaxLatencyMS = v.LatencyMS
			}
			if v.Version != "" && !judges[v.Version] {
				judges[v.Version] = true
				s.Judges = append(s.Judges, v.Version)
			}
			if v.Code == "" || v.Code == "ok" {
				continue
			}
			if s.ByCode == nil {
				s.ByCode = map[string]int{}
			}
			s.ByCode[v.Code]++
			c := byID[v.CallID]
			s.Concerns = append(s.Concerns, SupervisorVerdict{Seq: e.Seq, CallID: v.CallID, Tool: c.Tool, Subject: clip(c.Subject, 200),
				Before: v.Before, After: v.After, Code: v.Code, Rationale: clip(v.Rationale, 300)})
		case agent.EvSupervisorCorrection:
			seen = true
			s.Corrections++
		case agent.EvSupervisorReplan:
			seen = true
			s.Replans++
		}
	}
	if !seen {
		return nil
	}
	return &s
}

// codes lists the concerns by how often they came up.
func codes(m map[string]int) string {
	type kv struct {
		k string
		n int
	}
	var all []kv
	for k, n := range m {
		all = append(all, kv{k, n})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n || all[i].n == all[j].n && all[i].k < all[j].k })
	var parts []string
	for _, x := range all {
		parts = append(parts, fmt.Sprintf("%s %d", x.k, x.n))
	}
	return strings.Join(parts, " · ")
}
