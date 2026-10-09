// Package storetest is the conformance suite every controlplane.Store
// passes: memory, SQLite and Postgres alike.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

// Run runs the suite against stores from open, one fresh store per test.
func Run(t *testing.T, open func(t *testing.T) controlplane.Store) {
	t.Run("CreateGet", func(t *testing.T) { createGet(t, open(t)) })
	t.Run("UpdateVersions", func(t *testing.T) { updateVersions(t, open(t)) })
	t.Run("Immutable", func(t *testing.T) { immutableFields(t, open(t)) })
	t.Run("Delete", func(t *testing.T) { deleteGuarded(t, open(t)) })
	t.Run("ListFiltersAndPages", func(t *testing.T) { list(t, open(t)) })
	t.Run("ConcurrentUpdatesOneWins", func(t *testing.T) { concurrentUpdates(t, open(t)) })
	t.Run("Isolation", func(t *testing.T) { isolation(t, open(t)) })
	t.Run("IdempotencyClaims", func(t *testing.T) { claims(t, open(t)) })
	t.Run("IdempotencyConcurrentClaim", func(t *testing.T) { concurrentClaim(t, open(t)) })
	t.Run("Templates", func(t *testing.T) { templates(t, open(t)) })
}

var t0 = time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

func sandbox(t *testing.T, owner string, labels map[string]string) controlplane.Sandbox {
	t.Helper()
	s, err := controlplane.NewSandbox(owner, "default", "process", labels, t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func createGet(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	s := sandbox(t, "alice", map[string]string{"team": "a"})
	got, err := st.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion < 1 {
		t.Fatalf("resource version %d", got.ResourceVersion)
	}
	if _, err := st.Create(ctx, s); !errors.Is(err, controlplane.ErrExists) {
		t.Fatalf("second create: %v", err)
	}
	back, err := st.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != s.ID || back.Owner != "alice" || back.Labels["team"] != "a" || back.State != controlplane.StateProvisioning ||
		back.Generation != 1 || back.ResourceVersion != got.ResourceVersion || !back.CreatedAt.Equal(t0) {
		t.Fatalf("read back %+v", back)
	}
	if _, err := st.Get(ctx, "sbx_0000000000000000"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	bad := sandbox(t, "alice", nil)
	bad.ID = "nope"
	if _, err := st.Create(ctx, bad); !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	bad = sandbox(t, "alice", nil)
	bad.State = "Sleeping"
	if _, err := st.Create(ctx, bad); !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("bad state: %v", err)
	}
}

func updateVersions(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	s, err := st.Create(ctx, sandbox(t, "alice", nil))
	if err != nil {
		t.Fatal(err)
	}
	next := s.Clone()
	if err := next.Transition(controlplane.StateReady, t0); err != nil {
		t.Fatal(err)
	}
	if err := next.SetCondition(controlplane.Condition{Type: controlplane.CondDriverReady, Status: controlplane.ConditionTrue}, t0); err != nil {
		t.Fatal(err)
	}
	up, err := st.Update(ctx, next, s.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	if up.ResourceVersion <= s.ResourceVersion {
		t.Fatalf("version did not go up: %d to %d", s.ResourceVersion, up.ResourceVersion)
	}
	if _, err := st.Update(ctx, next, s.ResourceVersion); !errors.Is(err, controlplane.ErrVersionMismatch) {
		t.Fatalf("stale update: %v", err)
	}
	back, err := st.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.State != controlplane.StateReady || len(back.Conditions) != 1 || back.ResourceVersion != up.ResourceVersion {
		t.Fatalf("after update %+v", back)
	}
	ghost := sandbox(t, "alice", nil)
	if _, err := st.Update(ctx, ghost, 1); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("update of a missing sandbox: %v", err)
	}
}

func immutableFields(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	s, err := st.Create(ctx, sandbox(t, "alice", nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*controlplane.Sandbox){
		"owner":      func(x *controlplane.Sandbox) { x.Owner = "mallory" },
		"driver":     func(x *controlplane.Sandbox) { x.Driver = "docker" },
		"created_at": func(x *controlplane.Sandbox) { x.CreatedAt = t0.Add(time.Hour) },
		"generation": func(x *controlplane.Sandbox) { x.Generation = 0 },
	} {
		next := s.Clone()
		change(&next)
		if _, err := st.Update(ctx, next, s.ResourceVersion); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("%s changed: %v", name, err)
		}
	}
}

func deleteGuarded(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	s, err := st.Create(ctx, sandbox(t, "alice", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, s.ID, s.ResourceVersion+1); !errors.Is(err, controlplane.ErrVersionMismatch) {
		t.Fatalf("delete at the wrong version: %v", err)
	}
	if err := st.Delete(ctx, s.ID, s.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, s.ID); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := st.Delete(ctx, s.ID, s.ResourceVersion); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func list(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	want := map[string]bool{}
	for i := range 7 {
		owner := "alice"
		if i%2 == 1 {
			owner = "bob"
		}
		s, err := st.Create(ctx, sandbox(t, owner, map[string]string{"n": fmt.Sprint(i), "team": map[bool]string{true: "a", false: "b"}[i < 4]}))
		if err != nil {
			t.Fatal(err)
		}
		want[s.ID] = true
	}
	sel, err := controlplane.ParseSelector("team=a")
	if err != nil {
		t.Fatal(err)
	}
	got, next, err := st.List(ctx, controlplane.ListOptions{Selector: sel})
	if err != nil || len(got) != 4 || next != "" {
		t.Fatalf("selector: %d items, next %q, %v", len(got), next, err)
	}
	got, _, err = st.List(ctx, controlplane.ListOptions{Owner: "bob"})
	if err != nil || len(got) != 3 {
		t.Fatalf("owner filter: %d %v", len(got), err)
	}
	got, _, err = st.List(ctx, controlplane.ListOptions{State: controlplane.StateReady})
	if err != nil || len(got) != 0 {
		t.Fatalf("state filter: %d %v", len(got), err)
	}
	got, next, err = st.List(ctx, controlplane.ListOptions{Labels: map[string]string{"team": "a"}, Owner: "alice", Limit: 1})
	if err != nil || len(got) != 1 || next == "" || got[0].Labels["team"] != "a" || got[0].Owner != "alice" {
		t.Fatalf("label filter: %d items, next %q, %v", len(got), next, err)
	}
	if got, _, _ = st.List(ctx, controlplane.ListOptions{Labels: map[string]string{"team": ""}}); len(got) != 0 {
		t.Fatalf("an empty label value matched %d", len(got))
	}

	seen := map[string]bool{}
	after, last := "", ""
	for pages := 0; ; pages++ {
		if pages > 7 {
			t.Fatal("paging does not end")
		}
		page, next, err := st.List(ctx, controlplane.ListOptions{Limit: 3, After: after})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 3 {
			t.Fatalf("page of %d", len(page))
		}
		for _, s := range page {
			if s.ID <= last {
				t.Fatalf("not in id order: %s after %s", s.ID, last)
			}
			last = s.ID
			seen[s.ID] = true
		}
		if next == "" {
			break
		}
		after = next
	}
	if len(seen) != len(want) {
		t.Fatalf("paging saw %d of %d", len(seen), len(want))
	}
}

func concurrentUpdates(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	s, err := st.Create(ctx, sandbox(t, "alice", nil))
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := s.Clone()
			_ = next.Transition(controlplane.StateReady, t0)
			if _, err := st.Update(ctx, next, s.ResourceVersion); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, controlplane.ErrVersionMismatch) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d writers won against one version", wins.Load())
	}
}

// What a caller holds is a copy: changing it changes nothing stored.
func isolation(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	s, err := st.Create(ctx, sandbox(t, "alice", map[string]string{"a": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	s.Labels["a"] = "changed"
	got, err := st.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Labels["a"] = "changed again"
	again, err := st.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Labels["a"] != "1" {
		t.Fatalf("stored labels changed through a copy: %v", again.Labels)
	}
}

func claims(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	rec := controlplane.IdemRecord{Scope: "s", Key: "k", RequestHash: "h", State: controlplane.IdemPending, ExpiresAt: t0.Add(time.Minute)}
	if _, ok, err := st.Claim(ctx, rec, t0); err != nil || !ok {
		t.Fatalf("first claim %v %v", ok, err)
	}
	cur, ok, err := st.Claim(ctx, rec, t0)
	if err != nil || ok || cur.State != controlplane.IdemPending || cur.RequestHash != "h" {
		t.Fatalf("second claim %+v %v %v", cur, ok, err)
	}
	other := rec
	other.Scope = "s2"
	if _, ok, _ := st.Claim(ctx, other, t0); !ok {
		t.Fatal("a key in another scope was taken")
	}
	if err := st.Complete(ctx, "s", "k", 201, []byte("body"), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cur, ok, err = st.Claim(ctx, rec, t0.Add(30*time.Minute))
	if err != nil || ok || cur.State != controlplane.IdemDone || cur.Status != 201 || string(cur.Body) != "body" {
		t.Fatalf("completed claim %+v %v %v", cur, ok, err)
	}
	if _, ok, _ := st.Claim(ctx, rec, t0.Add(2*time.Hour)); !ok {
		t.Fatal("an expired record still held its key")
	}
	if err := st.Release(ctx, "s", "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Claim(ctx, rec, t0.Add(2*time.Hour)); !ok {
		t.Fatal("a released key could not be claimed")
	}
	if err := st.Complete(ctx, "s", "missing", 200, nil, t0); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("complete without a claim: %v", err)
	}
	// Release never removes a finished record.
	if err := st.Complete(ctx, "s", "k", 200, nil, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Release(ctx, "s", "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Claim(ctx, rec, t0.Add(2*time.Hour)); ok {
		t.Fatal("release removed a finished record")
	}
}

func concurrentClaim(t *testing.T, st controlplane.Store) {
	ctx := context.Background()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := controlplane.IdemRecord{Scope: "s", Key: "race", RequestHash: "h", ExpiresAt: t0.Add(time.Minute)}
			if _, ok, err := st.Claim(ctx, rec, t0); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d concurrent claims of one key succeeded", wins.Load())
	}
}
