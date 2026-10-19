package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/kubescope"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// revisionKey is the annotation a Deployment and its ReplicaSets carry their
// rollout revision in.
const revisionKey = "deployment.kubernetes.io/revision"

// rollback plans what kubectl rollout undo does: the Deployment's pod
// template set back to an earlier ReplicaSet's. The patch first tests the
// Deployment's resourceVersion, so a Deployment changed since the dry run and
// the approval is refused, not rolled back to a revision no one saw.
func (t ApplyTool) rollback(ctx context.Context, c *Cluster, a applyArgs) (*request, error) {
	if kubescope.Resource(a.Resource) != "deployments" || a.Name == "" {
		return nil, fmt.Errorf("rollback needs resource deployments and the Deployment's name")
	}
	path, err := resourcePath(c, "deployments", a.Namespace, a.Name)
	if err != nil {
		return nil, err
	}
	ns := a.Namespace
	if ns == "" {
		ns = c.Namespace
	}
	if ns == "*" {
		return nil, fmt.Errorf("rollback needs a namespace, not '*'")
	}
	data, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Metadata struct {
			UID             string            `json:"uid"`
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Paused   bool `json:"paused"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("the Deployment could not be read: %w", err)
	}
	if d.Spec.Paused {
		return nil, fmt.Errorf("deployment %s is paused; a paused Deployment is not rolled back", a.Name)
	}
	current, _ := strconv.Atoi(d.Metadata.Annotations[revisionKey])

	rsPath := "/apis/apps/v1/namespaces/" + url.PathEscape(ns) + "/replicasets"
	if sel := selectorOf(d.Spec.Selector.MatchLabels); sel != "" {
		rsPath += "?labelSelector=" + urlEscape(sel)
	}
	data, err = c.Do(ctx, "GET", rsPath, nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				Annotations     map[string]string `json:"annotations"`
				OwnerReferences []struct {
					UID string `json:"uid"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Template map[string]any `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("the Deployment's ReplicaSets could not be read: %w", err)
	}
	type revision struct {
		n        int
		rs       string
		template map[string]any
	}
	var revs []revision
	for _, rs := range list.Items {
		owned := false
		for _, o := range rs.Metadata.OwnerReferences {
			owned = owned || (o.UID != "" && o.UID == d.Metadata.UID)
		}
		n, err := strconv.Atoi(rs.Metadata.Annotations[revisionKey])
		if owned && err == nil && rs.Spec.Template != nil {
			revs = append(revs, revision{n, rs.Metadata.Name, rs.Spec.Template})
		}
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].n > revs[j].n })
	var to *revision
	for i := range revs {
		r := &revs[i]
		if (a.Revision > 0 && r.n == a.Revision) || (a.Revision <= 0 && r.n < current) {
			to = r
			break
		}
	}
	switch {
	case a.Revision > 0 && a.Revision == current:
		return nil, fmt.Errorf("deployment %s is already at revision %d", a.Name, current)
	case to == nil && a.Revision > 0:
		have := make([]int, 0, len(revs))
		for _, r := range revs {
			have = append(have, r.n)
		}
		return nil, fmt.Errorf("deployment %s has no revision %d to roll back to (it has %s)", a.Name, a.Revision, revisionList(have))
	case to == nil:
		return nil, fmt.Errorf("deployment %s has no revision before %d to roll back to", a.Name, current)
	}

	// The template as the ReplicaSet runs it, less the hash the controller adds.
	tmpl := to.template
	if meta, ok := tmpl["metadata"].(map[string]any); ok {
		if labels, ok := meta["labels"].(map[string]any); ok {
			delete(labels, "pod-template-hash")
		}
	}
	body, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": d.Metadata.ResourceVersion},
		{"op": "replace", "path": "/spec/template", "value": tmpl},
	})
	if err != nil {
		return nil, err
	}
	from, target, rsName := current, to.n, to.rs
	return &request{method: "PATCH", path: path, body: body, contentType: "application/json-patch+json",
		live: path, hint: " (the rollback was not applied; if the Deployment changed since it was approved, ask again)",
		done: func([]byte) string {
			return fmt.Sprintf("Rolled deployment %s back from revision %d to revision %d (ReplicaSet %s's template). "+
				"Wait for the rollout with k8s_get deployments %s and wait_seconds.", a.Name, from, target, rsName, a.Name)
		}}, nil
}

func selectorOf(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

func revisionList(revs []int) string {
	if len(revs) == 0 {
		return "none"
	}
	out := make([]string, 0, len(revs))
	for _, n := range revs {
		out = append(out, strconv.Itoa(n))
	}
	return strings.Join(out, ", ")
}

// ---------------------------------------------------------------- waiting

// maxWait bounds how long one k8s_get waits for a rollout.
const maxWait = 10 * time.Minute

// waitPoll is how often a waiting k8s_get reads the Deployment again.
var waitPoll = 2 * time.Second

type rolloutState struct {
	Metadata struct {
		Generation  int64             `json:"generation"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int   `json:"replicas"`
		UpdatedReplicas    int   `json:"updatedReplicas"`
		AvailableReplicas  int   `json:"availableReplicas"`
		Conditions         []struct {
			Type    string `json:"type"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// progress reads a Deployment's rollout as kubectl rollout status does:
// done, failed, or what it is still waiting for.
func (s rolloutState) progress() (done, failed bool, what string) {
	want := 1
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	st := s.Status
	counts := fmt.Sprintf("%d/%d updated, %d available, %d in all", st.UpdatedReplicas, want, st.AvailableReplicas, st.Replicas)
	if s.Metadata.Generation > st.ObservedGeneration {
		return false, false, "the controller has not yet seen the latest change (" + counts + ")"
	}
	for _, c := range st.Conditions {
		if c.Type == "Progressing" && c.Reason == "ProgressDeadlineExceeded" {
			return false, true, "the rollout exceeded its progress deadline: " + oneLine(c.Message, 200) + " (" + counts + ")"
		}
	}
	switch {
	case st.UpdatedReplicas < want:
		return false, false, "waiting for updated replicas (" + counts + ")"
	case st.Replicas > st.UpdatedReplicas:
		return false, false, "waiting for old replicas to end (" + counts + ")"
	case st.AvailableReplicas < st.UpdatedReplicas:
		return false, false, "waiting for updated replicas to become available (" + counts + ")"
	}
	return true, false, counts
}

// wait reads a Deployment until its rollout finishes, fails or the wait
// ends, and says which.
func (t GetTool) wait(ctx context.Context, c *Cluster, a getArgs) tools.Result {
	if kubescope.Resource(a.Resource) != "deployments" || a.Name == "" {
		return errf("wait_seconds waits for one Deployment's rollout: give resource deployments and its name.")
	}
	path, err := resourcePath(c, "deployments", a.Namespace, a.Name)
	if err != nil {
		return errf("%v", err)
	}
	limit := time.Duration(a.WaitSeconds) * time.Second
	if limit > maxWait {
		limit = maxWait
	}
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	what, rev := "it had not been read yet", ""
	for {
		data, err := c.Do(wctx, "GET", path, nil)
		if err != nil && (wctx.Err() == nil || ctx.Err() != nil) {
			return errf("%v", err)
		}
		if err == nil {
			var s rolloutState
			if err := json.Unmarshal(data, &s); err != nil {
				return errf("the Deployment could not be read: %v", err)
			}
			done, failed, now := s.progress()
			what, rev = now, s.Metadata.Annotations[revisionKey]
			took := time.Since(start).Round(time.Second)
			switch {
			case done:
				return tools.Result{Content: fmt.Sprintf("Deployment %s rolled out (revision %s) after %s: %s.", a.Name, rev, took, what)}
			case failed:
				return errf("Deployment %s did not roll out (revision %s) after %s: %s.", a.Name, rev, took, what)
			}
		}
		select {
		case <-wctx.Done():
		case <-time.After(waitPoll):
			continue
		}
		if ctx.Err() != nil {
			return errf("Stopped waiting for deployment %s: %v.", a.Name, ctx.Err())
		}
		return errf("Deployment %s had not rolled out (revision %s) after %s: %s. Wait again, or look at its pods and events.",
			a.Name, rev, time.Since(start).Round(time.Second), what)
	}
}
