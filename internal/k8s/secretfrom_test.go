package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

const (
	tlsKey = "-----BEGIN PRIVATE KEY-----\nS0VZLU1BVEVSSUFMLURPLU5PVC1MRUFL\n-----END PRIVATE KEY-----\n"
	tlsCrt = "-----BEGIN CERTIFICATE-----\nQ0VSVElGSUNBVEU=\n-----END CERTIFICATE-----\n"
)

// secretRun runs one k8s_apply through the loop, with TLS_KEY stored and
// site.crt in the workspace, and returns what was asked, recorded and sent.
func secretRun(t *testing.T, pol *policy.Engine, args func(crt string) string) (*fakeCluster, *reasons, []agent.Event) {
	t.Helper()
	f := &fakeCluster{answer: `{"kind":"Secret","apiVersion":"v1","metadata":{"name":"site-tls","namespace":"prod"},` +
		`"data":{"tls.crt":"cGxhY2Vob2xkZXI=","tls.key":"cGxhY2Vob2xkZXI="}}`}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	sess := newSession(t)
	crt := filepath.Join(sess.Root, "site.crt")
	if err := os.WriteFile(crt, []byte(tlsCrt), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(ApplyTool{M: mgr, Secret: stored(map[string]string{"TLS_KEY": tlsKey})})
	ap := &reasons{}
	store := agent.NewMemStore()
	l := agent.NewLoop(&oneCall{name: "k8s_apply", args: args(crt)}, reg, pol, ap, sess,
		agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s")
	return f, ap, evs
}

func tlsArgs(keySecret string) func(string) string {
	return func(crt string) string {
		b, _ := json.Marshal(map[string]any{"action": "apply", "secret_from": map[string]any{
			"name": "site-tls", "type": "kubernetes.io/tls",
			"data": map[string]any{"tls.crt": map[string]string{"file": crt}, "tls.key": map[string]string{"secret": keySecret}},
		}})
		return string(b)
	}
}

func allowKey() *policy.Engine {
	pol := policy.New(policy.ModeDefault)
	_ = pol.AddAllow("secret(TLS_KEY)")
	return pol
}

// A TLS Secret built from a stored key and a workspace certificate: the
// approval names the sources and never a value, the dry run carries
// placeholders, and only the write approved carries the values.
func TestSecretFromBuildsTheSecretWithoutTheModel(t *testing.T) {
	f, ap, evs := secretRun(t, allowKey(), tlsArgs("TLS_KEY"))
	if len(ap.seen) != 1 {
		t.Fatalf("asked %d times: %v", len(ap.seen), evs)
	}
	why := ap.seen[0]
	for _, want := range []string{"builds Secret site-tls (kubernetes.io/tls) with tls.crt from file ",
		"tls.key from stored secret TLS_KEY", "dry run: creates Secret prod/site-tls",
		"[withheld: " + itoa(len(tlsKey)) + " bytes, sha256 " + sha(tlsKey)} {
		if !strings.Contains(why, want) {
			t.Errorf("approval %q lacks %q", why, want)
		}
	}
	keyB64 := base64.StdEncoding.EncodeToString([]byte(tlsKey))
	for _, ev := range evs {
		for _, leak := range []string{"S0VZLU1BVEVSSUFM", keyB64[:24], "Q0VSVElGSUNBVEU"} {
			if strings.Contains(string(ev.Payload), leak) {
				t.Errorf("%s holds a value: %s", ev.Type, ev.Payload)
			}
		}
	}
	dry, real := f.writes()
	if len(dry) != 1 || len(real) != 1 {
		t.Fatalf("dry %d, real %d", len(dry), len(real))
	}
	if strings.Contains(dry[0].body, keyB64) || !strings.Contains(dry[0].body, base64.StdEncoding.EncodeToString(placeholderFor("tls.key", typeTLS))) {
		t.Errorf("the dry run carried the key: %s", dry[0].body)
	}
	var sentObj struct {
		Kind     string `json:"kind"`
		Type     string `json:"type"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(real[0].body), &sentObj); err != nil {
		t.Fatal(err)
	}
	if sentObj.Kind != "Secret" || sentObj.Type != typeTLS || sentObj.Metadata.Name != "site-tls" ||
		sentObj.Data["tls.key"] != keyB64 || sentObj.Data["tls.crt"] != base64.StdEncoding.EncodeToString([]byte(tlsCrt)) {
		t.Errorf("sent %s", real[0].body)
	}
	if !strings.Contains(real[0].uri, "/api/v1/namespaces/prod/secrets/site-tls?") {
		t.Errorf("sent to %s", real[0].uri)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// The stored secret needs its own allow rule, as every secret use does.
func TestSecretFromNeedsTheSecretRule(t *testing.T) {
	f, ap, evs := secretRun(t, policy.New(policy.ModeDefault), tlsArgs("TLS_KEY"))
	if len(ap.seen) != 0 || len(f.got) != 0 {
		t.Fatalf("ran without secret(TLS_KEY): asked %v, sent %+v", ap.seen, f.got)
	}
	if out := toolResult(evs); !strings.Contains(out, "secret(TLS_KEY)") {
		t.Fatalf("told %s", out)
	}
}

// A value where a secret's name belongs is withheld from the record and the call refused.
func TestSecretFromWithholdsAValueGivenAsAName(t *testing.T) {
	f, ap, evs := secretRun(t, allowKey(), tlsArgs(tlsKey))
	if len(ap.seen) != 0 || len(f.got) != 0 {
		t.Fatalf("ran with a value for a name: asked %v, sent %+v", ap.seen, f.got)
	}
	for _, ev := range evs {
		if strings.Contains(string(ev.Payload), "S0VZLU1BVEVSSUFM") {
			t.Errorf("%s holds the value: %s", ev.Type, ev.Payload)
		}
	}
}

// A file a read rule denies is not read into a Secret.
func TestSecretFromHonoursReadRules(t *testing.T) {
	pol := allowKey()
	if err := pol.AddDeny("read(*.crt)"); err != nil {
		t.Fatal(err)
	}
	f, ap, evs := secretRun(t, pol, tlsArgs("TLS_KEY"))
	if len(ap.seen) != 0 || len(f.got) != 0 {
		t.Fatalf("read a denied file: asked %v, sent %+v", ap.seen, f.got)
	}
	if out := toolResult(evs); !strings.Contains(out, "site.crt it would read") {
		t.Fatalf("told %s", out)
	}
}

// The Secret is judged as an apply in its namespace, so a rule there holds.
func TestSecretFromIsJudgedInItsNamespace(t *testing.T) {
	pol := allowKey()
	if err := pol.AddDeny("k8s_apply(*/prod/apply)"); err != nil {
		t.Fatal(err)
	}
	f, _, _ := secretRun(t, pol, tlsArgs("TLS_KEY"))
	if len(f.got) != 0 {
		t.Fatalf("a denied apply reached the cluster: %+v", f.got)
	}
}

// A source that cannot be read, or a certificate that is not one, is refused
// before anyone is asked, naming the source and never its content.
func TestSecretFromRefusesBadSourcesBeforeAsking(t *testing.T) {
	for name, args := range map[string]func(string) string{
		"missing file": func(string) string {
			return tlsArgs("TLS_KEY")("/nowhere/site.crt")
		},
		"not a cert": func(crt string) string {
			b, _ := json.Marshal(map[string]any{"action": "apply", "secret_from": map[string]any{
				"name": "site-tls", "type": "kubernetes.io/tls",
				"data": map[string]any{"tls.crt": map[string]string{"secret": "TLS_KEY"}, "tls.key": map[string]string{"secret": "TLS_KEY"}},
			}})
			return string(b)
		},
		"manifest too": func(crt string) string {
			b, _ := json.Marshal(map[string]any{"action": "apply", "manifest": `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"}}`,
				"secret_from": map[string]any{"name": "s", "data": map[string]any{"k": map[string]string{"secret": "TLS_KEY"}}}})
			return string(b)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, ap, evs := secretRun(t, allowKey(), args)
			if len(ap.seen) != 0 {
				t.Fatalf("asked %v", ap.seen)
			}
			if _, real := f.writes(); len(real) != 0 {
				t.Fatalf("sent %+v", real)
			}
			if out := toolResult(evs); out == "" || strings.Contains(out, "S0VZ") {
				t.Fatalf("told %q", out)
			}
		})
	}
}
