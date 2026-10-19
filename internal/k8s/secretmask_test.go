package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// secretServer answers every request with body, as a cluster holding a Secret would.
func secretServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const tlsSecret = `{"kind":"Secret","apiVersion":"v1","type":"kubernetes.io/tls",
 "metadata":{"name":"site-tls","namespace":"prod","annotations":{
   "kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"tls.key\":\"S0VZLU1BVEVSSUFM\"}}",
   "team":"web"}},
 "data":{"tls.crt":"Q0VSVA==","tls.key":"S0VZLU1BVEVSSUFM"},
 "stringData":{"password":"hunter2-plain"}}`

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A Secret read with k8s_get shows its keys, each value's length and sha256,
// and never a value, encoded or not.
func TestGetSecretMasksValues(t *testing.T) {
	srv := secretServer(t, tlsSecret)
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	for _, resource := range []string{"secrets", "secret", "Secret"} {
		args, _ := json.Marshal(map[string]string{"resource": resource, "name": "site-tls"})
		res := GetTool{M: mgr}.Run(context.Background(), nil, args)
		if res.IsError {
			t.Fatalf("get %s: %s", resource, res.Content)
		}
		// Neither a value nor its plain sha256, which a short one is guessed from.
		for _, leak := range []string{"S0VZLU1BVEVSSUFM", "KEY-MATERIAL", "hunter2-plain", "Q0VSVA==",
			sha("KEY-MATERIAL"), sha("hunter2-plain"), "sha256"} {
			if strings.Contains(res.Content, leak) {
				t.Errorf("get %s showed a value %q:\n%s", resource, leak, res.Content)
			}
		}
		for _, want := range []string{`"tls.key"`, `"tls.crt"`, `"password"`,
			"12 bytes, fingerprint " + secrets.Fingerprint([]byte("KEY-MATERIAL")),
			"13 bytes, fingerprint " + secrets.Fingerprint([]byte("hunter2-plain")),
			`"team": "web"`, "withheld: holds the Secret's values"} {
			if !strings.Contains(res.Content, want) {
				t.Errorf("get %s: want %q in:\n%s", resource, want, res.Content)
			}
		}
	}
}

// What the resource names decides, not what the response says it is: a
// Secret's response that does not call itself one, or is not JSON, is masked.
func TestGetSecretMasksWhateverTheResponseSays(t *testing.T) {
	for _, body := range []string{
		`{"data":{"k":"c2VjcmV0LXZhbHVl"}}`,
		`data: {k: c2VjcmV0LXZhbHVl}`,
		`{"kind":"Secret","data":["c2VjcmV0LXZhbHVl"]}`,
	} {
		srv := secretServer(t, body)
		mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
		args, _ := json.Marshal(map[string]string{"resource": "secrets", "name": "x"})
		res := GetTool{M: mgr}.Run(context.Background(), nil, args)
		if strings.Contains(res.Content, "c2VjcmV0LXZhbHVl") {
			t.Errorf("a value reached the model for %s:\n%s", body, res.Content)
		}
	}
}

// A list of Secrets names them and shows none of their values.
func TestListSecretsShowsNoValues(t *testing.T) {
	srv := secretServer(t, `{"kind":"SecretList","items":[`+tlsSecret+`]}`)
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]string{"resource": "secrets"})
	res := GetTool{M: mgr}.Run(context.Background(), nil, args)
	if !strings.Contains(res.Content, "prod/site-tls") || strings.Contains(res.Content, "S0VZ") {
		t.Errorf("list: %s", res.Content)
	}
}

// What the cluster answers an apply of a Secret is masked too.
func TestApplySecretResultIsMasked(t *testing.T) {
	srv := secretServer(t, tlsSecret)
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]string{"action": "apply",
		"manifest": `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"site-tls"},"data":{"tls.key":"S0VZLU1BVEVSSUFM"}}`})
	res := ApplyTool{M: mgr}.Run(context.Background(), nil, args)
	if res.IsError || strings.Contains(res.Content, "hunter2") || strings.Contains(res.Content, `"S0VZLU1BVEVSSUFM"`) {
		t.Errorf("apply result: %s", res.Content)
	}
}
