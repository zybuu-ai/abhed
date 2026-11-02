package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/store/local"
	"github.com/zybuu-ai/abhed/store/sqlite"
)

// record audit -format ocsf prints every row of each audit chain as API
// Activity, with the chain's name, the row's place and its links, which are
// the chain's own: the last line's hash is the head the chain reports.
func TestRecordAuditAsOCSF(t *testing.T) {
	cfg := config.Default()
	cfg.Record.Dir, _ = filepath.EvalSymlinks(t.TempDir())
	cfg.ControlPlane.Enabled = true
	cpDir := t.TempDir()
	_ = os.Chmod(cpDir, 0o700)
	cfg.ControlPlane.SQLitePath = filepath.Join(cpDir, "sandboxes.db")
	f, err := local.OpenAudit(cfg.Record.Dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []string{"requested", "ok"} {
		_ = f.Append(context.Background(), controlplane.AuditEvent{At: time.Now(), Principal: "alice", Action: "api:POST /v1/sessions", Outcome: o,
			Detail: map[string]string{"route": "POST /v1/sessions", "remote": "10.1.2.3"}})
	}
	_ = f.Close()
	db, err := sqlite.Open(context.Background(), cfg.ControlPlane.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Append(context.Background(), controlplane.AuditEvent{At: time.Now(), Principal: controlplane.SystemPrincipal, Action: "reconcile", SandboxID: "sb", Outcome: "ok"})
	_ = db.Close()

	for _, v := range []string{ocsf.Version, "1.1.0"} {
		var out, errw strings.Builder
		if code := verifyAudit(cfg, []string{"-format", "ocsf", "-ocsf-version", v}, &out, &errw); code != 0 {
			t.Fatalf("%s: %d %q", v, code, errw.String())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 3 {
			t.Fatalf("%s: %d events: %s", v, len(lines), out.String())
		}
		var recs []map[string]any
		for _, line := range lines {
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatal(err)
			}
			if err := ocsf.Validate(ev); err != nil {
				t.Fatalf("%s: %v", v, err)
			}
			recs = append(recs, ev["unmapped"].(map[string]any)["record"].(map[string]any))
		}
		head, _ := local.AuditChain(cfg.Record.Dir).Head(context.Background(), "default")
		if recs[1]["chain"] != local.AuditChainName || recs[1]["pos"] != float64(2) || recs[1]["hash"] != head.Hash || recs[1]["prev"] != recs[0]["hash"] {
			t.Fatalf("%s: the local chain's links: %v (head %+v)", v, recs[1], head)
		}
		if recs[2]["chain"] != sqlite.AuditChainName || recs[2]["pos"] != float64(1) || recs[2]["hash"] == "" {
			t.Fatalf("%s: the SQLite chain's links: %v", v, recs[2])
		}
	}
	var out, errw strings.Builder
	if code := verifyAudit(cfg, []string{"-format", "xml"}, &out, &errw); code != 2 {
		t.Fatalf("-format xml: %d", code)
	}
}
