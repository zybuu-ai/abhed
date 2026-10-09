package controlplane_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/controlplane/storetest"
)

var t0 = time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) cp.Store { return cp.NewMemory() })
}

// allowed is the lifecycle written out again, independently of the table in
// state.go, so a change to either is caught here.
var allowed = map[[2]cp.State]bool{
	{cp.StateProvisioning, cp.StateReady}:    true,
	{cp.StateProvisioning, cp.StateError}:    true,
	{cp.StateProvisioning, cp.StateDeleting}: true,
	{cp.StateReady, cp.StateStopping}:        true,
	{cp.StateReady, cp.StateCompleted}:       true,
	{cp.StateReady, cp.StateError}:           true,
	{cp.StateReady, cp.StateDeleting}:        true,
	{cp.StateStopping, cp.StateStopped}:      true,
	{cp.StateStopping, cp.StateError}:        true,
	{cp.StateStopping, cp.StateDeleting}:     true,
	{cp.StateStopped, cp.StateStarting}:      true,
	{cp.StateStopped, cp.StateDeleting}:      true,
	{cp.StateStarting, cp.StateReady}:        true,
	{cp.StateStarting, cp.StateError}:        true,
	{cp.StateStarting, cp.StateDeleting}:     true,
	{cp.StateCompleted, cp.StateStarting}:    true,
	{cp.StateCompleted, cp.StateDeleting}:    true,
	{cp.StateError, cp.StateStarting}:        true,
	{cp.StateError, cp.StateDeleting}:        true,
}

func TestEveryTransition(t *testing.T) {
	if len(cp.States) != 8 {
		t.Fatalf("%d states; update this test with the contract", len(cp.States))
	}
	all := append(append([]cp.State{}, cp.States...), "", "Sleeping")
	for _, from := range all {
		for _, to := range all {
			want := allowed[[2]cp.State{from, to}]
			if got := cp.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
			s := cp.Sandbox{State: from, Generation: 4, UpdatedAt: t0}
			err := s.Transition(to, t0.Add(time.Second))
			if want {
				gen := int64(4)
				if to == cp.StateStarting {
					gen = 5
				}
				if err != nil || s.State != to || s.Generation != gen || !s.UpdatedAt.Equal(t0.Add(time.Second)) {
					t.Errorf("%s to %s: %v, now %+v", from, to, err, s)
				}
				continue
			}
			if !errors.Is(err, cp.ErrInvalidTransition) {
				t.Errorf("%s to %s allowed: %v", from, to, err)
			}
			if s.State != from || s.Generation != 4 || !s.UpdatedAt.Equal(t0) {
				t.Errorf("refused %s to %s changed the sandbox: %+v", from, to, s)
			}
		}
	}
}

func TestLifecycleShape(t *testing.T) {
	for _, s := range cp.States {
		if !s.Valid() {
			t.Errorf("%s not valid", s)
		}
		if s != cp.StateDeleting && !cp.CanTransition(s, cp.StateDeleting) {
			t.Errorf("%s cannot be deleted", s)
		}
		if cp.AcceptsWork(s) != (s == cp.StateReady) {
			t.Errorf("AcceptsWork(%s)", s)
		}
	}
	if cp.State("Sleeping").Valid() {
		t.Error("unknown state valid")
	}
}

func TestNewSandbox(t *testing.T) {
	s, err := cp.NewSandbox("alice", "default", "process", map[string]string{"team": "a"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !cp.ValidID(s.ID) || s.Generation != 1 || s.State != cp.StateProvisioning || s.Owner != "alice" || !s.CreatedAt.Equal(t0) {
		t.Fatalf("%+v", s)
	}
	s2, _ := cp.NewSandbox("alice", "default", "process", nil, t0)
	if s2.ID == s.ID {
		t.Fatal("ids repeat")
	}
	for name, f := range map[string]func() error{
		"bad driver":  func() error { _, err := cp.NewSandbox("a", "t", "Docker!", nil, t0); return err },
		"no template": func() error { _, err := cp.NewSandbox("a", "", "process", nil, t0); return err },
		"no owner":    func() error { _, err := cp.NewSandbox("", "t", "process", nil, t0); return err },
		"reserved label": func() error {
			_, err := cp.NewSandbox("a", "t", "process", map[string]string{"abhed.dev/owner": "x"}, t0)
			return err
		},
	} {
		if err := f(); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, bad := range []string{"", "sbx_", "sbx_0123456789abcdeg", "sbx_0123456789ABCDEF", "abc_0123456789abcdef", "sbx_0123456789abcdef:exec"} {
		if cp.ValidID(bad) {
			t.Errorf("ValidID(%q)", bad)
		}
	}
}

func TestSetTemplateBumpsGeneration(t *testing.T) {
	s := cp.Sandbox{Template: "a", Generation: 2}
	if err := s.SetTemplate("a"); err != nil || s.Generation != 2 {
		t.Fatalf("same template: %v %d", err, s.Generation)
	}
	if err := s.SetTemplate("b"); err != nil || s.Generation != 3 || s.Template != "b" {
		t.Fatalf("new template: %v %+v", err, s)
	}
	if err := s.SetTemplate(""); !errors.Is(err, cp.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestSetCondition(t *testing.T) {
	s := cp.Sandbox{Generation: 3}
	c := cp.Condition{Type: cp.CondDriverReady, Status: cp.ConditionFalse, Reason: "Creating"}
	if err := s.SetCondition(c, t0); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Condition(cp.CondDriverReady)
	if !ok || got.ObservedGeneration != 3 || !got.LastTransition.Equal(t0) {
		t.Fatalf("%+v", got)
	}
	c.Reason = "StillCreating"
	_ = s.SetCondition(c, t0.Add(time.Minute))
	got, _ = s.Condition(cp.CondDriverReady)
	if !got.LastTransition.Equal(t0) || got.Reason != "StillCreating" {
		t.Fatalf("same status moved the transition time: %+v", got)
	}
	c.Status = cp.ConditionTrue
	_ = s.SetCondition(c, t0.Add(2*time.Minute))
	got, _ = s.Condition(cp.CondDriverReady)
	if !got.LastTransition.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("status change kept the old time: %+v", got)
	}
	if len(s.Conditions) != 1 {
		t.Fatalf("%d conditions", len(s.Conditions))
	}
	if err := s.SetCondition(cp.Condition{Type: "x", Status: "Maybe"}, t0); !errors.Is(err, cp.ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.SetCondition(cp.Condition{Status: cp.ConditionTrue}, t0); !errors.Is(err, cp.ErrInvalid) {
		t.Fatal(err)
	}
	if _, ok := s.Condition("missing"); ok {
		t.Fatal("missing condition found")
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := cp.Sandbox{Labels: map[string]string{"a": "1"}, Conditions: []cp.Condition{{Type: "x"}}}
	c := s.Clone()
	c.Labels["a"] = "2"
	c.Conditions[0].Type = "y"
	if s.Labels["a"] != "1" || s.Conditions[0].Type != "x" {
		t.Fatal("clone shares state")
	}
}

func TestValidateLabels(t *testing.T) {
	good := map[string]string{"team": "payments", "app.kubernetes.io/name": "x", "a": "", "k_8.s-1": "v.1_x-2", "abhed.dev/session": "s1"}
	if err := cp.ValidateLabels(good, true); err != nil {
		t.Fatal(err)
	}
	if err := cp.ValidateLabels(map[string]string{"abhed.dev/session": "s"}, false); !errors.Is(err, cp.ErrInvalid) {
		t.Fatal("reserved key accepted from a client")
	}
	for _, bad := range []map[string]string{
		{"": "v"}, {"-a": "v"}, {"a-": "v"}, {"a b": "v"}, {"a": "-v"}, {"a": "v v"},
		{strings.Repeat("a", 64): "v"}, {"a": strings.Repeat("v", 64)}, {"Upper.Case/a": "v"}, {"/a": "v"}, {"p/": "v"},
		{"a/b/c": "v"},
	} {
		if err := cp.ValidateLabels(bad, true); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("%v accepted", bad)
		}
	}
	many := map[string]string{}
	for i := range cp.MaxLabels + 1 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	if err := cp.ValidateLabels(many, false); !errors.Is(err, cp.ErrInvalid) {
		t.Error("too many labels accepted")
	}
}

func TestSelectors(t *testing.T) {
	labels := map[string]string{"team": "a", "env": "prod", "abhed.dev/session": "s1"}
	cases := []struct {
		sel  string
		want bool
	}{
		{"", true},
		{"team=a", true},
		{"team==a", true},
		{"team=b", false},
		{"team!=b", true},
		{"team!=a", false},
		{"missing!=x", true},
		{"team", true},
		{"missing", false},
		{"!missing", true},
		{"!team", false},
		{"env in (prod, staging)", true},
		{"env in (dev)", false},
		{"missing in (x)", false},
		{"env notin (dev,staging)", true},
		{"env notin (prod)", false},
		{"missing notin (x)", true},
		{"team=a, env in (prod), !gone", true},
		{"team=a,env=dev", false},
		{"abhed.dev/session=s1", true},
	}
	for _, c := range cases {
		sel, err := cp.ParseSelector(c.sel)
		if err != nil {
			t.Errorf("%q: %v", c.sel, err)
			continue
		}
		if got := sel.Matches(labels); got != c.want {
			t.Errorf("%q matches %v, want %v", c.sel, got, c.want)
		}
	}
	for _, bad := range []string{"team=a,,env=prod", "team=a=b", "env in prod", "env in (prod", "env between (a,b)",
		"env in ()", "a b", "=v", "!", "env in (a,(b))", "Bad Key=v", "k=v v", "env in (a, -b)"} {
		if _, err := cp.ParseSelector(bad); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestCheckIfMatch(t *testing.T) {
	ok := []string{"", "*", `"7"`, ` "7" `, `"3", "7"`}
	for _, h := range ok {
		if err := cp.CheckIfMatch(h, 7); err != nil {
			t.Errorf("%q: %v", h, err)
		}
	}
	if err := cp.CheckIfMatch(`"6"`, 7); !errors.Is(err, cp.ErrPreconditionFailed) {
		t.Errorf("stale: %v", err)
	}
	for _, h := range []string{`W/"7"`, `7`, `"x"`, `""`, `"0"`, `"-7"`, `"7`} {
		if err := cp.CheckIfMatch(h, 7); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("%q: %v", h, err)
		}
	}
	if cp.ETag(7) != `"7"` {
		t.Fatal(cp.ETag(7))
	}
}

func TestIdempotency(t *testing.T) {
	ctx := context.Background()
	now := t0
	idem := cp.Idempotency{Store: cp.NewMemory(), Now: func() time.Time { return now }}
	var runs int
	fn := func() (cp.Response, error) {
		runs++
		return cp.Response{Status: 201, Body: []byte(fmt.Sprintf("run %d", runs))}, nil
	}
	resp, replayed, err := idem.Do(ctx, "alice", "POST", "/v1/sandboxes", "k1", []byte("{}"), fn)
	if err != nil || replayed || resp.Status != 201 || string(resp.Body) != "run 1" {
		t.Fatalf("first: %+v %v %v", resp, replayed, err)
	}
	resp, replayed, err = idem.Do(ctx, "alice", "POST", "/v1/sandboxes", "k1", []byte("{}"), fn)
	if err != nil || !replayed || string(resp.Body) != "run 1" || runs != 1 {
		t.Fatalf("repeat: %+v %v %v runs=%d", resp, replayed, err, runs)
	}
	if _, _, err := idem.Do(ctx, "alice", "POST", "/v1/sandboxes", "k1", []byte(`{"x":1}`), fn); !errors.Is(err, cp.ErrIdempotencyMismatch) {
		t.Fatalf("other body: %v", err)
	}
	// Another principal, or another route, has its own key space.
	if _, replayed, _ := idem.Do(ctx, "bob", "POST", "/v1/sandboxes", "k1", []byte("{}"), fn); replayed || runs != 2 {
		t.Fatalf("bob replayed alice's response (runs=%d)", runs)
	}
	if _, replayed, _ := idem.Do(ctx, "alice", "DELETE", "/v1/sandboxes/x", "k1", []byte("{}"), fn); replayed || runs != 3 {
		t.Fatalf("another route replayed (runs=%d)", runs)
	}
	now = now.Add(cp.IdempotencyTTL + time.Second)
	if _, replayed, _ := idem.Do(ctx, "alice", "POST", "/v1/sandboxes", "k1", []byte("{}"), fn); replayed || runs != 4 {
		t.Fatal("an expired key replayed")
	}
	// No key: always runs.
	_, _, _ = idem.Do(ctx, "alice", "POST", "/v1/sandboxes", "", nil, fn)
	_, _, _ = idem.Do(ctx, "alice", "POST", "/v1/sandboxes", "", nil, fn)
	if runs != 6 {
		t.Fatalf("keyless runs=%d", runs)
	}
	for _, bad := range []string{strings.Repeat("k", 256), "has space", "tab\t", "é"} {
		if _, _, err := idem.Do(ctx, "alice", "POST", "/p", bad, nil, fn); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("key %q: %v", bad, err)
		}
	}
}

func TestIdempotencyFailureReleases(t *testing.T) {
	ctx := context.Background()
	idem := cp.Idempotency{Store: cp.NewMemory()}
	boom := errors.New("validation failed")
	if _, _, err := idem.Do(ctx, "a", "POST", "/p", "k", nil, func() (cp.Response, error) { return cp.Response{}, boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	resp, replayed, err := idem.Do(ctx, "a", "POST", "/p", "k", nil, func() (cp.Response, error) { return cp.Response{Status: 200}, nil })
	if err != nil || replayed || resp.Status != 200 {
		t.Fatalf("retry after a failure: %+v %v %v", resp, replayed, err)
	}
	// An error status is a response: it replays.
	_, _, _ = idem.Do(ctx, "a", "POST", "/q", "k", nil, func() (cp.Response, error) { return cp.Response{Status: 409}, nil })
	resp, replayed, _ = idem.Do(ctx, "a", "POST", "/q", "k", nil, func() (cp.Response, error) { return cp.Response{Status: 201}, nil })
	if !replayed || resp.Status != 409 {
		t.Fatalf("error status not replayed: %+v %v", resp, replayed)
	}
}

func TestIdempotencyInFlightAndLease(t *testing.T) {
	ctx := context.Background()
	now := t0
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	idem := cp.Idempotency{Store: cp.NewMemory(), Now: clock}
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = idem.Do(ctx, "a", "POST", "/p", "k", nil, func() (cp.Response, error) {
			close(inside)
			<-release
			return cp.Response{Status: 201}, nil
		})
	}()
	<-inside
	if _, _, err := idem.Do(ctx, "a", "POST", "/p", "k", nil, nil); !errors.Is(err, cp.ErrIdempotencyInFlight) {
		t.Fatalf("repeat while running: %v", err)
	}
	close(release)
	<-done

	// A claim whose server died frees the key after the lease.
	st := cp.NewMemory()
	_, _, _ = st.Claim(ctx, cp.IdemRecord{Scope: cp.Scope("a", "POST", "/p"), Key: "dead",
		RequestHash: fmt.Sprintf("%x", sha256.Sum256(nil)), ExpiresAt: t0.Add(cp.PendingLease)}, t0)
	idem = cp.Idempotency{Store: st, Now: clock}
	if _, _, err := idem.Do(ctx, "a", "POST", "/p", "dead", nil, nil); !errors.Is(err, cp.ErrIdempotencyInFlight) {
		t.Fatalf("within the lease: %v", err)
	}
	mu.Lock()
	now = t0.Add(cp.PendingLease + time.Second)
	mu.Unlock()
	if _, replayed, err := idem.Do(ctx, "a", "POST", "/p", "dead", nil, func() (cp.Response, error) { return cp.Response{Status: 200}, nil }); err != nil || replayed {
		t.Fatalf("after the lease: %v %v", replayed, err)
	}
}

// renewSeen tells the test each time a claim is renewed, and to when.
type renewSeen struct {
	cp.Store
	renewed chan time.Time
}

func (r renewSeen) Renew(ctx context.Context, scope, key, token string, expires time.Time) error {
	err := r.Store.Renew(ctx, scope, key, token, expires)
	if err == nil {
		select {
		case r.renewed <- expires:
		default:
		}
	}
	return err
}

// A request that runs past the lease keeps its claim, so a repeat after
// PendingLease does not run it a second time.
func TestIdempotencyLongRunKeepsClaim(t *testing.T) {
	ctx := context.Background()
	now := t0
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	st := renewSeen{Store: cp.NewMemory(), renewed: make(chan time.Time, 1)}
	idem := cp.Idempotency{Store: st, Now: clock, RenewEvery: 5 * time.Millisecond}
	var runs atomic.Int32
	inside, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fn := func() (cp.Response, error) {
		if runs.Add(1) > 1 {
			return cp.Response{}, errors.New("ran a second time")
		}
		close(inside)
		<-release
		return cp.Response{Status: 200, Body: []byte("once")}, nil
	}
	go func() { defer close(done); _, _, _ = idem.Do(ctx, "a", "POST", "/exec", "k", nil, fn) }()
	<-inside
	later := t0.Add(cp.PendingLease + time.Minute)
	mu.Lock()
	now = later
	mu.Unlock()
	for exp := range st.renewed {
		if exp.After(later) {
			break
		}
	}
	if _, _, err := idem.Do(ctx, "a", "POST", "/exec", "k", nil, fn); !errors.Is(err, cp.ErrIdempotencyInFlight) {
		t.Fatalf("repeat after the lease: %v", err)
	}
	close(release)
	<-done
	resp, replayed, err := idem.Do(ctx, "a", "POST", "/exec", "k", nil, fn)
	if err != nil || !replayed || string(resp.Body) != "once" || runs.Load() != 1 {
		t.Fatalf("replay %q %v %v, ran %d times", resp.Body, replayed, err, runs.Load())
	}
}

// A request whose claim lapsed (its server stalled past the lease) cannot
// complete the claim a later request now holds.
func TestIdempotencyLapsedClaimCannotComplete(t *testing.T) {
	ctx := context.Background()
	now := t0
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	idem := cp.Idempotency{Store: cp.NewMemory(), Now: clock, RenewEvery: time.Hour}
	inside, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := idem.Do(ctx, "a", "POST", "/exec", "k", nil, func() (cp.Response, error) {
			close(inside)
			<-release
			return cp.Response{Status: 200, Body: []byte("first")}, nil
		})
		done <- err
	}()
	<-inside
	mu.Lock()
	now = t0.Add(cp.PendingLease + time.Second)
	mu.Unlock()
	inside2, release2, done2 := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := idem.Do(ctx, "a", "POST", "/exec", "k", nil, func() (cp.Response, error) {
			close(inside2)
			<-release2
			return cp.Response{Status: 200, Body: []byte("second")}, nil
		})
		done2 <- err
	}()
	<-inside2
	close(release)
	if err := <-done; !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("the lapsed request completed: %v", err)
	}
	if _, _, err := idem.Do(ctx, "a", "POST", "/exec", "k", nil, nil); !errors.Is(err, cp.ErrIdempotencyInFlight) {
		t.Fatalf("the second claim was not kept: %v", err)
	}
	close(release2)
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
	if resp, replayed, err := idem.Do(ctx, "a", "POST", "/exec", "k", nil, nil); err != nil || !replayed || string(resp.Body) != "second" {
		t.Fatalf("replay %q %v %v", resp.Body, replayed, err)
	}
}

func TestIdempotencyConcurrentRunsOnce(t *testing.T) {
	ctx := context.Background()
	idem := cp.Idempotency{Store: cp.NewMemory()}
	var runs atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := idem.Do(ctx, "a", "POST", "/p", "k", []byte("b"), func() (cp.Response, error) {
				runs.Add(1)
				time.Sleep(time.Millisecond)
				return cp.Response{Status: 201}, nil
			})
			if err != nil && !errors.Is(err, cp.ErrIdempotencyInFlight) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("ran %d times", runs.Load())
	}
}

func TestMutate(t *testing.T) {
	ctx := context.Background()
	st := cp.NewMemory()
	s, _ := cp.NewSandbox("alice", "default", "process", nil, t0)
	s, err := st.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	// Without If-Match, racing writers all land: none is lost.
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cp.Mutate(ctx, st, s.ID, "", func(x *cp.Sandbox) error {
				if x.Labels == nil {
					x.Labels = map[string]string{}
				}
				x.Labels[fmt.Sprintf("k%d", i)] = "v"
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := st.Get(ctx, s.ID)
	if len(got.Labels) != 4 {
		t.Fatalf("lost updates: %v", got.Labels)
	}
	if _, err := cp.Mutate(ctx, st, s.ID, cp.ETag(s.ResourceVersion), func(*cp.Sandbox) error { return nil }); !errors.Is(err, cp.ErrPreconditionFailed) {
		t.Fatalf("stale If-Match: %v", err)
	}
	up, err := cp.Mutate(ctx, st, s.ID, cp.ETag(got.ResourceVersion), func(x *cp.Sandbox) error {
		return x.Transition(cp.StateReady, t0)
	})
	if err != nil || up.State != cp.StateReady {
		t.Fatalf("current If-Match: %v", err)
	}
	if _, err := cp.Mutate(ctx, st, s.ID, "", func(x *cp.Sandbox) error {
		x.Labels["poison"] = "1"
		return x.Transition(cp.StateStarting, t0)
	}); !errors.Is(err, cp.ErrInvalidTransition) {
		t.Fatalf("refused transition: %v", err)
	}
	after, _ := st.Get(ctx, s.ID)
	if after.ResourceVersion != up.ResourceVersion || after.Labels["poison"] != "" {
		t.Fatal("a refused mutation was written")
	}
}

// With If-Match, losing the race between read and write is a failed
// precondition, never a silent retry onto a version the caller did not see.
func TestMutateIfMatchRace(t *testing.T) {
	ctx := context.Background()
	st := &racingStore{Memory: cp.NewMemory()}
	s, _ := cp.NewSandbox("alice", "default", "process", nil, t0)
	s, _ = st.Create(ctx, s)
	st.race = true
	if _, err := cp.Mutate(ctx, st, s.ID, cp.ETag(s.ResourceVersion), func(*cp.Sandbox) error { return nil }); !errors.Is(err, cp.ErrPreconditionFailed) {
		t.Fatalf("got %v", err)
	}
	st.raced = false
	if _, err := cp.Mutate(ctx, st, s.ID, "", func(*cp.Sandbox) error { return nil }); err != nil || !st.raced {
		t.Fatalf("without If-Match the race is retried: %v (raced %v)", err, st.raced)
	}
}

// racingStore lets another writer in between a Get and the next Update, once.
type racingStore struct {
	*cp.Memory
	race, raced bool
}

func (r *racingStore) Update(ctx context.Context, s cp.Sandbox, expect int64) (cp.Sandbox, error) {
	if r.race && !r.raced {
		r.raced = true
		cur, _ := r.Get(ctx, s.ID)
		if _, err := r.Memory.Update(ctx, cur, cur.ResourceVersion); err != nil {
			return cp.Sandbox{}, err
		}
	}
	return r.Memory.Update(ctx, s, expect)
}

// A restore is allowed only once the instance is not running, and is a new
// generation; refused, it changes nothing.
func TestRestoreBumpsGenerationFromStoppedOnly(t *testing.T) {
	for _, st := range cp.States {
		s := cp.Sandbox{State: st, Generation: 4}
		err := s.Restore(t0)
		if cp.CanRestore(st) {
			if err != nil || s.State != cp.StateStarting || s.Generation != 5 {
				t.Errorf("restore from %s: %v %+v", st, err, s)
			}
			continue
		}
		if !errors.Is(err, cp.ErrInvalidTransition) || s.State != st || s.Generation != 4 {
			t.Errorf("restore from %s was not refused cleanly: %v %+v", st, err, s)
		}
	}
	for _, st := range []cp.State{cp.StateStopped, cp.StateCompleted, cp.StateError} {
		if !cp.CanRestore(st) {
			t.Errorf("%s should allow a restore", st)
		}
	}
}
