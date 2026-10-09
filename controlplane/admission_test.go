package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

func newRigWith(t *testing.T, values map[string]string, adjust func(*cp.Config)) *rig {
	t.Helper()
	st, err := cp.NewSettings(values, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", driver.CapAttach)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	clk := &clock{t: t0}
	ws := t.TempDir()
	c := cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }, Now: clk.now}
	adjust(&c)
	svc, err := cp.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{svc: svc, store: mem, fake: fake, clock: clk}
}

// hook is an Interceptor driven by a function, which records what it saw.
type hook struct {
	mu        sync.Mutex
	admit     func(req cp.AdmissionRequest) (cp.AdmissionResult, error)
	seen      []cp.AdmissionRequest
	committed []string
}

func (h *hook) Admit(_ context.Context, req cp.AdmissionRequest) (cp.AdmissionResult, error) {
	h.mu.Lock()
	h.seen = append(h.seen, req)
	h.mu.Unlock()
	if h.admit == nil {
		return cp.AdmissionResult{Object: req.Object, Decisions: []cp.AdmissionDecision{{Hook: "h", Phase: "validate", Decision: "allow"}}}, nil
	}
	return h.admit(req)
}

func (h *hook) Committed(_ context.Context, req cp.AdmissionRequest, _ cp.Sandbox, err error) []cp.AdmissionDecision {
	h.mu.Lock()
	defer h.mu.Unlock()
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	h.committed = append(h.committed, req.Operation+":"+outcome)
	return []cp.AdmissionDecision{{Hook: "audit", Phase: "post_commit", Decision: "allow"}}
}

func (h *hook) ops() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.seen {
		out = append(out, r.Operation)
	}
	return out
}

func admissions(t *testing.T, r *rig, id string) []cp.AuditEvent {
	t.Helper()
	evs, err := r.store.AuditFor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []cp.AuditEvent
	for _, ev := range evs {
		if ev.Action == "admission" {
			out = append(out, ev)
		}
	}
	return out
}

func TestEveryMutationIsAdmittedAndRecorded(t *testing.T) {
	h := &hook{}
	r := newRigWith(t, nil, func(c *cp.Config) { c.Interceptor = h })
	ctx := context.Background()
	sb := r.ready(t)
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "echo token=hunter2"}, cp.ExecIO{}); err != nil {
		t.Fatal(err)
	}
	v := "x"
	if _, err := r.svc.Update(ctx, alice, sb.ID, "*", cp.Patch{Labels: map[string]*string{"a": &v}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Stop(ctx, alice, sb.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	r.reconcile(t, sb.ID)
	if _, err := r.svc.Start(ctx, alice, sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Delete(ctx, alice, sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(h.ops(), ","), "create,exec,update,stop,start,delete"; got != want {
		t.Fatalf("admitted %s, want %s", got, want)
	}
	if got := strings.Join(h.committed, ","); got != "create:ok,exec:ok,update:ok,stop:ok,start:ok,delete:ok" {
		t.Fatalf("committed %s", got)
	}
	for _, req := range h.seen {
		if req.Operation == cp.OpExec && strings.Contains(req.Command, "hunter2") {
			t.Fatalf("the interceptor was sent the command unredacted: %q", req.Command)
		}
	}
	evs := admissions(t, r, sb.ID)
	if len(evs) != 12 {
		t.Fatalf("recorded %d admission decisions, want 12", len(evs))
	}
	for _, ev := range evs {
		if ev.Principal != "alice" || ev.Detail["hook"] == "" || ev.Detail["operation"] == "" {
			t.Fatalf("decision recorded without its principal or hook: %+v", ev)
		}
	}
}

func TestDeniedCreateStoresNothing(t *testing.T) {
	h := &hook{admit: func(req cp.AdmissionRequest) (cp.AdmissionResult, error) {
		return cp.AdmissionResult{Decisions: []cp.AdmissionDecision{{Hook: "opa", Phase: "validate", Decision: "deny", Reason: "no"}}},
			cp.ErrDenied
	}}
	r := newRigWith(t, nil, func(c *cp.Config) { c.Interceptor = h })
	_, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default"})
	if !errors.Is(err, cp.ErrDenied) || cp.ErrorCode(err) != "forbidden" {
		t.Fatalf("denied create: %v (%s)", err, cp.ErrorCode(err))
	}
	if items, _, _ := r.store.List(context.Background(), cp.ListOptions{}); len(items) != 0 {
		t.Fatal("a denied sandbox was stored")
	}
	id := h.seen[0].SandboxID
	evs := admissions(t, r, id)
	if len(evs) != 1 || evs[0].Outcome != cp.OutcomeFailed || evs[0].Detail["decision"] != "deny" || evs[0].Principal != "alice" {
		t.Fatalf("the denial is not recorded: %+v", evs)
	}
}

func TestAdmissionFaultsFailClosed(t *testing.T) {
	for name, err := range map[string]error{
		"unavailable": cp.ErrUnavailable,
		"unnamed":     errors.New("socket closed"),
		"timeout":     context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			h := &hook{}
			r := newRigWith(t, nil, func(c *cp.Config) { c.Interceptor = h })
			sb := r.ready(t)
			h.admit = func(req cp.AdmissionRequest) (cp.AdmissionResult, error) {
				return cp.AdmissionResult{Object: req.Object}, err
			}
			ctx := context.Background()
			if _, _, gotErr := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); !errors.Is(gotErr, cp.ErrUnavailable) {
				t.Fatalf("exec with a failing interceptor: %v", gotErr)
			}
			if r.fake.Calls("exec") != 0 {
				t.Fatal("the command ran though admission failed")
			}
			before, _ := r.store.Get(ctx, sb.ID)
			v := "x"
			if _, gotErr := r.svc.Update(ctx, alice, sb.ID, "", cp.Patch{Labels: map[string]*string{"a": &v}}); !errors.Is(gotErr, cp.ErrUnavailable) {
				t.Fatalf("update with a failing interceptor: %v", gotErr)
			}
			if after, _ := r.store.Get(ctx, sb.ID); after.ResourceVersion != before.ResourceVersion {
				t.Fatal("the update was written though admission failed")
			}
		})
	}
}

func TestPatchesMayOnlyTighten(t *testing.T) {
	values := map[string]string{cp.SetMinTier: "process", cp.SetExecTimeout: "300"}
	cases := map[string]struct {
		patch func(sb *cp.Sandbox)
		ok    bool
	}{
		"tighter timeout":       {func(sb *cp.Sandbox) { sb.Settings = map[string]string{cp.SetExecTimeout: "60"} }, true},
		"stronger tier":         {func(sb *cp.Sandbox) { sb.Settings = map[string]string{cp.SetMinTier: "vm"} }, true},
		"a label":               {func(sb *cp.Sandbox) { sb.Labels = map[string]string{"team": "a"} }, true},
		"looser timeout":        {func(sb *cp.Sandbox) { sb.Settings = map[string]string{cp.SetExecTimeout: "900"} }, false},
		"lower tier":            {func(sb *cp.Sandbox) { sb.Settings = map[string]string{cp.SetMinTier: "none"} }, false},
		"looser restart":        {func(sb *cp.Sandbox) { sb.Settings = map[string]string{cp.SetRestartPolicy: "always"} }, false},
		"a global setting":      {func(sb *cp.Sandbox) { sb.Settings = map[string]string{cp.SetMutationsPerMin: "1"} }, false},
		"a reserved label":      {func(sb *cp.Sandbox) { sb.Labels = map[string]string{"abhed.dev/session": "s"} }, false},
		"the template":          {func(sb *cp.Sandbox) { sb.Template = "open" }, false},
		"the owner":             {func(sb *cp.Sandbox) { sb.Owner = "mallory" }, false},
		"the driver":            {func(sb *cp.Sandbox) { sb.Driver = "other" }, false},
		"the state":             {func(sb *cp.Sandbox) { sb.State = cp.StateReady }, false},
		"the policy revision":   {func(sb *cp.Sandbox) { sb.PolicyRev = "p-0" }, false},
		"the tier":              {func(sb *cp.Sandbox) { sb.Tier = "vm" }, false},
		"an invalid label name": {func(sb *cp.Sandbox) { sb.Labels = map[string]string{"-bad": "x"} }, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := &hook{admit: func(req cp.AdmissionRequest) (cp.AdmissionResult, error) {
				out := req.Object.Clone()
				c.patch(&out)
				return cp.AdmissionResult{Object: out, Decisions: []cp.AdmissionDecision{{Hook: "p", Phase: "patch", Decision: "patch"}}}, nil
			}}
			r := newRigWith(t, values, func(c *cp.Config) { c.Interceptor = h })
			sb, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default"})
			if c.ok {
				if err != nil {
					t.Fatalf("an allowed patch was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, cp.ErrDenied) {
				t.Fatalf("a widening patch was accepted: %+v %v", sb, err)
			}
			if items, _, _ := r.store.List(context.Background(), cp.ListOptions{}); len(items) != 0 {
				t.Fatal("a refused patch was stored")
			}
			evs := admissions(t, r, h.seen[0].SandboxID)
			if last := evs[len(evs)-1]; last.Detail["hook"] != "abhed" || last.Detail["decision"] != "deny" {
				t.Fatalf("the refusal is not recorded: %+v", evs)
			}
		})
	}
}

func TestPatchedSettingsAreKeptAndExecCannotBePatched(t *testing.T) {
	h := &hook{admit: func(req cp.AdmissionRequest) (cp.AdmissionResult, error) {
		out := req.Object.Clone()
		switch req.Operation {
		case cp.OpCreate:
			out.Labels = map[string]string{"patched": "yes"}
		case cp.OpExec:
			out.Labels = map[string]string{"patched": "again"}
		}
		return cp.AdmissionResult{Object: out}, nil
	}}
	r := newRigWith(t, nil, func(c *cp.Config) { c.Interceptor = h })
	sb := r.ready(t)
	if sb.Labels["patched"] != "yes" {
		t.Fatalf("the patch was not committed: %+v", sb.Labels)
	}
	if _, _, err := r.svc.Exec(context.Background(), alice, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); !errors.Is(err, cp.ErrDenied) {
		t.Fatalf("a patched exec: %v", err)
	}
	if r.fake.Calls("exec") != 0 {
		t.Fatal("a patched exec ran")
	}
}

type flakyAudit struct {
	*cp.Memory
	refuse string
}

func (f *flakyAudit) Append(ctx context.Context, ev cp.AuditEvent) error {
	if ev.Action == f.refuse {
		return errors.New("disk full")
	}
	return f.Memory.Append(ctx, ev)
}

func TestUnrecordableDecisionRefuses(t *testing.T) {
	r := newRigWith(t, nil, func(c *cp.Config) {
		c.Interceptor = &hook{}
		c.Audit = &flakyAudit{Memory: c.Store.(*cp.Memory), refuse: "admission"}
	})
	if _, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default"}); !errors.Is(err, cp.ErrUnavailable) {
		t.Fatalf("create whose admission could not be recorded: %v", err)
	}
	if items, _, _ := r.store.List(context.Background(), cp.ListOptions{}); len(items) != 0 {
		t.Fatal("stored though its admission was not recorded")
	}
}

func TestNodeIDIsRecorded(t *testing.T) {
	r := newRigWith(t, nil, func(c *cp.Config) { c.NodeID = "node-a" })
	sb := r.ready(t)
	evs, _ := r.store.AuditFor(context.Background(), sb.ID)
	if len(evs) == 0 {
		t.Fatal("nothing recorded")
	}
	for _, ev := range evs {
		if ev.Detail["node"] != "node-a" {
			t.Fatalf("event without the node: %+v", ev)
		}
	}
}

// owner is an Ownership a test sets.
type owner struct {
	mu    sync.Mutex
	mine  bool
	err   error
	asked int
}

func (o *owner) Owns(context.Context, string) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.asked++
	return o.mine, o.err
}

func (o *owner) set(mine bool, err error) {
	o.mu.Lock()
	o.mine, o.err = mine, err
	o.mu.Unlock()
}

func TestOnlyTheOwnerActs(t *testing.T) {
	o := &owner{mine: true}
	r := newRigWith(t, nil, func(c *cp.Config) { c.Ownership = o })
	ctx := context.Background()
	sb := r.ready(t)
	for name, err := range map[string]error{"another replica": nil, "unreadable": errors.New("db down")} {
		t.Run(name, func(t *testing.T) {
			o.set(false, err)
			if _, _, gotErr := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); !errors.Is(gotErr, cp.ErrUnavailable) {
				t.Fatalf("exec on a sandbox not owned here: %v", gotErr)
			}
			if _, gotErr := r.svc.Stop(ctx, alice, sb.ID, "", 0); !errors.Is(gotErr, cp.ErrUnavailable) {
				t.Fatalf("stop on a sandbox not owned here: %v", gotErr)
			}
			r.fake.SetPhase(sb.ID, driver.PhaseFailed)
			calls := r.fake.Calls("status")
			if got := r.reconcile(t, sb.ID); got.State != cp.StateReady || r.fake.Calls("status") != calls {
				t.Fatalf("a replica that does not own the sandbox reconciled it: %s", got.State)
			}
			r.fake.SetPhase(sb.ID, driver.PhaseRunning)
		})
	}
	if r.fake.Calls("exec") != 0 {
		t.Fatal("a command ran on a replica that does not own the sandbox")
	}
	o.set(true, nil)
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); err != nil {
		t.Fatalf("exec on the owner: %v", err)
	}
}

// An interceptor sees whether an exec reads streamed input, where it runs
// and which variables its caller set, by name and never by value.
func TestExecAdmissionCarriesStdinCwdAndEnvNames(t *testing.T) {
	h := &hook{}
	r := newRigWith(t, nil, func(c *cp.Config) { c.Interceptor = h })
	ctx := context.Background()
	sb := r.ready(t)
	req := cp.ExecRequest{Command: "cat", Cwd: "sub", Env: []string{"API_TOKEN=hunter2", "MODE=fast"}, Stdin: true}
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", req, cp.ExecIO{Stdin: strings.NewReader("x")}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	got := h.seen[len(h.seen)-1]
	h.mu.Unlock()
	if got.Operation != cp.OpExec || !got.Stdin || got.Cwd != "sub" || strings.Join(got.EnvNames, ",") != "API_TOKEN,MODE" {
		t.Fatalf("interceptor saw %+v", got)
	}
	if s := fmt.Sprintf("%+v", got); strings.Contains(s, "hunter2") || strings.Contains(s, "fast") {
		t.Fatalf("a variable's value reached the interceptor: %s", s)
	}
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	got = h.seen[len(h.seen)-1]
	h.mu.Unlock()
	if got.Stdin || got.Cwd != "" || got.EnvNames != nil {
		t.Fatalf("a plain exec: %+v", got)
	}

	// A session's command and its terminal shell, through Service.Command.
	st, err := cp.NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", driver.CapCommand)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	root := t.TempDir()
	hc := &hook{}
	svc, err := cp.New(cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st, Interceptor: hc,
		Workspace: func(id string) (string, error) { return root + "/" + id, nil }, Now: (&clock{t: t0}).now})
	if err != nil {
		t.Fatal(err)
	}
	user := cp.Caller{Principal: "alice", Admin: true, Scope: map[string]string{cp.LabelSession: "s1"}}
	maker := cp.Caller{Principal: cp.SessionsPrincipal, Scope: user.Scope}
	csb, err := svc.Create(ctx, maker, cp.CreateRequest{Template: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WaitReady(ctx, user, csb.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		req   driver.CommandRequest
		stdin bool
	}{{driver.CommandRequest{Command: "ls", Cwd: "src"}, false}, {driver.CommandRequest{Shell: true, Cwd: "src"}, true}} {
		if _, _, err := svc.Command(ctx, user, csb.ID, c.req); err != nil {
			t.Fatal(err)
		}
		hc.mu.Lock()
		got := hc.seen[len(hc.seen)-1]
		hc.mu.Unlock()
		if got.Operation != cp.OpExec || got.Stdin != c.stdin || got.Cwd != "src" || got.EnvNames != nil {
			t.Fatalf("%+v: interceptor saw %+v", c.req, got)
		}
	}
}
