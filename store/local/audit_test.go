package local

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

func auditEv(action, outcome string) controlplane.AuditEvent {
	return controlplane.AuditEvent{At: time.Unix(1700000000, 0), Principal: "alice", Action: action,
		Outcome: outcome, Detail: map[string]string{"route": action, "tenant": "default"}}
}

func TestAuditChainVerifies(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenAudit(dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, o := range []string{"requested", "ok", "requested", "failed"} {
		if err := a.Append(ctx, auditEv("api:POST /v1/sessions", o)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := VerifyAudit(AuditPath(dir, "default"))
	if err != nil || !r.OK() || r.Lines != 4 || r.Head == "" || r.Torn {
		t.Fatalf("%+v %v", r, err)
	}
	// A second opener continues the chain.
	b, err := OpenAudit(dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Append(ctx, auditEv("api:DELETE /v1/sessions/{id}", "requested")); err != nil {
		t.Fatal(err)
	}
	evs, err := b.AuditFor(ctx, "")
	if err != nil || len(evs) != 5 || evs[4].Seq != 5 || evs[4].Principal != "alice" {
		t.Fatalf("%v %+v", err, evs)
	}
	_ = b.Close()
	if info, _ := os.Stat(AuditPath(dir, "default")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestAuditEditIsFound(t *testing.T) {
	dir := t.TempDir()
	a, _ := OpenAudit(dir, "default")
	for i := 0; i < 3; i++ {
		_ = a.Append(context.Background(), auditEv("api:POST /v1/sessions", "ok"))
	}
	_ = a.Close()
	path := AuditPath(dir, "default")
	b, _ := os.ReadFile(path)
	lines := bytes.SplitAfter(b, []byte("\n"))
	for name, edited := range map[string][]byte{
		"principal": bytes.Join([][]byte{lines[0], bytes.Replace(lines[1], []byte(`"alice"`), []byte(`"mallory"`), 1), lines[2]}, nil),
		"removed":   bytes.Join([][]byte{lines[0], lines[2]}, nil),
		"reordered": bytes.Join([][]byte{lines[1], lines[0], lines[2]}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "api.jsonl")
			_ = os.WriteFile(p, edited, 0o600)
			r, err := VerifyAudit(p)
			if err != nil || r.OK() {
				t.Fatalf("an edit went unseen: %+v %v", r, err)
			}
		})
	}
}

func TestAuditTornLineIsCut(t *testing.T) {
	dir := t.TempDir()
	a, _ := OpenAudit(dir, "default")
	_ = a.Append(context.Background(), auditEv("api:POST /v1/sessions", "requested"))
	_ = a.Close()
	path := AuditPath(dir, "default")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"seq":2,"at":"x`)
	_ = f.Close()
	if r, _ := VerifyAudit(path); !r.Torn || !r.OK() {
		t.Fatalf("torn tail not reported: %+v", r)
	}
	a, _ = OpenAudit(dir, "default")
	if err := a.Append(context.Background(), auditEv("api:POST /v1/sessions", "ok")); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	if r, _ := VerifyAudit(path); !r.OK() || r.Torn || r.Lines != 2 {
		t.Fatalf("after a torn line: %+v", r)
	}
}

func TestAuditConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	a, _ := OpenAudit(dir, "default")
	b, _ := OpenAudit(dir, "default")
	var wg sync.WaitGroup
	for _, w := range []*AuditFile{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := w.Append(context.Background(), auditEv("api:POST /v1/sessions", "ok")); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
	if r, _ := VerifyAudit(AuditPath(dir, "default")); !r.OK() || r.Lines != 100 {
		t.Fatalf("%+v", r)
	}
}

func TestAuditRefusesLinkedFolder(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, "default", "audit")); err != nil {
		t.Skip(err)
	}
	if _, err := OpenAudit(dir, "default"); err == nil {
		t.Fatal("a linked audit folder was used")
	}
}

// The audit file answers the chain API: its head, and ranges that hold up
// to an edited line.
func TestAuditFileIsAChainSource(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenAudit(dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, o := range []string{"requested", "ok", "requested", "ok"} {
		if err := a.Append(ctx, auditEv("api:POST /v1/sessions", o)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	src := AuditChain(dir)
	h, err := src.Head(ctx, "default")
	if err != nil || h.Pos != 4 || len(h.Hash) != 64 || h.Chain != "local/audit" {
		t.Fatalf("head = %+v, %v", h, err)
	}
	if r, err := src.VerifyRange(ctx, "default", 2, 0); err != nil || !r.OK() || r.To != 4 || r.Hash != h.Hash || r.Before == Genesis {
		t.Fatalf("range = %+v, %v", r, err)
	}
	path := AuditPath(dir, "default")
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(data, []byte(`"outcome":"requested"`), []byte(`"outcome":"rejected"`), 2), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lines 1 and 3 were edited, so no range from the start holds.
	if r, err := src.VerifyRange(ctx, "default", 1, 2); err != nil || r.OK() {
		t.Fatalf("a range over an edited first line = %+v, %v", r, err)
	}
	if r, _ := src.VerifyRange(ctx, "default", 1, 4); r.OK() || r.FirstBad != 1 {
		t.Fatalf("whole range = %+v", r)
	}
}
