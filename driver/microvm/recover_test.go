//go:build !windows

package microvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type cgLog struct {
	mu     sync.Mutex
	killed []string
}

func (c *cgLog) kill(_, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.killed = append(c.killed, id)
	return nil
}

func TestRecoverCleansOnlyWhatIsOurs(t *testing.T) {
	o := fcOptions(t)
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	fc := d.be.(*firecracker)
	cg := &cgLog{}
	fc.cgKill, fc.cgRemove = cg.kill, func(string, string) error { return nil }
	base := filepath.Join(o.Firecracker.ChrootBase, "firecracker")
	jail := func(name string, m *marker) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Join(p, "root"), 0o700); err != nil {
			t.Fatal(err)
		}
		if m != nil {
			b, _ := json.Marshal(m)
			if err := os.WriteFile(filepath.Join(p, markerName), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	ours := jail("sbx-1", &marker{StateDir: o.StateDir, ID: "sbx_1"})
	other := jail("sbx-2", &marker{StateDir: "/elsewhere/state", ID: "sbx_2"})
	bare := jail("sbx-3", nil)
	wrongID := jail("sbx-4", &marker{StateDir: o.StateDir, ID: "sbx_5"})
	// A marker that is a link to a file someone else wrote is not ours.
	linked := jail("sbx-6", nil)
	planted := filepath.Join(t.TempDir(), "m.json")
	b, _ := json.Marshal(marker{StateDir: o.StateDir, ID: "sbx_6"})
	_ = os.WriteFile(planted, b, 0o600)
	_ = os.Symlink(planted, filepath.Join(linked, markerName))
	// A link where a jail would be is never followed.
	target := t.TempDir()
	_ = os.WriteFile(filepath.Join(target, markerName), b, 0o600)
	_ = os.Symlink(target, filepath.Join(base, "sbx-7"))

	// Instance state a killed driver left, and things that are not that.
	for _, n := range []string{"sbx_1", "sbx_9"} {
		if err := os.MkdirAll(filepath.Join(o.StateDir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(o.StateDir, "sbx_8"), nil, 0o600)
	_ = os.MkdirAll(filepath.Join(o.StateDir, "not-ours"), 0o700)

	cleaned, err := d.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close(context.Background()) }()
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Fatal("our jail was left")
	}
	for _, p := range []string{other, bare, wrongID, linked, target, filepath.Join(target, markerName), planted} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was touched: %v", p, err)
		}
	}
	if !slices.Equal(cg.killed, []string{"sbx-1"}) {
		t.Fatalf("killed cgroups %v", cg.killed)
	}
	for _, n := range []string{"sbx_1", "sbx_9"} {
		if _, err := os.Stat(filepath.Join(o.StateDir, n)); !os.IsNotExist(err) {
			t.Fatalf("stale state %s was left", n)
		}
	}
	for _, n := range []string{"sbx_8", "not-ours", ".lock"} {
		if _, err := os.Lstat(filepath.Join(o.StateDir, n)); err != nil {
			t.Fatalf("%s was removed", n)
		}
	}
	if !strings.Contains(strings.Join(cleaned, ","), "VM sbx_1") {
		t.Fatalf("cleaned %q", cleaned)
	}
}

func TestOneDriverPerStateFolder(t *testing.T) {
	o := fcOptions(t)
	d1, _ := New(o)
	d2, _ := New(o)
	for _, d := range []*Driver{d1, d2} {
		fc := d.be.(*firecracker)
		fc.cgKill, fc.cgRemove = func(string, string) error { return nil }, func(string, string) error { return nil }
	}
	if _, err := d1.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Recover(context.Background()); err == nil || !strings.Contains(err.Error(), "another microvm driver") {
		t.Fatalf("a second driver took the same state folder: %v", err)
	}
	if err := d1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Recover(context.Background()); err != nil {
		t.Fatalf("the lock outlived its driver: %v", err)
	}
	_ = d2.Close(context.Background())
}

// A helper's VM ends with the driver that started it, killed or not.
func TestHelperEndsWithItsDriver(t *testing.T) {
	o := testOptions(t)
	o.Helper = &Helper{Path: fakeVMM(t, "helper")}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	h := d.be.(*helper)
	h.env, h.probe = []string{"ABHED_FAKE_VMM=helper"}, func() error { return nil }
	if _, err := d.Create(context.Background(), driverSpec()); err != nil {
		t.Fatal(err)
	}
	m := d.insts[id].m.(*procMachine)
	// What the driver's death does: its end of the helper's stdin closes.
	if err := m.reap(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.done():
	case <-time.After(5 * time.Second):
		t.Fatal("the helper outlived its driver's end of stdin")
	}
	_ = d.Delete(context.Background(), id)
}
