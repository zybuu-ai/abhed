package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/controlplane/storetest"
)

var t0 = time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

func dbPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cp", DefaultName)
}

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newSandbox(t *testing.T, owner string) controlplane.Sandbox {
	t.Helper()
	s, err := controlplane.NewSandbox(owner, "default", "process", map[string]string{"n": "0"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) controlplane.Store { return open(t, dbPath(t)) })
}

func TestAuditAppendOnly(t *testing.T) {
	ctx := context.Background()
	s := open(t, dbPath(t))
	for i, outcome := range []string{controlplane.OutcomeRequested, controlplane.OutcomeOK} {
		if err := s.Append(ctx, controlplane.AuditEvent{At: t0.Add(time.Duration(i) * time.Second), Principal: "alice",
			Action: "create", SandboxID: "sbx_0123456789abcdef", Generation: 1, Outcome: outcome,
			Detail: map[string]string{"template": "default"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`UPDATE sandbox_audit SET principal = 'mallory'`,
		`UPDATE sandbox_audit SET outcome = 'failed' WHERE seq = 1`,
		`DELETE FROM sandbox_audit WHERE seq = 2`,
		`DELETE FROM sandbox_audit`,
		`INSERT OR REPLACE INTO sandbox_audit (seq, at, principal, action, sandbox_id, generation, outcome)
		 VALUES (1, '', 'mallory', 'x', 'sbx_0123456789abcdef', 1, 'ok')`,
	} {
		if _, err := s.w.Exec(stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", stmt, err)
		}
	}
	got, err := s.AuditFor(ctx, "sbx_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Principal != "alice" || got[0].Outcome != controlplane.OutcomeRequested ||
		got[1].Outcome != controlplane.OutcomeOK || got[0].Seq >= got[1].Seq || !got[0].At.Equal(t0) ||
		got[1].Detail["template"] != "default" {
		t.Fatalf("record after refused changes: %+v", got)
	}
}

func TestReopenKeepsEverything(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Create(ctx, newSandbox(t, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path)
	back, err := s.Get(ctx, a.ID)
	if err != nil || back.ResourceVersion != a.ResourceVersion {
		t.Fatalf("after reopening: %+v %v", back, err)
	}
	b, err := s.Create(ctx, newSandbox(t, "bob"))
	if err != nil || b.ResourceVersion <= a.ResourceVersion {
		t.Fatalf("resource version after reopening: %d then %d, %v", a.ResourceVersion, b.ResourceVersion, err)
	}
	var versions int
	if err := s.r.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&versions); err != nil || versions != SchemaVersion {
		t.Fatalf("schema versions recorded %d, %v", versions, err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := dbPath(t)
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, 'later')`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("a newer schema was opened: %v", err)
	}
}

// Mutate without If-Match loses no update when many writers race; with it,
// one wins and the rest are told their version is gone.
func TestMutateRaces(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t)
	s := open(t, path)
	// A second store on the same file stands for a second process.
	other := open(t, path)
	sb, err := s.Create(ctx, newSandbox(t, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := controlplane.Store(s)
			if i%2 == 1 {
				st = other
			}
			for range 5 {
				_, err := controlplane.Mutate(ctx, st, sb.ID, "", func(x *controlplane.Sandbox) error {
					n, _ := strconv.Atoi(x.Labels["n"])
					x.Labels["n"] = strconv.Itoa(n + 1)
					return nil
				})
				switch {
				case err == nil:
					ok.Add(1)
				case !errors.Is(err, controlplane.ErrVersionMismatch):
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	got, err := s.Get(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["n"] != strconv.FormatInt(ok.Load(), 10) || ok.Load() == 0 {
		t.Fatalf("%d mutations succeeded and the counter says %s", ok.Load(), got.Labels["n"])
	}

	etag := controlplane.ETag(got.ResourceVersion)
	var wins, refused atomic.Int32
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := controlplane.Store(s)
			if i%2 == 1 {
				st = other
			}
			_, err := controlplane.Mutate(ctx, st, sb.ID, etag, func(x *controlplane.Sandbox) error {
				x.Labels["winner"] = strconv.Itoa(i)
				return nil
			})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, controlplane.ErrPreconditionFailed):
				refused.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || refused.Load() != 7 {
		t.Fatalf("with If-Match: %d won, %d refused", wins.Load(), refused.Load())
	}
}

const crashEnv = "ABHED_SQLITE_CRASH"

// TestCrashHelper is the process that dies; it does nothing on its own.
func TestCrashHelper(t *testing.T) {
	spec := os.Getenv(crashEnv)
	if spec == "" {
		return
	}
	path, mode, _ := strings.Cut(spec, "|")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sb, _ := controlplane.NewSandbox("alice", "default", "process", nil, t0)
	sb.ID = "sbx_00000000000000aa"
	if _, err := s.Create(ctx, sb); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := s.Append(ctx, controlplane.AuditEvent{At: t0, Principal: "alice", Action: "create",
		SandboxID: sb.ID, Generation: 1, Outcome: controlplane.OutcomeOK}); err != nil {
		os.Exit(2)
	}
	if mode == "mid-transaction" {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			os.Exit(2)
		}
		if _, err := tx.Exec(`INSERT INTO sandboxes (id, owner, state, resource_version, object)
			VALUES ('sbx_00000000000000bb', 'alice', 'Ready', 99, '{}')`); err != nil {
			os.Exit(2)
		}
		if _, err := tx.Exec(`UPDATE counters SET value = 1000`); err != nil {
			os.Exit(2)
		}
		if _, err := tx.Exec(`INSERT INTO sandbox_audit (at, principal, action, sandbox_id, generation, outcome)
			VALUES ('x', 'mallory', 'create', 'sbx_00000000000000bb', 1, 'ok')`); err != nil {
			os.Exit(2)
		}
	}
	// Dies without committing or closing, as a killed server does.
	os.Exit(3)
}

func TestCrashSafety(t *testing.T) {
	for _, mode := range []string{"after-commit", "mid-transaction"} {
		t.Run(mode, func(t *testing.T) {
			path := dbPath(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashEnv+"="+path+"|"+mode)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatalf("helper: %v\n%s", err, out)
			}
			ctx := context.Background()
			s := open(t, path)
			var check string
			if err := s.r.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
				t.Fatalf("integrity after the crash: %q %v", check, err)
			}
			a, err := s.Get(ctx, "sbx_00000000000000aa")
			if err != nil {
				t.Fatalf("the committed sandbox was lost: %v", err)
			}
			if _, err := s.Get(ctx, "sbx_00000000000000bb"); !errors.Is(err, controlplane.ErrNotFound) {
				t.Fatalf("the uncommitted sandbox survived: %v", err)
			}
			rec, err := s.AuditFor(ctx, "sbx_00000000000000aa")
			if err != nil || len(rec) != 1 {
				t.Fatalf("the committed record: %+v %v", rec, err)
			}
			if rec, _ := s.AuditFor(ctx, "sbx_00000000000000bb"); len(rec) != 0 {
				t.Fatalf("the uncommitted record survived: %+v", rec)
			}
			b, err := s.Create(ctx, newSandbox(t, "bob"))
			if err != nil || b.ResourceVersion != a.ResourceVersion+1 {
				t.Fatalf("resource version after the crash: %d then %d, %v", a.ResourceVersion, b.ResourceVersion, err)
			}
		})
	}
}

// A write that outlives its context is rolled back, and the store still works.
func TestCancelledWrite(t *testing.T) {
	s := open(t, dbPath(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Create(ctx, newSandbox(t, "alice")); err == nil {
		t.Fatal("a cancelled create succeeded")
	}
	list, _, err := s.List(context.Background(), controlplane.ListOptions{})
	if err != nil || len(list) != 0 {
		t.Fatalf("after a cancelled create: %d %v", len(list), err)
	}
	if _, err := s.Create(context.Background(), newSandbox(t, "alice")); err != nil {
		t.Fatal(err)
	}
}

// A file written before migration 2 is brought up in order: each version is
// recorded once, with no gap, and the newer table works.
func TestOlderSchemaMigratesInOrder(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`DROP TABLE sandbox_templates; ALTER TABLE sandbox_idempotency DROP COLUMN claim_token;
		DELETE FROM schema_version WHERE version > 1`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = open(t, path)
	rows, err := s.r.Query(`SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := 1
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != want {
			t.Fatalf("schema version %d recorded where %d was due", v, want)
		}
		want++
	}
	if want-1 != SchemaVersion {
		t.Fatalf("recorded up to %d, build is at %d", want-1, SchemaVersion)
	}
	if _, err := s.CreateTemplate(ctx, controlplane.Template{Name: "base", Driver: "process"}); err != nil {
		t.Fatalf("templates after the upgrade: %v", err)
	}
	rec := controlplane.IdemRecord{Scope: "s", Key: "k", RequestHash: "h", ExpiresAt: time.Now().Add(time.Minute), Token: "t"}
	if _, ok, err := s.Claim(ctx, rec, time.Now()); err != nil || !ok {
		t.Fatalf("claim after the upgrade: %v %v", ok, err)
	}
	if err := s.Complete(ctx, "s", "k", "t", 200, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("complete after the upgrade: %v", err)
	}
}
