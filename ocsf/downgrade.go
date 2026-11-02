package ocsf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Downgrade writes ev in an older schema version. What the older version does
// not define is moved, by its dotted path, under unmapped.downgraded, and an
// enum value it does not define becomes Other (99) where it has one, so the
// event still validates and nothing is lost.
func Downgrade(ev Event, version string) (Event, error) {
	out, err := normal(ev)
	if err != nil {
		return nil, err
	}
	meta, _ := out["metadata"].(map[string]any)
	from, _ := meta["version"].(string)
	if from == version {
		return out, nil
	}
	if !older(version, from) {
		return nil, fmt.Errorf("ocsf: %s is not older than %s; only a downgrade is written", version, from)
	}
	v, err := schema(version)
	if err != nil {
		return nil, err
	}
	uid, _ := intOf(out["class_uid"])
	c, ok := v.Classes[strconv.Itoa(uid)]
	if !ok {
		return nil, fmt.Errorf("ocsf: class %d is not in OCSF %s", uid, version)
	}
	dropped := map[string]any{}
	// HTTP Activity before 1.5 requires a response, which a decision on a
	// request has not got: it is written as the connection's Network Activity.
	if a, ok := c.Attributes["http_response"]; uid == 4002 && ok && a.Req == "required" && out["http_response"] == nil {
		dropped["class_uid"], dropped["activity_id"] = out["class_uid"], out["activity_id"]
		uid, c = 4001, v.Classes["4001"]
		out["class_uid"], out["class_name"] = uid, classNames[uid]
		if act, _ := intOf(out["action_id"]); act == 2 {
			out["activity_id"], out["activity_name"] = 5, "Refuse"
		} else {
			out["activity_id"], out["activity_name"] = 1, "Open"
		}
		out["type_name"] = classNames[uid] + ": " + out["activity_name"].(string)
	}
	prune(v, c.Attributes, out, "", dropped)
	if meta, ok := out["metadata"].(map[string]any); ok {
		meta["version"] = version
		if ps, ok := meta["profiles"].([]any); ok {
			var kept []any
			for _, p := range ps {
				if s, _ := p.(string); slices.Contains(c.Profiles, s) {
					kept = append(kept, s)
				} else {
					dropped["metadata.profiles"] = append(asList(dropped["metadata.profiles"]), s)
				}
			}
			if len(kept) == 0 {
				delete(meta, "profiles")
			} else {
				meta["profiles"] = kept
			}
		}
	}
	act, _ := intOf(out["activity_id"])
	out["type_uid"] = uid*100 + act
	if len(dropped) > 0 {
		un, _ := out["unmapped"].(map[string]any)
		if un == nil {
			un = map[string]any{}
			out["unmapped"] = un
		}
		un["downgraded"] = map[string]any{"from": from, "attributes": dropped}
	}
	return out, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// older reports whether version a is before b.
func older(a, b string) bool {
	ia, ib := slices.Index(Versions, a), slices.Index(Versions, b)
	return ia >= 0 && ib >= 0 && ia > ib
}

func prune(v versionDef, defs map[string]attrDef, obj map[string]any, path string, dropped map[string]any) {
	for name, val := range obj {
		if name == "unmapped" && path == "" {
			continue
		}
		a, ok := defs[name]
		if !ok {
			dropped[path+name] = val
			delete(obj, name)
			continue
		}
		switch {
		case a.Type == "object_t" && a.Object != "object":
			o, ok := v.Objects[a.Object]
			if !ok {
				dropped[path+name] = val
				delete(obj, name)
				continue
			}
			if a.Array {
				for i, item := range asList(val) {
					if m, ok := item.(map[string]any); ok {
						prune(v, o.Attributes, m, fmt.Sprintf("%s%s[%d].", path, name, i), dropped)
					}
				}
			} else if m, ok := val.(map[string]any); ok {
				prune(v, o.Attributes, m, path+name+".", dropped)
			}
		case len(a.Enum) > 0:
			n, isInt := intOf(val)
			if isInt && !slices.Contains(a.Enum, strconv.Itoa(n)) {
				dropped[path+name] = val
				if slices.Contains(a.Enum, "99") {
					obj[name] = 99
					if name == "activity_id" {
						obj["activity_name"] = "Other"
					}
				} else {
					delete(obj, name)
				}
			}
		}
	}
}

// Replay rebuilds the record line an event was mapped from: its id, session,
// seq, type, actor, trust, exact time, payload and chain links. A line whose
// payload was an object comes back in the record's canonical form, so its
// hash checks as it did in the record.
func Replay(ev Event) (Line, error) {
	n, err := normal(ev)
	if err != nil {
		return Line{}, err
	}
	meta, _ := n["metadata"].(map[string]any)
	un, _ := n["unmapped"].(map[string]any)
	rec, _ := un["record"].(map[string]any)
	if meta == nil || rec == nil {
		return Line{}, errors.New("ocsf: the event was not mapped from a record line")
	}
	var l Line
	l.ID, _ = meta["uid"].(string)
	l.SessionID, _ = meta["correlation_uid"].(string)
	t, _ := meta["event_code"].(string)
	l.Type = agent.EventType(t)
	seq, ok := intOf(meta["sequence"])
	if !ok {
		return Line{}, errors.New("ocsf: metadata.sequence is missing")
	}
	l.Seq = int64(seq)
	s := func(k string) string { v, _ := rec[k].(string); return v }
	l.ParentID, l.Prev, l.Hash = s("parent_id"), s("prev"), s("hash")
	l.Actor, l.Trust = agent.Actor(s("actor")), agent.Trust(s("trust"))
	if l.CreatedAt, err = time.Parse(time.RFC3339Nano, s("created_at")); err != nil {
		return Line{}, fmt.Errorf("ocsf: unmapped.record.created_at: %w", err)
	}
	switch {
	case un["payload"] != nil:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(un["payload"]); err != nil {
			return Line{}, err
		}
		l.Payload = bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	case un["payload_raw"] != nil:
		raw, _ := un["payload_raw"].(string)
		l.Payload = json.RawMessage(raw)
	}
	return l, nil
}

// ReplayJSON is Replay of one JSON line, numbers kept exactly as written.
func ReplayJSON(line []byte) (Line, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var ev map[string]any
	if err := dec.Decode(&ev); err != nil {
		return Line{}, err
	}
	return Replay(ev)
}
