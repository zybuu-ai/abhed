package ui

import (
	"encoding/json"
	"testing"
)

// The one-line summary names where a remote or cluster call goes and what it
// does, and a secret only by name.
func TestSummarizeRemoteAndClusterCalls(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"ssh", map[string]any{"host": "web1", "command": "uptime"}, "web1 $ uptime"},
		{"ssh", map[string]any{"host": "web1", "command": "sudo -S true", "stdin_secret": "PW"}, "web1 $ sudo -S true (stdin: stored secret PW)"},
		{"ssh_connect", map[string]any{"addr": "10.0.0.5", "user": "ops", "name": "lab"}, "ops@10.0.0.5 as lab"},
		{"ssh_put", map[string]any{"host": "web1", "remote_path": "/etc/tls/site.key", "secret": "SITE_KEY"}, "web1:/etc/tls/site.key ← stored secret SITE_KEY"},
		{"ssh_put", map[string]any{"host": "web1", "remote_path": "/etc/tls/site.crt", "file": "/w/site.crt"}, "web1:/etc/tls/site.crt ← file /w/site.crt"},
		{"ssh_get", map[string]any{"host": "web1", "remote_path": "/etc/nginx/nginx.conf", "file": "/w/nginx.conf"}, "web1:/etc/nginx/nginx.conf → /w/nginx.conf"},
		{"k8s_get", map[string]any{"resource": "deployments", "name": "web", "namespace": "demo", "cluster": "lab", "wait_seconds": 120},
			"deployments/web · namespace demo · cluster lab · wait 120s for the rollout"},
		{"k8s_apply", map[string]any{"action": "rollback", "resource": "deployments", "name": "web", "namespace": "demo", "cluster": "lab", "revision": 4},
			"rollback · cluster lab · namespace demo · deployments/web · revision 4"},
		{"k8s_apply", map[string]any{"action": "scale", "resource": "deployments", "name": "web", "replicas": 3},
			"scale · deployments/web · replicas 3"},
		{"k8s_login", map[string]any{"cluster": "lab", "token_secret": "LAB_TOKEN"}, "cluster lab with stored secret LAB_TOKEN"},
	} {
		raw, _ := json.Marshal(tc.args)
		if got := summarizeArgs(tc.tool, raw); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.tool, got, tc.want)
		}
	}
}
