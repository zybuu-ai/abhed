package sessionsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

type rig struct {
	svc   *controlplane.Service
	mem   *controlplane.Memory
	fake  *drivertest.Fake
	root  string
	store *agent.MemStore
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := controlplane.NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", driver.CapCommand)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := controlplane.NewMemory()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	folder := func(id string) (string, error) {
		d := filepath.Join(root, id)
		return d, os.MkdirAll(d, 0o700)
	}
	svc, err := controlplane.New(controlplane.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: folder, ReleaseWorkspace: func(id string) error { return os.RemoveAll(filepath.Join(root, id)) }})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{svc: svc, mem: mem, fake: fake, root: root, store: agent.NewMemStore()}
}

func (r *rig) manager(t *testing.T, mod func(*Config)) *Manager {
	t.Helper()
	cfg := Config{Service: r.svc, Tenant: "default",
		Workspace: func(id string) (string, error) {
			d := filepath.Join(r.root, id)
			return d, os.MkdirAll(d, 0o700)
		},
		Isolation: func(sb controlplane.Sandbox) tools.Isolation { return tools.Isolation{Tier: sb.Tier} },
		Ready:     10 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (r *rig) bind(t *testing.T, m *Manager, session, user string) Bound {
	t.Helper()
	rec := agent.NewRecorder(r.store, session, "")
	// As a continued session's recorder is, past what is already recorded.
	if evs, _ := r.store.Events(session); len(evs) > 0 {
		rec.Advance(evs[len(evs)-1].Seq)
	}
	b, err := m.Bind(context.Background(), Binding{SessionID: session, User: user, Tenant: "default",
		Shared: tools.Bash{SecretNames: []string{"TOKEN"}}, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func run(t *testing.T, b Bound, cwd, line string) (string, error) {
	t.Helper()
	cmd := b.Bash.Sandbox(context.Background(), cwd, line)
	if cmd.Err != nil {
		return "", cmd.Err
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestConfigIsChecked(t *testing.T) {
	r := newRig(t)
	for _, mod := range []func(*Config){
		func(c *Config) { c.Scope = "tenant" },
		func(c *Config) { c.Retention = "archive" },
		func(c *Config) { c.Pool = controlplane.MaxPoolSize + 1 },
		func(c *Config) { c.Template, c.Pool = "not a label", 1 },
		func(c *Config) { c.Service = nil },
	} {
		cfg := Config{Service: r.svc, Workspace: func(string) (string, error) { return "", nil },
			Isolation: func(controlplane.Sandbox) tools.Isolation { return tools.Isolation{} }}
		mod(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
}

// A session gets a Ready sandbox of its own, labelled with it, owned by the
// server, with a folder of its own; binding it again reattaches the same one.
func TestSessionGetsItsOwnSandbox(t *testing.T) {
	r := newRig(t)
	m := r.manager(t, nil)
	a := r.bind(t, m, "s1", "alice")
	sb := a.Sandbox
	if sb.State != controlplane.StateReady || sb.Owner != controlplane.SessionsPrincipal || sb.Labels[controlplane.LabelSession] != "s1" ||
		sb.Template != DefaultTemplate || a.Workspace != filepath.Join(r.root, sb.ID) {
		t.Fatalf("bound %+v at %s", sb, a.Workspace)
	}
	if a.Bash.SecretNames[0] != "TOKEN" || a.Bash.Isolation.Tier != "process" {
		t.Fatalf("the session's bash lost the shared settings: %+v", a.Bash)
	}
	again := r.bind(t, m, "s1", "alice")
	if again.Sandbox.ID != sb.ID || again.Workspace != a.Workspace {
		t.Fatalf("rebinding made %s, had %s", again.Sandbox.ID, sb.ID)
	}
	b := r.bind(t, m, "s2", "alice")
	if b.Sandbox.ID == sb.ID || b.Workspace == a.Workspace {
		t.Fatal("two sessions share a sandbox")
	}
}

// The session's commands run in its sandbox's folder, and the record says
// which sandbox before the first of them; one that cannot be recorded runs
// nothing.
func TestCommandsRunInTheSandboxAndAreLinked(t *testing.T) {
	r := newRig(t)
	m := r.manager(t, nil)
	a := r.bind(t, m, "s1", "alice")
	out, err := run(t, a, a.Workspace, "pwd; echo $ABHED_SANDBOX; echo x > made.txt")
	if err != nil || out != a.Workspace+"\n"+a.Sandbox.ID+"\n" {
		t.Fatalf("ran %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(a.Workspace, "made.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, a, a.Workspace, "true"); err != nil {
		t.Fatal(err)
	}
	evs, _ := r.store.Events("s1")
	bound := 0
	for _, ev := range evs {
		if ev.Type == agent.EvSandboxBound {
			bound++
			if !strings.Contains(string(ev.Payload), a.Sandbox.ID) || !strings.Contains(string(ev.Payload), `"driver":"process"`) {
				t.Fatalf("payload %s", ev.Payload)
			}
		}
	}
	if bound != 1 {
		t.Fatalf("sandbox.bound recorded %d times", bound)
	}
	// A record that refuses the write: nothing runs.
	b := r.bind(t, m, "s2", "alice")
	rec := agent.NewRecorder(r.store, "s2", "")
	rec.Gate = func() error { return errors.New("lease lost") }
	b2, err := m.Bind(context.Background(), Binding{SessionID: "s2", User: "alice", Tenant: "default", Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if b2.Sandbox.ID != b.Sandbox.ID {
		t.Fatal("rebinding moved the session")
	}
	if _, err := run(t, b2, b2.Workspace, "echo ran > ran.txt"); err == nil || !strings.Contains(err.Error(), "not run") {
		t.Fatalf("a command ran without its record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
	// A sandbox deleted from under the session refuses its commands.
	if err := r.svc.DeleteNow(context.Background(), maker(map[string]string{controlplane.LabelSession: "s1"}), a.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, a, a.Workspace, "true"); err == nil {
		t.Fatal("a deleted sandbox ran a command")
	}
}

func TestUserScopeSharesOnePerUser(t *testing.T) {
	r := newRig(t)
	m := r.manager(t, func(c *Config) { c.Scope = ScopeUser })
	a1, a2 := r.bind(t, m, "s1", "alice"), r.bind(t, m, "s2", "alice")
	b := r.bind(t, m, "s3", "bob")
	if a1.Sandbox.ID != a2.Sandbox.ID {
		t.Fatal("alice's sessions got different sandboxes")
	}
	if b.Sandbox.ID == a1.Sandbox.ID {
		t.Fatal("bob got alice's sandbox")
	}
	if _, ok := a1.Sandbox.Labels[controlplane.LabelSession]; ok || a1.Sandbox.Labels[controlplane.LabelSessionUser] != controlplane.UserLabel("default", "alice") {
		t.Fatalf("labels %v", a1.Sandbox.Labels)
	}
	// Deleting one session leaves the user's sandbox to the others.
	if err := m.SessionDeleted(context.Background(), "s1", "alice", "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mem.Get(context.Background(), a1.Sandbox.ID); err != nil {
		t.Fatalf("the user's sandbox went with one session: %v", err)
	}
}

func TestConcurrentBindsOfOneSessionMakeOneSandbox(t *testing.T) {
	r := newRig(t)
	m := r.manager(t, func(c *Config) { c.Scope = ScopeUser })
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := m.Bind(context.Background(), Binding{SessionID: "s" + string(rune('a'+i)), User: "alice", Tenant: "default"})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- b.Sandbox.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("one user got sandboxes %s and %s", first, id)
		}
	}
}

// A session in another tenant than the store's is refused, and never
// reaches the warm pool.
func TestAnotherTenantIsRefused(t *testing.T) {
	r := newRig(t)
	m := r.manager(t, func(c *Config) { c.Pool = 1 })
	if err := m.Pool().Fill(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Pool().Members(context.Background())
	if _, err := r.svc.WaitReady(context.Background(), controlplane.Caller{Principal: controlplane.SessionsPrincipal, Scope: map[string]string{controlplane.LabelPool: DefaultTemplate}}, before[0].ID); err != nil {
		t.Fatal(err)
	}
	_, err := m.Bind(context.Background(), Binding{SessionID: "s1", User: "mallory", Tenant: "other"})
	if !errors.Is(err, controlplane.ErrForbidden) {
		t.Fatalf("another tenant's session: %v", err)
	}
	after, _ := m.Pool().Members(context.Background())
	if len(after) != 1 || after[0].ID != before[0].ID || after[0].Labels[controlplane.LabelPool] != DefaultTemplate {
		t.Fatalf("the pool changed for another tenant: %+v", after)
	}
	if _, err := m.Workspace(context.Background(), "s1", "mallory", "other"); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("another tenant's workspace: %v", err)
	}
}

func TestBindClaimsFromTheWarmPool(t *testing.T) {
	r := newRig(t)
	m := r.manager(t, func(c *Config) { c.Pool = 2 })
	ctx := context.Background()
	if err := m.Pool().Fill(ctx); err != nil {
		t.Fatal(err)
	}
	ms, _ := m.Pool().Members(ctx)
	pooled := map[string]int64{}
	for _, mb := range ms {
		sb, err := r.svc.WaitReady(ctx, controlplane.Caller{Principal: controlplane.SessionsPrincipal, Scope: map[string]string{controlplane.LabelPool: DefaultTemplate}}, mb.ID)
		if err != nil {
			t.Fatal(err)
		}
		pooled[sb.ID] = sb.Generation
	}
	creates := r.fake.Calls("create")
	a := r.bind(t, m, "s1", "alice")
	gen, ok := pooled[a.Sandbox.ID]
	if !ok || a.Sandbox.Generation != gen+1 || r.fake.Calls("create") != creates {
		t.Fatalf("not claimed from the pool: %+v (pool %v)", a.Sandbox, pooled)
	}
	b := r.bind(t, m, "s2", "bob")
	if _, ok := pooled[b.Sandbox.ID]; !ok || b.Sandbox.ID == a.Sandbox.ID {
		t.Fatalf("second claim %s", b.Sandbox.ID)
	}
	// Empty now: the next session gets a new sandbox, never a claimed one.
	c := r.bind(t, m, "s3", "carol")
	if _, ok := pooled[c.Sandbox.ID]; ok {
		t.Fatal("a claimed sandbox was handed out again")
	}
}

func TestRetentionOnClose(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		retention string
		want      controlplane.State
		gone      bool
	}{{RetainStop, controlplane.StateStopped, false}, {RetainDelete, "", true}, {RetainKeep, controlplane.StateReady, false}} {
		t.Run(c.retention, func(t *testing.T) {
			r := newRig(t)
			m := r.manager(t, func(cfg *Config) { cfg.Retention, cfg.Pool = c.retention, 1 })
			if err := m.Pool().Fill(ctx); err != nil {
				t.Fatal(err)
			}
			a := r.bind(t, m, "s1", "alice")
			m.Release("s1")
			if err := m.Close(ctx); err != nil {
				t.Fatal(err)
			}
			sb, err := r.mem.Get(ctx, a.Sandbox.ID)
			switch {
			case c.gone && !errors.Is(err, controlplane.ErrNotFound):
				t.Fatalf("kept: %+v %v", sb, err)
			case !c.gone && (err != nil || sb.State != c.want):
				t.Fatalf("state %s, want %s (%v)", sb.State, c.want, err)
			}
			if left, _ := m.Pool().Members(ctx); len(left) != 0 {
				t.Fatalf("the pool was not drained: %d left", len(left))
			}
			if c.gone {
				if _, err := os.Stat(a.Workspace); err == nil {
					t.Fatal("the deleted sandbox's folder is still there")
				}
			}
		})
	}
}

// A stopped sandbox is started again when its session is continued, as the
// same sandbox with its folder, in a new generation.
func TestResumeReattaches(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	m := r.manager(t, nil)
	a := r.bind(t, m, "s1", "alice")
	if _, err := run(t, a, a.Workspace, "echo kept > notes.txt"); err != nil {
		t.Fatal(err)
	}
	m.Release("s1")
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	m2 := r.manager(t, nil)
	if ws, err := m2.Workspace(ctx, "s1", "alice", "default"); err != nil || ws != a.Workspace {
		t.Fatalf("workspace of a stopped session: %q %v", ws, err)
	}
	b := r.bind(t, m2, "s1", "alice")
	if b.Sandbox.ID != a.Sandbox.ID || b.Sandbox.Generation != a.Sandbox.Generation+1 || b.Sandbox.State != controlplane.StateReady {
		t.Fatalf("resumed %+v, was %+v", b.Sandbox, a.Sandbox)
	}
	if out, err := run(t, b, b.Workspace, "cat notes.txt"); err != nil || out != "kept\n" {
		t.Fatalf("the folder did not survive: %q %v", out, err)
	}
	// After a crash: Ready in the store, the instance gone.
	if err := r.fake.Delete(ctx, b.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	lost := r.bind(t, m2, "s1", "alice")
	if lost.Sandbox.ID != a.Sandbox.ID || lost.Sandbox.State != controlplane.StateReady {
		t.Fatalf("after a lost instance %+v", lost.Sandbox)
	}
	if out, err := run(t, lost, lost.Workspace, "cat notes.txt"); err != nil || out != "kept\n" {
		t.Fatalf("after a lost instance: %q %v", out, err)
	}
	// After a failed instance too.
	r.fake.SetPhase(b.Sandbox.ID, driver.PhaseFailed)
	_ = r.svc.Reconcile(ctx, b.Sandbox.ID)
	c := r.bind(t, m2, "s1", "alice")
	if c.Sandbox.ID != a.Sandbox.ID || c.Sandbox.State != controlplane.StateReady {
		t.Fatalf("after a failure %+v", c.Sandbox)
	}
}

func TestSessionDeletedDeletesItsSandbox(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	m := r.manager(t, nil)
	a := r.bind(t, m, "s1", "alice")
	if err := m.SessionDeleted(ctx, "s1", "alice", "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mem.Get(ctx, a.Sandbox.ID); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("still there: %v", err)
	}
	if _, err := os.Stat(a.Workspace); err == nil {
		t.Fatal("its folder is still there")
	}
	if _, err := m.Workspace(ctx, "s1", "alice", "default"); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("workspace of a deleted session: %v", err)
	}
}

// A sweep removes only sandboxes made before it whose session is gone, and
// never one bound here or one it cannot judge.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	old := r.manager(t, nil)
	gone, alive, unknown := r.bind(t, old, "gone", "a"), r.bind(t, old, "alive", "a"), r.bind(t, old, "unknown", "a")
	m := r.manager(t, nil)
	here := r.bind(t, m, "here", "a")
	since := time.Now().Add(time.Hour)
	err := m.Sweep(ctx, since, func(_ context.Context, id string) (bool, error) {
		switch id {
		case "alive":
			return true, nil
		case "unknown":
			return false, errors.New("store down")
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.mem.Get(ctx, gone.Sandbox.ID); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatal("the gone session's sandbox was kept")
	}
	for _, b := range []Bound{alive, unknown, here} {
		if _, err := r.mem.Get(ctx, b.Sandbox.ID); err != nil {
			t.Fatalf("%s was swept: %v", b.Sandbox.Labels, err)
		}
	}
	newer := r.bind(t, old, "newer", "a")
	if err := m.Sweep(ctx, newer.Sandbox.CreatedAt, func(context.Context, string) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mem.Get(ctx, newer.Sandbox.ID); err != nil {
		t.Fatal("a sandbox made after the sweep began was swept")
	}
}

// A sandbox that runs with no isolation is refused and not left behind.
func TestNoIsolationIsRefused(t *testing.T) {
	r := newRig(t)
	r.fake.SetTier("none")
	m := r.manager(t, nil)
	_, err := m.Bind(context.Background(), Binding{SessionID: "s1", User: "alice", Tenant: "default"})
	if !errors.Is(err, controlplane.ErrUnavailable) || !strings.Contains(err.Error(), "isolation") {
		t.Fatalf("a sandbox with no isolation: %v", err)
	}
	items, _, _ := r.mem.List(context.Background(), controlplane.ListOptions{})
	if len(items) != 0 {
		t.Fatalf("the refused sandbox was left: %+v", items)
	}
}

func TestDriverFailureIsRefused(t *testing.T) {
	r := newRig(t)
	r.fake.SetErr("create", driver.Errorf(driver.ErrUnavailable, "engine gone"))
	m := r.manager(t, nil)
	_, err := m.Bind(context.Background(), Binding{SessionID: "s1", User: "alice", Tenant: "default"})
	if err == nil || !strings.Contains(err.Error(), "engine gone") {
		t.Fatalf("a sandbox that failed: %v", err)
	}
}

// scopeGuard is an edition's guard: every create is in a workspace, and it
// keeps what it was asked.
type scopeGuard struct {
	mu      sync.Mutex
	creates []map[string]string
}

func (g *scopeGuard) Admit(_ context.Context, a *controlplane.Admission) (func(), error) {
	if a.Action != controlplane.ActionCreate {
		return nil, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.creates = append(g.creates, a.Next.Labels)
	if a.Next.Labels[wsKey] == "" {
		return nil, fmt.Errorf("%w: every sandbox belongs to a workspace", controlplane.ErrForbidden)
	}
	return nil, nil
}

func (g *scopeGuard) unscoped() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, l := range g.creates {
		if l[wsKey] == "" {
			n++
		}
	}
	return n
}

const wsKey = controlplane.ReservedPrefix + "workspace"

// Under an edition's scope, a session's sandbox and the warm pool it is
// claimed from are made in its user's workspace, so the edition's guard
// admits and counts each there; no unscoped fill is ever made; a scope that
// names a session label, or fails, refuses the session; and a sandbox left
// in another workspace is not reattached.
func TestEditionScopeAppliesToSandboxesAndPoolFills(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	g := &scopeGuard{}
	if err := r.svc.SetGuard(g); err != nil {
		t.Fatal(err)
	}
	m := r.manager(t, func(c *Config) { c.Pool = 1 })
	var mu sync.Mutex
	scopes := map[string]map[string]string{"alice": {wsKey: "pay"}, "bob": {wsKey: "ops"},
		"mallory": {controlplane.LabelSession: "s1"}}
	m.SetScope(func(_ context.Context, req ScopeRequest) (map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if s, ok := scopes[req.User]; ok && req.Tenant == "default" {
			return s, nil
		}
		return nil, fmt.Errorf("%w: %s is in no workspace", controlplane.ErrForbidden, req.User)
	})
	runCtx, stop := context.WithCancel(ctx)
	ran := make(chan struct{})
	go func() { defer close(ran); m.Run(runCtx) }()

	a := r.bind(t, m, "s1", "alice")
	if a.Sandbox.Labels[wsKey] != "pay" || a.Sandbox.Labels[controlplane.LabelSession] != "s1" {
		t.Fatalf("alice's sandbox: %v", a.Sandbox.Labels)
	}
	// The pay pool fills in the background, in pay.
	m.mu.Lock()
	pay := m.pools[scopeKey(map[string]string{wsKey: "pay"})]
	m.mu.Unlock()
	if pay == nil {
		t.Fatal("no warm pool for pay")
	}
	var member controlplane.Sandbox
	for deadline := time.Now().Add(10 * time.Second); member.ID == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		ms, _ := pay.Members(ctx)
		if len(ms) == 1 {
			var err error
			if member, err = r.svc.WaitReady(ctx, controlplane.Caller{Principal: controlplane.SessionsPrincipal, Scope: map[string]string{controlplane.LabelPool: DefaultTemplate}}, ms[0].ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if member.Labels[wsKey] != "pay" {
		t.Fatalf("the pay pool's member: %+v", member.Labels)
	}
	if b := r.bind(t, m, "s2", "alice"); b.Sandbox.ID != member.ID || b.Sandbox.Labels[wsKey] != "pay" {
		t.Fatalf("alice's second session was not claimed from pay's pool: %s %v", b.Sandbox.ID, b.Sandbox.Labels)
	}
	if b := r.bind(t, m, "s3", "bob"); b.Sandbox.Labels[wsKey] != "ops" || b.Sandbox.ID == member.ID {
		t.Fatalf("bob's sandbox: %s %v", b.Sandbox.ID, b.Sandbox.Labels)
	}
	for _, user := range []string{"mallory", "carol"} {
		if _, err := m.Bind(ctx, Binding{SessionID: "x-" + user, User: user, Tenant: "default"}); !errors.Is(err, controlplane.ErrForbidden) {
			t.Fatalf("%s's session: %v", user, err)
		}
	}
	mu.Lock()
	scopes["alice"] = map[string]string{wsKey: "ops"}
	mu.Unlock()
	if _, err := m.Bind(ctx, Binding{SessionID: "s1", User: "alice", Tenant: "default"}); !errors.Is(err, controlplane.ErrForbidden) {
		t.Fatalf("a sandbox in pay reattached in ops: %v", err)
	}
	stop()
	<-ran
	if n := g.unscoped(); n != 0 {
		t.Fatalf("%d sandboxes were made outside a workspace", n)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for key, p := range m.pools {
		if left, _ := p.Members(ctx); len(left) != 0 {
			t.Fatalf("pool %q kept %d after Close", key, len(left))
		}
	}
}

// fourEyesGuard holds every create and delete for approval unless the
// server makes it for a session, as a workspace with four-eyes rules would.
type fourEyesGuard struct {
	mu   sync.Mutex
	seen []controlplane.Admission
}

func (g *fourEyesGuard) Admit(_ context.Context, a *controlplane.Admission) (func(), error) {
	g.mu.Lock()
	g.seen = append(g.seen, *a)
	g.mu.Unlock()
	if (a.Action == controlplane.ActionCreate || a.Action == controlplane.ActionDelete) && a.Origin != controlplane.OriginSession && a.Approval == "" {
		return nil, &controlplane.ApprovalNeeded{ID: "ap-1"}
	}
	return nil, nil
}

// The server's own work on a session's sandbox, and its warm pool, shows
// the guard OriginSession, so four-eyes rules need not hold a session a
// person already started; the same create asked by a person does not.
func TestSessionWorkCarriesItsOrigin(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	g := &fourEyesGuard{}
	if err := r.svc.SetGuard(g); err != nil {
		t.Fatal(err)
	}
	m := r.manager(t, func(c *Config) { c.Pool = 1 })
	if err := m.Pool().Fill(ctx); err != nil {
		t.Fatalf("a pool fill was held: %v", err)
	}
	a := r.bind(t, m, "s1", "alice")
	b := r.bind(t, m, "s2", "bob")
	if _, err := run(t, a, a.Workspace, "true"); err != nil {
		t.Fatal(err)
	}
	if err := m.SessionDeleted(ctx, "s2", "bob", "default"); err != nil {
		t.Fatalf("deleting a session's sandbox was held: %v", err)
	}
	if _, err := r.mem.Get(ctx, b.Sandbox.ID); err == nil {
		t.Fatal("the deleted session's sandbox is still there")
	}
	g.mu.Lock()
	for _, s := range g.seen {
		if s.Origin != controlplane.OriginSession {
			t.Errorf("%s on %s was not marked as the session's: %+v", s.Action, s.Sandbox.ID, s)
		}
	}
	g.mu.Unlock()
	// A person's own create gets no such origin.
	if _, err := r.svc.Create(ctx, controlplane.Caller{Principal: "alice"}, controlplane.CreateRequest{Template: "x"}); !errors.Is(err, controlplane.ErrApprovalRequired) {
		t.Fatalf("a person's create: %v", err)
	}
}
