package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// A k8s_apply call is planned and dry-run before anyone is asked: the cluster
// says what the write would do, the approval shows the difference from the
// live object, and the request it checked is the one sent once approved.

// dryRunTimeout bounds the dry run, which runs before the approval with no
// call to stop it.
const dryRunTimeout = 30 * time.Second

// plannedFor is how long a planned request waits for its approval.
const plannedFor = 30 * time.Minute

// maxDiffLines is how many changed fields the approval names.
const maxDiffLines = 20

// planned is one call's checked request and what its dry run showed.
type planned struct {
	req     *request
	preview string
	at      time.Time
}

// plans are a session's planned requests, keyed by the call's arguments.
type plans struct {
	mu sync.Mutex
	m  map[[32]byte]planned
}

type plansKey struct{ m *Manager }

func (m *Manager) plans(sess *tools.Session, create bool) *plans {
	var mk func() any
	if create {
		mk = func() any { return &plans{m: map[[32]byte]planned{}} }
	}
	p, _ := sess.Scoped(plansKey{m}, mk).(*plans)
	return p
}

func (m *Manager) keep(sess *tools.Session, raw json.RawMessage, p planned) {
	ps := m.plans(sess, true)
	if ps == nil {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for k, old := range ps.m {
		if time.Since(old.at) > plannedFor {
			delete(ps.m, k)
		}
	}
	p.at = time.Now()
	ps.m[sha256.Sum256(raw)] = p
}

func (m *Manager) peek(sess *tools.Session, raw json.RawMessage) (planned, bool) {
	ps := m.plans(sess, false)
	if ps == nil {
		return planned{}, false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.m[sha256.Sum256(raw)]
	if !ok || time.Since(p.at) > plannedFor {
		return planned{}, false
	}
	return p, true
}

// takePlanned returns, once, the request planned for these arguments.
func (m *Manager) takePlanned(sess *tools.Session, raw json.RawMessage) *request {
	ps := m.plans(sess, false)
	if ps == nil {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	key := sha256.Sum256(raw)
	p, ok := ps.m[key]
	delete(ps.m, key)
	if !ok || time.Since(p.at) > plannedFor {
		return nil
	}
	return p.req
}

// Precheck plans the call and has the cluster dry-run it. A dry run the
// cluster refuses refuses the call before anyone is asked, with the
// cluster's reason; nothing is applied. A call that cannot be planned is left
// to Run, which says why.
func (t ApplyTool) Precheck(sess *tools.Session, raw json.RawMessage) error {
	if t.M == nil {
		return nil
	}
	var a applyArgs
	if json.Unmarshal(raw, &a) != nil {
		return nil
	}
	c, err := t.M.cluster(sess, a.Cluster, a.Context)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dryRunTimeout)
	defer cancel()
	req, err := t.plan(ctx, c, a)
	if err != nil {
		return nil
	}
	preview, err := dryRun(ctx, c, req)
	if err != nil {
		return fmt.Errorf("the cluster's dry run of this change failed, so it was not applied: %w", err)
	}
	t.M.keep(sess, raw, planned{req: req, preview: preview})
	return nil
}

// dryRun sends req as a server-side dry run and describes what it would change.
func dryRun(ctx context.Context, c *Cluster, req *request) (string, error) {
	var before map[string]any
	readErr := error(nil)
	if req.live != "" {
		data, err := c.Do(ctx, "GET", req.live, nil)
		switch {
		case errors.Is(err, errNotFound):
		case err != nil:
			readErr = err
		default:
			if json.Unmarshal(data, &before) != nil {
				readErr = errors.New("the live object is not JSON")
			}
		}
	}
	data, err := c.send(ctx, req, true)
	if err != nil {
		return "", err
	}
	if req.method == "DELETE" {
		return "dry run: the cluster accepts the delete", nil
	}
	var after map[string]any
	if json.Unmarshal(data, &after) != nil {
		return "dry run: the cluster accepts the change (its answer could not be read for a diff)", nil
	}
	switch {
	case readErr != nil:
		return "dry run: the cluster accepts the change; the live object could not be read for a diff (" +
			oneLine(readErr.Error(), 160) + ")", nil
	case before == nil:
		return "dry run: creates " + describe(after), nil
	}
	return "dry run: " + diffSummary(before, after), nil
}

// describe names an object as kind namespace/name.
func describe(obj map[string]any) string {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if ns, _ := meta["namespace"].(string); ns != "" {
		name = ns + "/" + name
	}
	return strings.TrimSpace(kind + " " + name)
}

// diffSummary is the change from before to after, field by field, on one
// line. A Secret's values are masked first, so only their lengths and
// hashes can show.
func diffSummary(before, after map[string]any) string {
	if isSecret(before) || isSecret(after) {
		maskSecret(before)
		maskSecret(after)
	}
	for _, obj := range []map[string]any{before, after} {
		delete(obj, "status")
		if meta, ok := obj["metadata"].(map[string]any); ok {
			for _, noise := range []string{"managedFields", "resourceVersion", "uid",
				"generation", "selfLink", "creationTimestamp"} {
				delete(meta, noise)
			}
		}
	}
	var lines []string
	diffValue("", before, after, &lines)
	if len(lines) == 0 {
		return "no change to the live object"
	}
	n := len(lines)
	shown := lines
	if n > maxDiffLines {
		shown = lines[:maxDiffLines]
	}
	out := fmt.Sprintf("%d field(s) change: %s", n, strings.Join(shown, "; "))
	if n > maxDiffLines {
		out += fmt.Sprintf("; and %d more", n-maxDiffLines)
	}
	return out
}

// diffValue appends a line for each field that differs between a and b.
func diffValue(path string, a, b any, out *[]string) {
	am, aMap := a.(map[string]any)
	bm, bMap := b.(map[string]any)
	if aMap && bMap {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			av, inA := am[k]
			bv, inB := bm[k]
			p := join(path, k)
			switch {
			case !inA:
				*out = append(*out, "+ "+p+" = "+show(bv))
			case !inB:
				*out = append(*out, "- "+p)
			default:
				diffValue(p, av, bv, out)
			}
		}
		return
	}
	al, aList := a.([]any)
	bl, bList := b.([]any)
	if aList && bList && len(al) == len(bl) {
		for i := range al {
			diffValue(fmt.Sprintf("%s[%d]", path, i), al[i], bl[i], out)
		}
		return
	}
	if show(a) != show(b) {
		*out = append(*out, path+": "+show(a)+" → "+show(b))
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// show is a value as compact JSON, cut to fit a line.
func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return oneLine(string(b), 120)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
