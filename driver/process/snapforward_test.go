//go:build !windows

package process

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/forward"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

type events struct {
	mu   sync.Mutex
	list []map[string]any
	fail error
}

func (e *events) record(name string, p map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail != nil {
		return e.fail
	}
	p["event"] = name
	e.list = append(e.list, p)
	return nil
}

func (e *events) named(name string) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for _, p := range e.list {
		if p["event"] == name {
			out = append(out, p)
		}
	}
	return out
}

func withOptions(t *testing.T) (*Driver, *events, string) {
	t.Helper()
	var closed atomic.Int32
	ev := &events{}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newDriverWith(func(driver.Spec) (sandbox.Sandbox, error) {
		return fakeSandbox{tier: sandbox.TierProcess, closed: &closed}, nil
	}, Options{SnapshotDir: filepath.Join(base, "snaps"), Record: ev.record})
	if err := driver.Check(d); err != nil {
		t.Fatal(err)
	}
	return d, ev, ws
}

// Snapshot is declared only with a snapshot folder; forward always.
func TestCapabilitiesFollowOptions(t *testing.T) {
	d, _, _, _ := fake(t, sandbox.TierProcess)
	if d.Info().Has(driver.CapSnapshot) || !d.Info().Has(driver.CapForward) {
		t.Fatalf("without a folder: %+v", d.Info())
	}
	if _, err := driver.Snapshot(context.Background(), d, "a", ""); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatal(err)
	}
	d2, _, _ := withOptions(t)
	if !d2.Info().Has(driver.CapSnapshot) {
		t.Fatalf("with a folder: %+v", d2.Info())
	}
}

// A snapshot is recorded with the sandbox's id, generation and digest; a
// restore needs the instance stopped, puts the workspace back, is
// recorded, and the start after it is the next generation.
func TestSnapshotRestoreLifecycle(t *testing.T) {
	ctx := context.Background()
	d, ev, ws := withOptions(t)
	if _, err := d.Create(ctx, driver.Spec{ID: "sbx_a", Generation: 2, Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "f"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := driver.Snapshot(ctx, d, "sbx_a", "v1")
	if err != nil {
		t.Fatal(err)
	}
	snaps := ev.named(driver.EventSnapshot)
	if len(snaps) != 1 || snaps[0]["sandbox_id"] != "sbx_a" || snaps[0]["generation"] != int64(2) || snaps[0]["digest"] != ref.Digest || ref.Digest == "" {
		t.Fatalf("snapshot record %+v ref %+v", snaps, ref)
	}
	if err := os.WriteFile(filepath.Join(ws, "f"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := driver.Restore(ctx, d, "sbx_a", ref); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("restore while running: %v", err)
	}
	if _, err := d.Stop(ctx, "sbx_a", time.Second); err != nil {
		t.Fatal(err)
	}
	bad := ref
	bad.Digest = "sha256:" + strings.Repeat("0", 64)
	if err := driver.Restore(ctx, d, "sbx_a", bad); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("digest mismatch: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "f")); string(b) != "v2" {
		t.Fatalf("a refused restore changed the workspace: %q", b)
	}
	if err := driver.Restore(ctx, d, "sbx_a", ref); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "f")); string(b) != "v1" {
		t.Fatalf("restored %q", b)
	}
	rs := ev.named(driver.EventRestore)
	if len(rs) != 2 || rs[0]["outcome"] != "refused" || rs[1]["outcome"] != "restored" || rs[1]["digest"] != ref.Digest {
		t.Fatalf("restore records %+v", rs)
	}
	if _, err := d.Start(ctx, "sbx_a"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	gen := d.insts["sbx_a"].spec.Generation
	d.mu.Unlock()
	if gen != 3 {
		t.Fatalf("generation after restart %d, want 3", gen)
	}
	if err := d.Delete(ctx, "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.opts.SnapshotDir, "sbx_a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleting the sandbox left its snapshots: %v", err)
	}
}

// A snapshot the record refuses is not kept.
func TestUnrecordedSnapshotIsNotKept(t *testing.T) {
	ctx := context.Background()
	d, ev, ws := withOptions(t)
	if _, err := d.Create(ctx, driver.Spec{ID: "sbx_a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	ev.fail = errors.New("record down")
	if _, err := driver.Snapshot(ctx, d, "sbx_a", ""); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(filepath.Join(d.opts.SnapshotDir, "sbx_a"))
	if len(left) != 0 {
		t.Fatalf("kept %v", left)
	}
}

// echoServer answers each line with "echo:" and the line.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					_, _ = io.WriteString(c, "echo:"+sc.Text()+"\n")
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// A forward reaches the port through a relay run as a sandboxed command,
// is recorded, and ends when the instance stops.
func TestForwardThroughTheSandbox(t *testing.T) {
	ctx := context.Background()
	d, ev, ws := withOptions(t)
	port := echoServer(t)
	if _, err := d.Create(ctx, driver.Spec{ID: "sbx_a", Generation: 1, Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	rwc, err := driver.Forward(ctx, d, "sbx_a", port)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(rwc, "hi\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(rwc).ReadString('\n')
	if err != nil || line != "echo:hi\n" {
		t.Fatalf("%q %v", line, err)
	}
	if opens := ev.named(driver.EventForward); len(opens) != 1 || opens[0]["action"] != "open" || opens[0]["port"] != port {
		t.Fatalf("forward records %+v", opens)
	}
	done := rwc.(*forward.Stream).Done()
	if _, err := d.Stop(ctx, "sbx_a", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the forward outlived the stop")
	}
	closes := ev.named(driver.EventForward)
	if len(closes) != 2 || closes[1]["action"] != "close" {
		t.Fatalf("forward records %+v", closes)
	}
	if _, err := driver.Forward(ctx, d, "sbx_a", port); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("forward while stopped: %v", err)
	}
}

// Nothing listening is a refusal, not a stream that ends at once.
func TestForwardToAClosedPortFails(t *testing.T) {
	ctx := context.Background()
	d, _, ws := withOptions(t)
	if _, err := d.Create(ctx, driver.Spec{ID: "sbx_a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if _, err := driver.Forward(ctx, d, "sbx_a", port); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("closed port: %v", err)
	}
	d.mu.Lock()
	n := d.insts["sbx_a"].forwards.Len()
	d.mu.Unlock()
	if n != 0 {
		t.Fatal("a failed forward was kept")
	}
	if _, err := d.Stop(ctx, "sbx_a", time.Second); err != nil {
		t.Fatal(err)
	}
}

// A host listener forwards connections, binds loopback by default, refuses
// a wider address unless asked, and closes with the sandbox.
func TestListenerClosesWithTheSandbox(t *testing.T) {
	ctx := context.Background()
	d, ev, ws := withOptions(t)
	port := echoServer(t)
	if _, err := d.Create(ctx, driver.Spec{ID: "sbx_a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	if _, err := forward.Listen(ctx, d, "sbx_a", port, forward.ListenOptions{Addr: "0.0.0.0:0", Record: ev.record}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a wide address without asking: %v", err)
	}
	l, err := forward.Listen(ctx, d, "sbx_a", port, forward.ListenOptions{Record: ev.record})
	if err != nil {
		t.Fatal(err)
	}
	if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Fatalf("listening on %v", ip)
	}
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "x\n")
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || line != "echo:x\n" {
		t.Fatalf("%q %v", line, err)
	}
	if _, err := d.Stop(ctx, "sbx_a", time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the listener outlived the sandbox")
	}
	if _, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		t.Fatal("still listening after the stop")
	}
	ls := ev.named(driver.EventForwardListen)
	if len(ls) != 2 || ls[0]["action"] != "listen" || ls[1]["action"] != "close" || ls[0]["wide"] != false {
		t.Fatalf("listener records %+v", ls)
	}
	_ = c.Close()

	// Binding wider is allowed when asked, and the record says so.
	if _, err := d.Start(ctx, "sbx_a"); err != nil {
		t.Fatal(err)
	}
	wide, err := forward.Listen(ctx, d, "sbx_a", port, forward.ListenOptions{Addr: "0.0.0.0:0", Wide: true, Record: ev.record})
	if err != nil {
		t.Fatal(err)
	}
	_ = wide.Close()
	<-wide.Done()
	ls = ev.named(driver.EventForwardListen)
	if ls[2]["wide"] != true {
		t.Fatalf("wide not recorded: %+v", ls[2])
	}
	if err := d.Delete(ctx, "sbx_a"); err != nil {
		t.Fatal(err)
	}
}
