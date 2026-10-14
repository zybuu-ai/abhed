package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

// A kick that arrives while a synchronous Reconcile runs is not lost: the
// pass it asked for is queued when Reconcile ends.
func TestKickDuringReconcileIsKept(t *testing.T) {
	reg := driver.NewRegistry()
	if err := reg.Register(drivertest.New("process")); err != nil {
		t.Fatal(err)
	}
	st, err := NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemory()
	svc, err := New(Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return t.TempDir(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sb, _ := NewSandbox("alice", "default", "process", nil, time.Now())
	if sb, err = mem.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc.probe = func(string, State) {
		once.Do(func() { close(entered); <-release })
	}
	done := make(chan error, 1)
	go func() { done <- svc.Reconcile(ctx, sb.ID) }()
	<-entered
	svc.dispatch(ctx, sb.ID) // as Run does with a kick
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-svc.kicks:
		if id != sb.ID {
			t.Fatalf("kicked %s", id)
		}
	default:
		t.Fatal("the kick made during the pass was lost")
	}
}
