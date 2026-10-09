package container

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/forward"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

type recorded struct {
	mu   sync.Mutex
	list []map[string]any
}

func (r *recorded) record(name string, p map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p["event"] = name
	r.list = append(r.list, p)
	return nil
}

func (r *recorded) named(name string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, p := range r.list {
		if p["event"] == name {
			out = append(out, p)
		}
	}
	return out
}

func TestForwardAndSnapshotCapabilities(t *testing.T) {
	d, _ := testDriver(t, &fakeRunner{podman: true}, Options{}, nil)
	if !d.Info().Has(driver.CapForward) || d.Info().Has(driver.CapSnapshot) {
		t.Fatalf("%+v", d.Info())
	}
	d2, _ := testDriver(t, &fakeRunner{podman: true}, Options{SnapshotDir: t.TempDir()}, nil)
	if err := driver.Check(d2); err != nil || !d2.Info().Has(driver.CapSnapshot) {
		t.Fatalf("%+v %v", d2.Info(), err)
	}
}

// Under --network none a forward is an exec of the relay, as the sandbox's
// user: the container is never given a network for it. It is recorded and
// ends when the container stops.
func TestForwardUsesTheExecChannelUnderNetworkNone(t *testing.T) {
	rec := &recorded{}
	f := &fakeRunner{podman: true, script: "printf R; cat"}
	d, ws := testDriver(t, f, Options{Record: rec.record}, nil)
	ctx := context.Background()
	if _, err := d.Create(ctx, spec(ws)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined(f.runCall(t)), " --network none ") {
		t.Fatal("the test needs a container without a network")
	}
	rwc, err := driver.Forward(ctx, d, spec(ws).ID, 8000)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(rwc, "ping\n")
	if line, err := bufio.NewReader(rwc).ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("%q %v", line, err)
	}
	var relay []string
	for _, c := range f.find("exec") {
		if slices.Contains(c, forward.RelayLine(8000)) {
			relay = c
		}
	}
	got := joined(relay)
	for _, want := range []string{" exec -i --user ", " -w " + ws + " ", " abhed-sbx-" + spec(ws).ID + " /bin/sh -c "} {
		if !strings.Contains(got, want) {
			t.Fatalf("relay argv lacks %q: %s", want, got)
		}
	}
	for _, c := range f.calls {
		if c[0] == "network" || (c[0] == "run" && !slices.Contains(c, "none")) {
			t.Fatalf("a forward changed the container's network: %v", c)
		}
	}
	done := rwc.(*forward.Stream).Done()
	if _, err := d.Stop(ctx, spec(ws).ID, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the forward outlived the stop")
	}
	if fw := rec.named(driver.EventForward); len(fw) != 2 || fw[0]["action"] != "open" || fw[1]["action"] != "close" || fw[0]["sandbox_id"] != spec(ws).ID {
		t.Fatalf("records %+v", fw)
	}
	if _, err := driver.Forward(ctx, d, spec(ws).ID, 8000); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("forward while stopped: %v", err)
	}
	if _, err := d.Done(spec(ws).ID); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("done while stopped: %v", err)
	}
}

func TestForwardRefusedByTheRelay(t *testing.T) {
	f := &fakeRunner{podman: true, script: "echo 'connect: refused' >&2; exit 70"}
	d, ws := testDriver(t, f, Options{}, nil)
	ctx := context.Background()
	if _, err := d.Create(ctx, spec(ws)); err != nil {
		t.Fatal(err)
	}
	_, err := driver.Forward(ctx, d, spec(ws).ID, 9)
	if !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "nothing accepted") {
		t.Fatalf("%v", err)
	}
	f.script = "exit 71"
	if _, err := driver.Forward(ctx, d, spec(ws).ID, 9); err == nil || !strings.Contains(err.Error(), "neither bash nor nc") {
		t.Fatalf("%v", err)
	}
}

const imageHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A snapshot commits the container under a name built from ids only and
// archives the workspace; the record carries the image and the digest.
// A restore needs the container stopped, checks the image's id and the
// archive's digest before changing anything, and the next start runs the
// committed image. Deleting the sandbox removes both.
func TestContainerSnapshotRestore(t *testing.T) {
	rec := &recorded{}
	imageID := "sha256:" + imageHex
	f := &fakeRunner{podman: true}
	f.respond = func(args []string) ([]byte, error, bool) {
		switch {
		case args[0] == "commit":
			return []byte(imageHex + "\n"), nil, true
		case args[0] == "image" && args[1] == "inspect":
			return []byte(imageID + "\n"), nil, true
		case args[0] == "images":
			return []byte("localhost/abhed-sbx-snapshot:" + strings.ToLower(args[len(args)-1][len("label=abhed.sandbox="):]) + "-snap_0011223344556677\nother/image:latest\n"), nil, true
		}
		return nil, nil, false
	}
	snaps := t.TempDir()
	d, ws := testDriver(t, f, Options{Record: rec.record, SnapshotDir: snaps}, nil)
	ctx := context.Background()
	id := spec(ws).ID
	if _, err := d.Create(ctx, spec(ws)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "f"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := driver.Snapshot(ctx, d, id, "s1")
	if err != nil {
		t.Fatal(err)
	}
	img := "localhost/abhed-sbx-snapshot:" + id + "-" + ref.ID
	if c := f.find("commit"); len(c) != 1 || !slices.Equal(c[0], []string{"commit", "abhed-sbx-" + id, img}) {
		t.Fatalf("commit %v", c)
	}
	if ref.Image != imageID || ref.Digest == "" {
		t.Fatalf("ref %+v", ref)
	}
	if s := rec.named(driver.EventSnapshot); len(s) != 1 || s[0]["image"] != imageID || s[0]["digest"] != ref.Digest || s[0]["generation"] != int64(1) {
		t.Fatalf("record %+v", s)
	}

	if err := driver.Restore(ctx, d, id, ref); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("restore while running: %v", err)
	}
	if _, err := d.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(ws, "f"), []byte("v2"), 0o644)
	other := ref
	other.Image = "sha256:" + strings.Repeat("f", 64)
	if err := driver.Restore(ctx, d, id, other); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("image mismatch: %v", err)
	}
	noImage := ref
	noImage.Image = ""
	if err := driver.Restore(ctx, d, id, noImage); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("no image: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "f")); string(b) != "v2" {
		t.Fatalf("a refused restore changed the workspace: %q", b)
	}
	if err := driver.Restore(ctx, d, id, ref); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "f")); string(b) != "v1" {
		t.Fatalf("restored %q", b)
	}
	if _, err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	run := joined(f.runCall(t))
	if !strings.Contains(run, " "+img+" ") || !strings.Contains(run, " --label abhed.generation=2 ") {
		t.Fatalf("the start after a restore: %s", run)
	}
	if err := d.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	rmis := f.find("rmi")
	if len(rmis) != 1 || !strings.HasPrefix(rmis[0][1], "localhost/abhed-sbx-snapshot:"+id+"-snap_") {
		t.Fatalf("rmi %v", rmis)
	}
	if _, err := os.Stat(filepath.Join(snaps, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshots left: %v", err)
	}
}

// A snapshot whose commit fails leaves no archive behind.
func TestFailedCommitKeepsNothing(t *testing.T) {
	f := &fakeRunner{podman: true}
	f.respond = func(args []string) ([]byte, error, bool) {
		if args[0] == "commit" {
			return nil, &engineError{args: args, code: 125, stderr: "no space"}, true
		}
		return nil, nil, false
	}
	snaps := t.TempDir()
	d, ws := testDriver(t, f, Options{SnapshotDir: snaps}, func(*sandbox.Policy) {})
	ctx := context.Background()
	if _, err := d.Create(ctx, spec(ws)); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Snapshot(ctx, d, spec(ws).ID, ""); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(filepath.Join(snaps, spec(ws).ID))
	if len(left) != 0 {
		t.Fatalf("kept %v", left)
	}
}

func TestImageIDForms(t *testing.T) {
	for in, want := range map[string]string{
		"sha256:" + imageHex + "\n":           "sha256:" + imageHex,
		imageHex:                              "sha256:" + imageHex,
		"Getting image\n" + imageHex + "\n":   "sha256:" + imageHex,
		"sha256:" + strings.ToUpper(imageHex): "sha256:" + imageHex,
	} {
		if got, ok := imageID(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "abc", "sha256:xyz"} {
		if _, ok := imageID(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
