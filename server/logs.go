package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/ocsf"
)

// streamLogs serves a session's record as OCSF events over SSE: the same
// stream, ownership checks and cursor as /events (Last-Event-ID or ?after=,
// the record's seq), each event mapped from its line. ?format=shorthand
// sends the one-line form instead; ?version= an older OCSF version.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	format := q.Get("format")
	if format == "" {
		format = "ocsf"
	}
	if format != "ocsf" && format != "shorthand" {
		WriteError(w, http.StatusBadRequest, "format is "+strconv.Quote(format)+"; use ocsf or shorthand")
		return
	}
	version := q.Get("version")
	if version == "" {
		version = ocsf.Version
	}
	if err := ocsf.CheckVersion(version); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	var types map[string]bool
	if t := q.Get("type"); t != "" {
		types = map[string]bool{}
		for _, name := range strings.Split(t, ",") {
			types[strings.TrimSpace(name)] = true
		}
	}
	id := r.PathValue("id")
	host, _ := os.Hostname()
	m := ocsf.NewMapper(ocsf.Options{Hostname: host, Tenant: TenantOf(r.Context())})
	primed := false
	write := func(w http.ResponseWriter, ev agent.Event) {
		// What a line needs from the ones before it, read once the stream
		// is already the caller's to read.
		if !primed {
			primed = true
			if before, err := s.store.Events(id); err == nil {
				for _, b := range before {
					if b.Seq < ev.Seq {
						m.Observe(ocsf.Line{Event: b})
					}
				}
			}
		}
		if types != nil && !types[string(ev.Type)] {
			m.Observe(ocsf.Line{Event: ev})
			return
		}
		out, ok, err := m.Map(ocsf.Line{Event: ev})
		if err != nil || !ok {
			return
		}
		if format == "shorthand" {
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, ocsf.Shorthand(out))
			return
		}
		if version != ocsf.Version {
			if out, err = ocsf.Downgrade(out, version); err != nil {
				return
			}
		}
		data, err := json.Marshal(out)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, data)
	}
	s.streamSession(w, r, write)
}

// sandboxEventsOCSF is a page of a sandbox's record as OCSF API Activity:
// GET /v1/sandboxes/{id}:events?format=ocsf[&version=V].
func (s *Server) sandboxEventsOCSF(w http.ResponseWriter, r *http.Request, items []controlplane.AuditEvent, next int64) {
	version := r.URL.Query().Get("version")
	if version == "" {
		version = ocsf.Version
	}
	if err := ocsf.CheckVersion(version); err != nil {
		writeCPError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	host, _ := os.Hostname()
	opts := ocsf.Options{Hostname: host, Tenant: TenantOf(r.Context())}
	out := make([]ocsf.Event, 0, len(items))
	for _, a := range items {
		ev := ocsf.MapAudit(a, opts)
		if version != ocsf.Version {
			var err error
			if ev, err = ocsf.Downgrade(ev, version); err != nil {
				writeCPError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
		}
		out = append(out, ev)
	}
	_ = json.NewEncoder(w).Encode(struct {
		Items []ocsf.Event `json:"items"`
		Next  int64        `json:"next,omitempty"`
	}{out, next})
}
