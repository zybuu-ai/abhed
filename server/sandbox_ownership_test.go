package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/wsconn"
)

// losable owns every sandbox until lose is called.
type losable struct{ lost chan struct{} }

func (l *losable) Owns(context.Context, string) (bool, error) {
	select {
	case <-l.lost:
		return false, nil
	default:
		return true, nil
	}
}

func (l *losable) Watch(context.Context, string) <-chan struct{} { return l.lost }

func (l *losable) lose() { close(l.lost) }

// A stream ends when this replica stops owning its sandbox, recorded as
// failed with "ownership lost" as its reason.
func TestSandboxStreamsEndWhenOwnershipIsLost(t *testing.T) {
	own := &losable{lost: make(chan struct{})}
	fake := drivertest.New("process", driver.CapAttach, driver.CapForward)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	st, err := controlplane.NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := controlplane.NewMemory()
	ws := t.TempDir()
	svc, err := controlplane.New(controlplane.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st, Ownership: own,
		Workspace: func(string) (string, error) { return ws, nil }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"
	srv := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{}),
		Auth:         &auth.Middleware{PublicPaths: PublicPaths(), Verifier: tokens{"alice": {Subject: "alice", Tenant: "default"}}},
		ControlPlane: svc, StreamRecheck: 50 * time.Millisecond})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	r := &wsRig{cpRig: &cpRig{srv: srv, h: srv.Handler(), svc: svc, store: mem, fake: fake}, url: hs.URL}
	shellFake(r.cpRig)
	sb := r.ready(t, "alice")

	attach, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
	if err != nil {
		t.Fatal(err)
	}
	control(t, attach)
	readUntil(t, attach, "$ ")
	exec, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	fake.ExecFunc = func(ctx context.Context, _ string, _ driver.ExecRequest) (driver.ExecResult, error) {
		<-ctx.Done()
		return driver.ExecResult{}, ctx.Err()
	}
	_ = exec.WriteMessage(wsconn.Text, []byte(`{"command":"sleep 600"}`))
	if c, _ := control(t, exec); c.Type != "started" {
		t.Fatalf("exec %+v", c)
	}

	own.lose()
	for name, ws := range map[string]*wsconn.Conn{"attach": attach, "exec": exec} {
		c, _ := control(t, ws)
		if c.Type != "error" || c.Error.Code != "unavailable" || !strings.Contains(c.Error.Message, "ownership lost") {
			t.Fatalf("%s: %+v", name, c)
		}
		ev := waitAudit(t, r.cpRig, sb.ID, name, controlplane.OutcomeFailed)
		if ev.Detail["reason"] != "ownership lost" || ev.Code != "unavailable" {
			t.Fatalf("%s record %+v", name, ev)
		}
	}
}
