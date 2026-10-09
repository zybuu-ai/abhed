package sandboxcli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
)

// sandboxPath is the REST path of a sandbox, with a verb when one is given.
func sandboxPath(id, verb string) string {
	p := "/v1/sandboxes/" + id
	if verb != "" {
		p += ":" + verb
	}
	return p
}

// oneID checks the arguments are exactly one sandbox id.
func oneID(pos []string, what string) (string, error) {
	if len(pos) != 1 {
		return "", usageError{fmt.Errorf("%s takes one sandbox id", what)}
	}
	if !controlplane.ValidID(pos[0]) {
		return "", usageError{fmt.Errorf("%q is not a sandbox id (sbx_ and 16 hex characters)", pos[0])}
	}
	return pos[0], nil
}

// settingsFlags are the settings shortcuts create and template create share.
type settingsFlags struct {
	set              multi
	minTier, cpus    string
	memoryMB         int
	labels, unlabels multi
}

func (s *settingsFlags) values() (map[string]string, error) {
	out, err := pairs("set", s.set)
	if err != nil {
		return nil, err
	}
	put := func(k, v string) {
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	if s.minTier != "" {
		put(controlplane.SetMinTier, s.minTier)
	}
	if s.cpus != "" {
		put(controlplane.SetCPUs, s.cpus)
	}
	if s.memoryMB > 0 {
		put(controlplane.SetMemoryMB, strconv.Itoa(s.memoryMB))
	}
	return out, nil
}

func sandboxRow(tw *tabwriter.Writer, sb controlplane.Sandbox) {
	tier := sb.Tier
	if tier == "" {
		tier = "-"
	}
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", sb.ID, clean(string(sb.State), 0), clean(tier, 0), clean(sb.Driver, 0),
		clean(sb.Template, 0), joinMap(sb.Labels), age(sb.CreatedAt))
}

const sandboxHeader = "ID\tSTATE\tTIER\tDRIVER\tTEMPLATE\tLABELS\tAGE\n"

func (r *runner) create(args []string) error {
	fs := r.flags("create", "[flags]")
	template := fs.String("template", "default", "the template to create it from")
	drv := fs.String("driver", "", "the driver (default: the template's, else the server's)")
	var sf settingsFlags
	fs.Var(&sf.labels, "l", "a label key=value (repeatable)")
	fs.Var(&sf.set, "set", "a setting key=value (repeatable), such as exec.timeout_seconds=60")
	fs.StringVar(&sf.minTier, "min-tier", "", "the weakest isolation acceptable: process, fence, container, vm or microvm")
	fs.StringVar(&sf.cpus, "cpus", "", "CPUs, such as 0.5 or 2")
	fs.IntVar(&sf.memoryMB, "memory-mb", 0, "memory in MiB")
	wait := fs.Bool("wait", false, "wait until it is Ready")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long -wait waits")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{errors.New("create takes no arguments; name the template with -template")}
	}
	labels, err := pairs("l", sf.labels)
	if err != nil {
		return err
	}
	settings, err := sf.values()
	if err != nil {
		return err
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	if *template != "default" {
		// A typo would otherwise make a sandbox from nothing but a name.
		_, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: "/v1/sandbox-templates/" + url.PathEscape(*template)}, nil)
		var ae *apiError
		switch {
		case errors.As(err, &ae) && ae.status == http.StatusNotFound:
			return &apiError{status: http.StatusNotFound, code: "not_found",
				msg: fmt.Sprintf("the server has no template %q; abhed sandbox template list shows those it has", *template)}
		case errors.As(err, &ae) && ae.status == http.StatusNotImplemented:
		case err != nil:
			return err
		}
	}
	body, _ := json.Marshal(controlplane.CreateRequest{Template: *template, Driver: *drv, Labels: labels, Settings: settings})
	var sb controlplane.Sandbox
	if _, err := r.cl.json(r.Ctx, call{method: http.MethodPost, path: "/v1/sandboxes", body: body, mutating: true}, &sb); err != nil {
		return err
	}
	if *wait {
		if sb, err = r.waitFor(sb.ID, *timeout, controlplane.StateReady); err != nil {
			return err
		}
	}
	return r.emit(sb, func(tw *tabwriter.Writer) {
		fmt.Fprint(tw, sandboxHeader)
		sandboxRow(tw, sb)
	})
}

// waitFor polls a sandbox until it is in want, or fails when it errs.
func (r *runner) waitFor(id string, timeout time.Duration, want controlplane.State) (controlplane.Sandbox, error) {
	deadline := time.Now().Add(timeout)
	for {
		var sb controlplane.Sandbox
		if _, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: sandboxPath(id, "")}, &sb); err != nil {
			return sb, err
		}
		switch sb.State {
		case want:
			return sb, nil
		case controlplane.StateError:
			why := ""
			for _, c := range sb.Conditions {
				if c.Status == controlplane.ConditionFalse && c.Message != "" {
					why = ": " + c.Message
				}
			}
			return sb, fmt.Errorf("sandbox %s failed%s", id, why)
		}
		if time.Now().After(deadline) {
			return sb, fmt.Errorf("sandbox %s is still %s after %s", id, sb.State, timeout)
		}
		select {
		case <-r.Ctx.Done():
			return sb, r.Ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type sandboxList struct {
	Items []controlplane.Sandbox `json:"items"`
	Next  string                 `json:"next,omitempty"`
}

func (r *runner) list(args []string) error {
	fs := r.flags("list", "[flags]")
	sel := fs.String("l", "", "a label selector, such as team=a,env!=prod")
	state := fs.String("state", "", "only sandboxes in this state, such as Ready")
	limit := fs.Int("limit", 0, "at most this many (default: the server's page)")
	after := fs.String("after", "", "the cursor a previous page printed")
	all := fs.Bool("all", false, "read every page")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{errors.New("list takes no arguments")}
	}
	if _, err := controlplane.ParseSelector(*sel); err != nil {
		return usageError{err}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	var out sandboxList
	cursor := *after
	for {
		q := url.Values{}
		for k, v := range map[string]string{"selector": *sel, "state": *state, "after": cursor} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		var page sandboxList
		if _, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: "/v1/sandboxes?" + q.Encode()}, &page); err != nil {
			return err
		}
		out.Items = append(out.Items, page.Items...)
		out.Next = page.Next
		if !*all || page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if out.Items == nil {
		out.Items = []controlplane.Sandbox{}
	}
	err = r.emit(out, func(tw *tabwriter.Writer) {
		fmt.Fprint(tw, sandboxHeader)
		for _, sb := range out.Items {
			sandboxRow(tw, sb)
		}
	})
	if r.out == "table" && out.Next != "" {
		fmt.Fprintf(r.Err, "more: -after %s\n", out.Next)
	}
	return err
}

func (r *runner) getSandbox(id string) (controlplane.Sandbox, string, error) {
	var sb controlplane.Sandbox
	etag, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: sandboxPath(id, "")}, &sb)
	return sb, etag, err
}

func (r *runner) get(args []string) error {
	fs := r.flags("get", "ID")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	id, err := oneID(pos, "get")
	if err != nil {
		return err
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	sb, _, err := r.getSandbox(id)
	if err != nil {
		return err
	}
	return r.emit(sb, func(tw *tabwriter.Writer) { sandboxDetail(tw, sb) })
}

func sandboxDetail(tw *tabwriter.Writer, sb controlplane.Sandbox) {
	row := func(k, v string) { fmt.Fprintf(tw, "%s:\t%s\n", clean(k, 0), clean(v, 0)) }
	row("ID", sb.ID)
	row("State", string(sb.State))
	row("Template", sb.Template)
	row("Driver", sb.Driver)
	row("Tier", orDash(sb.Tier))
	row("Owner", sb.Owner)
	row("Generation", strconv.FormatInt(sb.Generation, 10))
	row("Version", strconv.FormatInt(sb.ResourceVersion, 10))
	row("Labels", joinMap(sb.Labels))
	row("Settings", joinMap(sb.Settings))
	row("Created", sb.CreatedAt.Format(time.RFC3339))
	if sb.Restarts > 0 {
		row("Restarts", strconv.Itoa(sb.Restarts))
	}
	for _, c := range sb.Conditions {
		msg := c.Reason
		if c.Message != "" {
			msg += ": " + c.Message
		}
		row("Condition "+c.Type, string(c.Status)+" "+msg)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (r *runner) update(args []string) error {
	fs := r.flags("update", "ID [flags]")
	var labels, unlabels, set, unset multi
	fs.Var(&labels, "l", "set a label key=value (repeatable)")
	fs.Var(&unlabels, "unlabel", "remove a label (repeatable)")
	fs.Var(&set, "set", "set a setting key=value (repeatable)")
	fs.Var(&unset, "unset", "remove a setting (repeatable)")
	template := fs.String("template", "", "change the template; it takes effect at the next start")
	ifMatch := fs.String("if-match", "", "change it only at this version (ETag or resource_version); default: the version just read")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	id, err := oneID(pos, "update")
	if err != nil {
		return err
	}
	patch := map[string]any{}
	addMap := func(key string, sets multi, removes multi) error {
		kv, err := pairs(key, sets)
		if err != nil {
			return err
		}
		if len(kv) == 0 && len(removes) == 0 {
			return nil
		}
		m := map[string]any{}
		for k, v := range kv {
			m[k] = v
		}
		for _, k := range removes {
			m[k] = nil
		}
		patch[map[string]string{"l": "labels", "set": "settings"}[key]] = m
		return nil
	}
	if err := addMap("l", labels, unlabels); err != nil {
		return err
	}
	if err := addMap("set", set, unset); err != nil {
		return err
	}
	if *template != "" {
		patch["template"] = *template
	}
	if len(patch) == 0 {
		return usageError{errors.New("update needs -l, -unlabel, -set, -unset or -template")}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	etag := etagOf(*ifMatch)
	if etag == "" {
		if _, etag, err = r.getSandbox(id); err != nil {
			return err
		}
	}
	body, _ := json.Marshal(patch)
	var sb controlplane.Sandbox
	_, err = r.cl.json(r.Ctx, call{method: http.MethodPatch, path: sandboxPath(id, ""), body: body, mutating: true,
		contentType: "application/merge-patch+json", header: http.Header{"If-Match": {etag}}}, &sb)
	if ae := new(apiError); errors.As(err, &ae) && ae.status == http.StatusPreconditionFailed {
		return &apiError{status: ae.status, code: ae.code, msg: "the sandbox changed since " + etag + "; read it again and retry"}
	}
	if err != nil {
		return err
	}
	return r.emit(sb, func(tw *tabwriter.Writer) { sandboxDetail(tw, sb) })
}

// etagOf accepts a resource version or an entity tag.
func etagOf(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, `"`) || v == "*" {
		return v
	}
	return `"` + v + `"`
}

// lifecycle runs a verb that answers with the sandbox.
type lifecycleOpts struct {
	flags  func(fs *flag.FlagSet)
	body   func() any
	before func(id string) error
}

func (r *runner) lifecycle(name string, args []string, o lifecycleOpts) error {
	fs := r.flags(name, "ID")
	ifMatch := fs.String("if-match", "", "act only at this version (ETag or resource_version)")
	if o.flags != nil {
		o.flags(fs)
	}
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	id, err := oneID(pos, name)
	if err != nil {
		return err
	}
	if o.before != nil {
		if err := o.before(id); err != nil {
			return err
		}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	c := call{method: http.MethodPost, path: sandboxPath(id, name), mutating: true}
	if name == "delete" {
		c = call{method: http.MethodDelete, path: sandboxPath(id, ""), mutating: true}
	}
	if o.body != nil {
		c.body, _ = json.Marshal(o.body())
	}
	if e := etagOf(*ifMatch); e != "" {
		c.header = http.Header{"If-Match": {e}}
	}
	var sb controlplane.Sandbox
	if _, err := r.cl.json(r.Ctx, c, &sb); err != nil {
		return err
	}
	return r.emit(sb, func(tw *tabwriter.Writer) {
		fmt.Fprint(tw, sandboxHeader)
		sandboxRow(tw, sb)
	})
}

func (r *runner) stop(args []string) error {
	var grace *int
	return r.lifecycle("stop", args, lifecycleOpts{
		flags: func(fs *flag.FlagSet) { grace = fs.Int("grace", -1, "seconds the workload has to stop (default: 10)") },
		body: func() any {
			if *grace < 0 {
				return map[string]any{}
			}
			return map[string]int{"grace_seconds": *grace}
		}})
}

func (r *runner) start(args []string) error { return r.lifecycle("start", args, lifecycleOpts{}) }

func (r *runner) delete(args []string) error {
	var yes *bool
	return r.lifecycle("delete", args, lifecycleOpts{
		flags: func(fs *flag.FlagSet) { yes = fs.Bool("y", false, "do not ask first") },
		before: func(id string) error {
			if *yes {
				return nil
			}
			return r.confirm("delete", "Delete sandbox " + id + " and everything in it?")
		}})
}

func (r *runner) snapshot(args []string) error {
	fs := r.flags("snapshot", "ID [-name NAME]")
	name := fs.String("name", "", "the snapshot's name")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	id, err := oneID(pos, "snapshot")
	if err != nil {
		return err
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"name": *name})
	var res struct {
		Snapshot   driver.SnapshotRef `json:"snapshot"`
		Generation int64              `json:"generation"`
	}
	if _, err := r.cl.json(r.Ctx, call{method: http.MethodPost, path: sandboxPath(id, "snapshot"), body: body, mutating: true, timeout: 30 * time.Minute}, &res); err != nil {
		return err
	}
	return r.emit(res, func(tw *tabwriter.Writer) {
		fmt.Fprintln(tw, "SNAPSHOT\tNAME\tDIGEST\tBYTES\tGENERATION")
		s := res.Snapshot
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", s.ID, orDash(s.Name), s.Digest, s.Bytes, res.Generation)
	})
}

func (r *runner) restore(args []string) error {
	fs := r.flags("restore", "ID (-f FILE | SNAPSHOT_ID -digest DIGEST)")
	file := fs.String("f", "", "the snapshot as abhed sandbox snapshot -o json printed it (- for stdin)")
	digest := fs.String("digest", "", "the snapshot's digest, with SNAPSHOT_ID")
	ifMatch := fs.String("if-match", "", "restore only at this version")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageError{errors.New("restore takes a sandbox id")}
	}
	id, err := oneID(pos[:1], "restore")
	if err != nil {
		return err
	}
	var ref driver.SnapshotRef
	switch {
	case *file != "" && len(pos) == 1:
		var raw []byte
		if *file == "-" {
			raw, err = io.ReadAll(io.LimitReader(r.In, 1<<20))
		} else {
			raw, err = os.ReadFile(*file) // #nosec G304 -- a file the person named
		}
		if err != nil {
			return err
		}
		var wrapped struct {
			Snapshot *driver.SnapshotRef `json:"snapshot"`
		}
		if json.Unmarshal(raw, &wrapped) == nil && wrapped.Snapshot != nil {
			ref = *wrapped.Snapshot
		} else if err := json.Unmarshal(raw, &ref); err != nil {
			return usageError{fmt.Errorf("%s holds no snapshot: %w", *file, err)}
		}
	case *file == "" && len(pos) == 2 && *digest != "":
		ref = driver.SnapshotRef{ID: pos[1], Digest: *digest}
	default:
		return usageError{errors.New("restore needs -f FILE, or SNAPSHOT_ID with -digest")}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"snapshot": ref})
	c := call{method: http.MethodPost, path: sandboxPath(id, "restore"), body: body, mutating: true, timeout: 30 * time.Minute}
	if e := etagOf(*ifMatch); e != "" {
		c.header = http.Header{"If-Match": {e}}
	}
	var sb controlplane.Sandbox
	if _, err := r.cl.json(r.Ctx, c, &sb); err != nil {
		return err
	}
	return r.emit(sb, func(tw *tabwriter.Writer) {
		fmt.Fprint(tw, sandboxHeader)
		sandboxRow(tw, sb)
	})
}

func (r *runner) expose(args []string) error {
	fs := r.flags("expose", "ID PORT")
	ttl := fs.Duration("ttl", time.Hour, "how long the URL lasts")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError{errors.New("expose takes a sandbox id and a port")}
	}
	id, err := oneID(pos[:1], "expose")
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(pos[1])
	if err != nil || port < 1 || port > 65535 {
		return usageError{fmt.Errorf("port %q: a number from 1 to 65535", pos[1])}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]int{"port": port, "ttl_seconds": int(ttl.Seconds())})
	var res struct {
		ID        string    `json:"id"`
		URL       string    `json:"url"`
		Path      string    `json:"path"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := r.cl.json(r.Ctx, call{method: http.MethodPost, path: sandboxPath(id, "expose"), body: body, mutating: true}, &res); err != nil {
		return err
	}
	return r.emit(res, func(tw *tabwriter.Writer) {
		fmt.Fprintln(tw, "URL\tEXPIRES")
		fmt.Fprintf(tw, "%s\t%s\n", res.URL, res.ExpiresAt.Local().Format(time.RFC3339))
	})
}

type eventsPage struct {
	Items []controlplane.AuditEvent `json:"items"`
	Next  int64                     `json:"next,omitempty"`
}

func (r *runner) logs(args []string) error {
	fs := r.flags("logs", "ID")
	after := fs.Int64("after", 0, "only events after this sequence number")
	limit := fs.Int("limit", 0, "at most this many (default: every event)")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	id, err := oneID(pos, "logs")
	if err != nil {
		return err
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	var out eventsPage
	cursor := *after
	for {
		q := url.Values{"after": {strconv.FormatInt(cursor, 10)}}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit-len(out.Items)))
		}
		var page eventsPage
		if _, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: sandboxPath(id, "events") + "?" + q.Encode()}, &page); err != nil {
			return err
		}
		out.Items = append(out.Items, page.Items...)
		if page.Next == 0 || (*limit > 0 && len(out.Items) >= *limit) {
			out.Next = page.Next
			break
		}
		cursor = page.Next
	}
	if out.Items == nil {
		out.Items = []controlplane.AuditEvent{}
	}
	return r.emit(out, func(tw *tabwriter.Writer) {
		fmt.Fprintln(tw, "SEQ\tTIME\tPRINCIPAL\tACTION\tGEN\tOUTCOME\tDETAIL")
		for _, ev := range out.Items {
			outcome := ev.Outcome
			if ev.Code != "" {
				outcome += " (" + ev.Code + ")"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%s\t%s\n", ev.Seq, ev.At.Local().Format(time.RFC3339), clean(ev.Principal, 0),
				clean(ev.Action, 0), ev.Generation, clean(outcome, 0), joinMap(ev.Detail))
		}
	})
}
