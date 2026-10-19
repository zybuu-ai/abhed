package ocsf

import (
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// AuditLink is a control-plane row's place in its chain, where the store
// chains it: the chain's name, the row's position, and its links.
type AuditLink struct {
	Chain string
	Pos   int64
	Prev  string
	Hash  string
}

// auditVerbs are API Activity's activity ids for the control plane's verbs.
var auditVerbs = map[string]int{"create": 1, "get": 2, "list": 2, "read": 2, "update": 3, "delete": 4, "remove": 4}

// methodActivities are API Activity's activity ids for an API request's method.
var methodActivities = map[string]int{"POST": 1, "GET": 2, "PUT": 3, "PATCH": 3, "DELETE": 4}

var activityNames = map[int]string{1: "Create", 2: "Read", 3: "Update", 4: "Delete", 99: "Other"}

// MapAudit maps one control-plane audit row (the sandbox API's record and
// the API's own audit of every mutating request, a record apart from the
// session record) to API Activity.
func MapAudit(a controlplane.AuditEvent, opts Options) Event {
	return MapAuditLink(a, nil, opts)
}

// MapAuditLink is MapAudit with the row's chain links, kept under
// unmapped.record as a session line's are.
func MapAuditLink(a controlplane.AuditEvent, link *AuditLink, opts Options) Event {
	m := NewMapper(opts)
	b := &builder{m: m, ev: Event{}, l: Line{Event: agent.Event{
		SessionID: a.SandboxID, Seq: a.Seq, Type: agent.EventType("controlplane." + a.Action),
		Actor: agent.ActorSystem, Trust: agent.Trusted, CreatedAt: a.At,
	}}}
	method, route, isAPI := apiAction(a.Action)
	act := 99
	switch {
	case isAPI:
		if id, ok := methodActivities[method]; ok {
			act = id
		}
	default:
		// The verb is the action itself or a word of it: create, template.create,
		// admin:user.delete.
		parts := strings.FieldsFunc(a.Action, func(r rune) bool { return r == '.' || r == '_' || r == ':' || r == ' ' })
		for i := len(parts) - 1; i >= 0; i-- {
			if id, ok := auditVerbs[strings.ToLower(parts[i])]; ok {
				act = id
				break
			}
		}
	}
	b.classify(6003, act, activityNames[act])
	user := map[string]any{"name": a.Principal, "type_id": 1, "type": "User"}
	if strings.HasPrefix(a.Principal, "system:") {
		user["type_id"], user["type"] = 3, "System"
	}
	b.set("actor", map[string]any{"user": user, "app_name": "Abhed"})
	b.profile("host")
	if isAPI {
		b.api(route, "")
		req := map[string]any{"url": map[string]any{"path": filledPath(route, a.Detail)}}
		if _, ok := httpActivities[method]; ok {
			req["http_method"] = method
		}
		b.set("http_request", req)
	} else {
		b.api(a.Action, "")
	}
	if ip := a.Detail["remote"]; ip != "" {
		b.set("src_endpoint", map[string]any{"ip": ip})
	}
	switch a.Outcome {
	case controlplane.OutcomeOK:
		b.status(1, "", "")
	case controlplane.OutcomeFailed:
		b.status(2, "", a.Code)
	default:
		b.status(99, caser(a.Outcome), "")
	}
	var resources []any
	if a.SandboxID != "" {
		resources = append(resources, map[string]any{"uid": a.SandboxID, "type": "sandbox"})
	}
	// What the request made, as its answer named it.
	var created []string
	for k := range a.Detail {
		if strings.HasPrefix(k, "created.") {
			created = append(created, k)
		}
	}
	sort.Strings(created)
	for _, k := range created {
		resources = append(resources, map[string]any{"uid": a.Detail[k], "type": strings.TrimPrefix(k, "created."),
			"role_id": 1, "role": "Target"})
	}
	if len(resources) > 0 {
		b.set("resources", resources)
	}
	b.set("message", strings.TrimSpace(a.Principal+" "+a.Action+" "+a.SandboxID+" "+a.Outcome))
	ev := b.finish()
	meta := ev["metadata"].(map[string]any)
	meta["log_name"] = "abhed.controlplane"
	if a.SandboxID != "" {
		meta["correlation_uid"] = a.SandboxID
	} else {
		delete(meta, "correlation_uid")
	}
	delete(meta, "uid")
	un := ev["unmapped"].(map[string]any)
	rec := map[string]any{"created_at": a.At.UTC().Format(time.RFC3339Nano)}
	if link != nil {
		rec["chain"], rec["pos"], rec["prev"], rec["hash"] = link.Chain, link.Pos, link.Prev, link.Hash
	}
	un["record"] = rec
	un["audit"] = map[string]any{"principal": a.Principal, "action": a.Action, "outcome": a.Outcome,
		"generation": a.Generation, "code": a.Code, "detail": a.Detail}
	return ev
}

// apiAction splits an API audit action, "api:POST /v1/sessions/{id}", into
// its method and route.
func apiAction(action string) (method, route string, ok bool) {
	route, ok = strings.CutPrefix(action, "api:")
	if !ok {
		return "", "", false
	}
	method, _, _ = strings.Cut(route, " ")
	return method, route, true
}

// filledPath is a route's path with the values its wildcards took.
func filledPath(route string, detail map[string]string) string {
	_, path, found := strings.Cut(route, " ")
	if !found {
		path = route
	}
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			name := strings.TrimSuffix(seg[1:len(seg)-1], "...")
			if v, ok := detail["path."+name]; ok {
				segs[i] = v
			}
		}
	}
	return strings.Join(segs, "/")
}

func caser(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
