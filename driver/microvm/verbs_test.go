//go:build !windows

package microvm

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

func tarOf(t *testing.T, name, body string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	return &b
}

// A copy in lands in the guest and comes back to the host workspace; a copy
// out is the folder under its own name; paths out of the workspace or into
// its state are refused.
func TestCopyInAndOut(t *testing.T) {
	ws, _ := sandboxPair(t)
	if err := os.Mkdir(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, be := newFake(t)
	if !d.Info().Has(driver.CapCopy) || !d.Info().Has(driver.CapForward) || d.Info().Has(driver.CapSnapshot) {
		t.Fatalf("capabilities %v", d.Info().Capabilities)
	}
	ctx := context.Background()
	if _, err := d.Create(ctx, driver.Spec{ID: id, Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	if err := d.CopyIn(ctx, id, "src", tarOf(t, "in.txt", "hello")); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(be.last().dir, "src", "in.txt")) != "hello" || read(t, filepath.Join(ws, "src", "in.txt")) != "hello" {
		t.Fatal("the copy did not reach the guest and the host")
	}
	// Made in the guest, read out after it is copied back.
	if _, _, err := execIn(t, d, driver.ExecRequest{Command: "echo made > src/out.txt"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := d.CopyOut(ctx, id, filepath.Join(ws, "src"), &out); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		names[h.Name] = string(b)
	}
	if names["src/out.txt"] != "made\n" || names["src/in.txt"] != "hello" {
		t.Fatalf("copied out %v", names)
	}
	for _, p := range []string{"../outside", "/etc", ".abhed", "src/.abhed/x"} {
		if err := d.CopyIn(ctx, id, p, tarOf(t, "x", "x")); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("copy in to %s: %v", p, err)
		}
		if err := d.CopyOut(ctx, id, p, io.Discard); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("copy out of %s: %v", p, err)
		}
	}
	if err := d.CopyIn(ctx, id, "src", tarOf(t, "../escape", "x")); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an entry out of the folder: %v", err)
	}
}

// A forward reaches a port on the guest's loopback and ends with the instance.
func TestForwardReachesTheGuestLoopback(t *testing.T) {
	d, _ := newFake(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, driverSpec()); err != nil {
		t.Fatal(err)
	}
	// The fake guest runs on this host, so its loopback is this one's.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	st, err := d.Forward(ctx, id, ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(st, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo %q %v", got, err)
	}
	stopped := make(chan error, 1)
	go func() { _, err := d.Stop(ctx, id, time.Second); stopped <- err }()
	if _, err := io.ReadAll(st); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Logf("read after stop: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if _, err := d.Forward(ctx, id, 1); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("a forward to a stopped instance: %v", err)
	}
}

// A snapshot holds the workspace as the guest left it; a restore needs the
// instance stopped and the next start boots with it.
func TestSnapshotAndRestore(t *testing.T) {
	ws, _ := sandboxPair(t)
	be := &fakeBackend{}
	o := testOptions(t)
	o.SnapshotDir = filepath.Join(shortTemp(t), "snaps")
	var events []string
	o.Record = func(ev string, _ map[string]any) error { events = append(events, ev); return nil }
	d := newDriver(o, be)
	d.image = copyImage
	if !d.Info().Has(driver.CapSnapshot) {
		t.Fatal("no snapshot capability with a snapshot_dir")
	}
	ctx := context.Background()
	if _, err := d.Create(ctx, driver.Spec{ID: id, Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execIn(t, d, driver.ExecRequest{Command: "echo one > f"}); err != nil {
		t.Fatal(err)
	}
	ref, err := d.Snapshot(ctx, id, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := execIn(t, d, driver.ExecRequest{Command: "echo two > f"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(ctx, id, ref); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("a restore while running: %v", err)
	}
	if _, err := d.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(ctx, id, ref); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(ws, "f")) != "one\n" {
		t.Fatal("the host workspace was not restored")
	}
	if _, err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, out, err := execIn(t, d, driver.ExecRequest{Command: "cat f"}); err != nil || out != "one\n" {
		t.Fatalf("the restarted guest has %q, %v", out, err)
	}
	if len(events) != 2 || events[0] != driver.EventSnapshot || events[1] != driver.EventRestore {
		t.Fatalf("events %v", events)
	}
}
