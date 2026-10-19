package hawkeye

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/ui"
)

// RemoteTarget is one host or cluster the session changed, with every
// command or write it was sent there, in order.
type RemoteTarget struct {
	// Kind is ssh for a host, k8s for a cluster.
	Kind string `json:"kind"`
	// Name is the host, or the cluster, as the call named it.
	Name    string         `json:"name"`
	Changes []RemoteChange `json:"changes"`
}

// RemoteChange is one remote command or cluster write, and how it ended.
type RemoteChange struct {
	Seq    int64  `json:"seq"`
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// What is the command, or the cluster write's action and object.
	What      string `json:"what"`
	Namespace string `json:"namespace,omitempty"`
	// Where is the address or server and credential the approval named.
	Where    string `json:"where,omitempty"`
	Decision string `json:"decision"`
	By       string `json:"by,omitempty"`
	Ran      bool   `json:"ran"`
	ExitCode *int   `json:"exit_code,omitempty"`
	IsError  bool   `json:"is_error"`
	// PlanHash and PlanStep name the approved runbook step the call was, if any.
	PlanHash string `json:"plan_hash,omitempty"`
	PlanStep int    `json:"plan_step,omitempty"`
}

// remoteTools are the calls that change something outside the session.
var remoteTools = map[string]string{"ssh": "ssh", "k8s_apply": "k8s"}

// remoteOf reads the host or cluster, namespace and what a remote call runs
// from its full arguments, before they are clipped for the report.
func remoteOf(tool string, args json.RawMessage) (name, ns, what string) {
	var a map[string]any
	if json.Unmarshal(args, &a) != nil {
		return "", "", ""
	}
	s := func(k string) string { v, _ := a[k].(string); return v }
	switch tool {
	case "ssh":
		return s("host"), "", s("command")
	case "k8s_apply":
		name = s("cluster")
		if name == "" {
			name = s("context")
		}
		what = s("action")
		switch {
		case s("name") != "":
			what += " " + s("resource") + "/" + s("name")
		case s("manifest") != "":
			what += " " + manifestName(s("manifest"))
		}
		if n, ok := a["replicas"].(float64); ok {
			what += " replicas=" + strconv.Itoa(int(n))
		}
		return name, s("namespace"), strings.TrimSpace(what)
	}
	return "", "", ""
}

// manifestName is a manifest's kind/name, for a line in the report.
func manifestName(m string) string {
	var o struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(m), &o) != nil || o.Kind == "" {
		return "manifest"
	}
	return strings.ToLower(o.Kind) + "/" + o.Metadata.Name
}

// remoteInfo is what a remote call's full arguments said, kept by call id.
type remoteInfo struct{ name, ns, what, where string }

// remoteChanges groups the session's remote calls by host and cluster, in
// the order each was first reached.
func remoteChanges(calls []Call, remote map[string]remoteInfo) []RemoteTarget {
	var out []RemoteTarget
	at := map[string]int{}
	for _, c := range calls {
		kind, ok := remoteTools[c.Tool]
		if !ok || c.Actor == "user" {
			continue
		}
		info := remote[c.CallID]
		rc := RemoteChange{Seq: c.Seq, CallID: c.CallID, Tool: c.Tool, What: info.what, Namespace: info.ns, Where: info.where,
			Decision: c.Decision, By: c.By, Ran: c.Ran, ExitCode: c.ExitCode, IsError: c.IsError, PlanHash: c.PlanHash, PlanStep: c.PlanStep}
		name := info.name
		key := kind + "\x00" + name
		i, seen := at[key]
		if !seen {
			i = len(out)
			at[key] = i
			out = append(out, RemoteTarget{Kind: kind, Name: name})
		}
		out[i].Changes = append(out[i].Changes, rc)
	}
	for i := range out {
		sort.SliceStable(out[i].Changes, func(a, b int) bool { return out[i].Changes[a].Seq < out[i].Changes[b].Seq })
	}
	return out
}

// shownRemote makes every record-derived string of the section visible.
func shownRemote(in []RemoteTarget) []RemoteTarget {
	v := ui.Visible
	return each(in, func(t RemoteTarget) RemoteTarget {
		t.Kind, t.Name = v(t.Kind), v(t.Name)
		t.Changes = each(t.Changes, func(c RemoteChange) RemoteChange {
			c.CallID, c.Tool, c.What, c.Namespace, c.Where = v(c.CallID), v(c.Tool), v(c.What), v(c.Namespace), v(c.Where)
			c.Decision, c.By, c.PlanHash = v(c.Decision), v(c.By), v(c.PlanHash)
			return c
		})
		return t
	})
}
