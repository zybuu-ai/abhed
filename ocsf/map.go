// Package ocsf writes the record as OCSF events (Open Cybersecurity Schema
// Framework, schema.ocsf.io), version 1.8.0, and takes them down to 1.5.0,
// 1.3.0 and 1.1.0 for a reader that needs an older one.
//
// Every event is derived from one line of the hash-chained record, never from
// a copy kept elsewhere, so nothing is best-effort: the line's seq is
// metadata.sequence, its id metadata.uid, its session metadata.correlation_uid,
// and unmapped.record keeps what the line holds beyond that (its exact time,
// actor, trust and chain links) with the payload as unmapped.payload. Replay
// rebuilds the line from the event, and its hash still checks. The payload
// is the record's, already redacted when it was written, so an OCSF event
// carries no more than the record does.
package ocsf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Event is one OCSF event, as it is written to JSON.
type Event = map[string]any

// Line is one line of the record: the event and, where the store gives them,
// the hash of the line before and the line's own.
type Line struct {
	agent.Event
	Prev string
	Hash string
}

// Options say what every event names about where it came from.
type Options struct {
	// ProductVersion is Abhed's version, metadata.product.version.
	ProductVersion string
	// Hostname is the machine the record was written on.
	Hostname string
	// Tenant is the record's tenant, metadata.tenant_uid.
	Tenant string
}

// Mapper maps a session's lines in order. It keeps what a later line needs
// from an earlier one, such as the model provider a session started with, so
// a reader that starts part way through primes it with the lines before.
type Mapper struct {
	opts     Options
	provider map[string]string // session -> model provider
}

// NewMapper returns a Mapper for opts.
func NewMapper(opts Options) *Mapper {
	return &Mapper{opts: opts, provider: map[string]string{}}
}

// Observe takes what later lines need from l without mapping it.
func (m *Mapper) Observe(l Line) {
	switch l.Type {
	case agent.EvSessionStarted, agent.EvSessionResumed, agent.EvModelSwitched:
		var p struct {
			Provider string `json:"provider"`
		}
		if json.Unmarshal(l.Payload, &p) == nil && p.Provider != "" {
			m.provider[l.SessionID] = p.Provider
		}
	}
}

// Map maps one line. A type the table leaves out (Mapped reports which and
// why) gives ok false.
func (m *Mapper) Map(l Line) (ev Event, ok bool, err error) {
	m.Observe(l)
	r, known := table[l.Type]
	if !known {
		r = rule{fn: mapBase}
	}
	if r.skip != "" {
		return nil, false, nil
	}
	var payload map[string]any
	if len(l.Payload) > 0 {
		dec := json.NewDecoder(bytes.NewReader(l.Payload))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			// A payload that is not an object is kept whole under unmapped.
			payload = nil
		}
	}
	b := &builder{m: m, l: l, p: payload, ev: Event{}}
	r.fn(b)
	return b.finish(), true, nil
}

// builder assembles one event.
type builder struct {
	m        *Mapper
	l        Line
	p        map[string]any
	ev       Event
	class    int
	activity int
	actName  string
	profiles []string
}

func (b *builder) set(k string, v any) { b.ev[k] = v }

func (b *builder) str(k string) string {
	s, _ := b.p[k].(string)
	return s
}

func (b *builder) num(k string) (int64, bool) {
	switch n := b.p[k].(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(n), true
	}
	return 0, false
}

func (b *builder) flag(k string) bool {
	v, _ := b.p[k].(bool)
	return v
}

func (b *builder) profile(p string) {
	for _, have := range b.profiles {
		if have == p {
			return
		}
	}
	b.profiles = append(b.profiles, p)
}

// classify sets the class and activity; name is the activity's caption.
func (b *builder) classify(class, activity int, name string) {
	b.class, b.activity, b.actName = class, activity, name
}

// severity sets severity_id and its caption.
func (b *builder) severity(id int) {
	b.set("severity_id", id)
	b.set("severity", severities[id])
}

// status sets status_id, with a caption of its own for Other.
func (b *builder) status(id int, caption, detail string) {
	b.set("status_id", id)
	if caption == "" {
		caption = statuses[id]
	}
	b.set("status", caption)
	if detail != "" {
		b.set("status_detail", detail)
	}
}

// decision sets the security control outcome: action_id and disposition_id.
func (b *builder) decision(allowed bool, disposition int) {
	b.profile("security_control")
	if allowed {
		b.set("action_id", 1)
		b.set("action", "Allowed")
	} else {
		b.set("action_id", 2)
		b.set("action", "Denied")
	}
	b.set("disposition_id", disposition)
	b.set("disposition", dispositions[disposition])
}

// actor is who the line says acted: the record's actor, and the person by
// name where the payload names one.
func (b *builder) actor(name string) {
	user := map[string]any{}
	switch b.l.Actor {
	case agent.ActorUser:
		user["type_id"], user["type"] = 1, "User"
	case agent.ActorSystem:
		user["type_id"], user["type"] = 3, "System"
	default:
		user["type_id"], user["type"] = 99, actorCaption(b.l.Actor)
	}
	if name == "" {
		name = string(b.l.Actor)
	}
	user["name"] = name
	b.set("actor", map[string]any{"user": user, "app_name": "Abhed",
		"session": map[string]any{"uid": b.l.SessionID}})
	b.profile("host")
}

func actorCaption(a agent.Actor) string {
	switch a {
	case agent.ActorAgent:
		return "Agent"
	case agent.ActorTool:
		return "Tool"
	}
	return string(a)
}

// device is the machine the record was written on.
func (b *builder) device() {
	d := map[string]any{"type_id": 0, "type": "Unknown"}
	if b.m.opts.Hostname != "" {
		d["hostname"] = b.m.opts.Hostname
	}
	b.set("device", d)
	b.profile("host")
}

func (b *builder) source() map[string]any {
	if b.m.opts.Hostname != "" {
		return map[string]any{"hostname": b.m.opts.Hostname}
	}
	return map[string]any{"name": "abhed"}
}

// api names the operation: the tool for a call, else the record's type.
func (b *builder) api(operation, callID string) {
	if operation == "" {
		operation = string(b.l.Type)
	}
	a := map[string]any{"operation": operation, "service": map[string]any{"name": "abhed"}}
	if callID != "" {
		a["request"] = map[string]any{"uid": callID}
	}
	b.set("api", a)
	b.set("src_endpoint", b.source())
}

// ai fills the ai_operation profile: the model, when the session named its
// provider, and the role and tokens of the message.
func (b *builder) ai(model string, role int, prompt, completion int64) {
	if provider := b.m.provider[b.l.SessionID]; provider != "" && model != "" {
		b.set("ai_model", map[string]any{"name": model, "ai_provider": provider})
	}
	mc := map[string]any{"ai_role_id": role, "ai_role": aiRoles[role]}
	if prompt > 0 || completion > 0 {
		mc["prompt_tokens"], mc["completion_tokens"], mc["total_tokens"] = prompt, completion, prompt+completion
	}
	b.set("message_context", mc)
	b.profile("ai_operation")
}

func (b *builder) finish() Event {
	ev := b.ev
	ev["class_uid"] = b.class
	ev["class_name"] = classNames[b.class]
	ev["category_uid"] = b.class / 1000
	ev["category_name"] = categoryNames[b.class/1000]
	ev["activity_id"] = b.activity
	ev["activity_name"] = b.actName
	ev["type_uid"] = b.class*100 + b.activity
	ev["type_name"] = classNames[b.class] + ": " + b.actName
	ev["time"] = b.l.CreatedAt.UnixMilli()
	if _, ok := ev["severity_id"]; !ok {
		b.severity(1)
	}
	if _, ok := ev["message"]; !ok {
		ev["message"] = string(b.l.Type)
	}
	meta := map[string]any{
		"version":         Version,
		"product":         map[string]any{"name": "Abhed", "vendor_name": "Zybuu", "version": b.m.opts.ProductVersion},
		"uid":             b.l.ID,
		"sequence":        b.l.Seq,
		"correlation_uid": b.l.SessionID,
		"event_code":      string(b.l.Type),
		"log_name":        "abhed.record",
	}
	if b.m.opts.ProductVersion == "" {
		delete(meta["product"].(map[string]any), "version")
	}
	if b.m.opts.Tenant != "" {
		meta["tenant_uid"] = b.m.opts.Tenant
	}
	if len(b.profiles) > 0 {
		meta["profiles"] = append([]string(nil), b.profiles...)
	}
	ev["metadata"] = meta
	rec := map[string]any{
		"created_at": b.l.CreatedAt.UTC().Format(time.RFC3339Nano),
		"actor":      string(b.l.Actor),
		"trust":      string(b.l.Trust),
	}
	if b.l.ParentID != "" {
		rec["parent_id"] = b.l.ParentID
	}
	if b.l.Hash != "" {
		rec["prev"], rec["hash"] = b.l.Prev, b.l.Hash
	}
	un := map[string]any{"record": rec}
	if b.p != nil {
		un["payload"] = b.p
	} else if len(b.l.Payload) > 0 {
		un["payload_raw"] = string(b.l.Payload)
	}
	ev["unmapped"] = un
	return ev
}

var classNames = map[int]string{
	0: "Base Event", 1001: "File System Activity", 1007: "Process Activity", 2004: "Detection Finding",
	4001: "Network Activity", 4002: "HTTP Activity", 4007: "SSH Activity",
	5019: "Device Config State Change", 6002: "Application Lifecycle", 6003: "API Activity",
}

var categoryNames = map[int]string{
	0: "Uncategorized", 1: "System Activity", 2: "Findings", 4: "Network Activity",
	5: "Discovery", 6: "Application Activity",
}

var severities = map[int]string{0: "Unknown", 1: "Informational", 2: "Low", 3: "Medium", 4: "High", 5: "Critical"}

var statuses = map[int]string{0: "Unknown", 1: "Success", 2: "Failure", 99: "Other"}

var dispositions = map[int]string{1: "Allowed", 2: "Blocked", 8: "Approved", 15: "Detected", 17: "Logged"}

var aiRoles = map[int]string{1: "User", 2: "Assistant", 3: "Tool", 4: "Agent", 5: "Orchestrator"}

// String is the line's place in the record, for an error.
func (l Line) String() string { return fmt.Sprintf("%s/%d (%s)", l.SessionID, l.Seq, l.Type) }
