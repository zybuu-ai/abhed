package driver_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

// minimal implements only the required methods.
type minimal struct{ caps []driver.Capability }

func (m minimal) Info() driver.Info {
	return driver.Info{Name: "minimal", Version: "1", Capabilities: m.caps}
}
func (minimal) Create(context.Context, driver.Spec) (driver.Status, error) {
	return driver.Status{}, nil
}
func (minimal) Start(context.Context, string) (driver.Status, error) { return driver.Status{}, nil }
func (minimal) Stop(context.Context, string, time.Duration) (driver.Status, error) {
	return driver.Status{}, nil
}
func (minimal) Delete(context.Context, string) error                  { return nil }
func (minimal) Status(context.Context, string) (driver.Status, error) { return driver.Status{}, nil }
func (minimal) Exec(context.Context, string, driver.ExecRequest) (driver.ExecResult, error) {
	return driver.ExecResult{}, nil
}

func TestCheckRefusesUnimplementedCapability(t *testing.T) {
	for _, c := range []driver.Capability{driver.CapAttach, driver.CapCopy, driver.CapForward, driver.CapSnapshot} {
		err := driver.Check(minimal{caps: []driver.Capability{c}})
		if !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("declaring %s without implementing it: got %v, want ErrInvalid", c, err)
		}
	}
	if err := driver.Check(minimal{}); err != nil {
		t.Fatalf("a driver declaring nothing: %v", err)
	}
}

func TestCheckRefusesUnknownAndDuplicateCapability(t *testing.T) {
	f := drivertest.New("fake", "teleport")
	if err := driver.Check(f); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("unknown capability: %v", err)
	}
	f = drivertest.New("fake", driver.CapCopy, driver.CapCopy)
	if err := driver.Check(f); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("duplicate capability: %v", err)
	}
	if err := driver.Check(drivertest.New("Bad Name")); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
}

// An implemented but undeclared capability is still refused: the
// declaration, not the method set, is what a caller relies on.
func TestHelpersRefuseUndeclaredCapability(t *testing.T) {
	ctx := context.Background()
	f := drivertest.New("fake")
	if _, err := f.Create(ctx, driver.Spec{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	checks := map[string]error{}
	_, checks["attach"] = driver.Attach(ctx, f, "a", driver.AttachRequest{})
	checks["copy_in"] = driver.CopyIn(ctx, f, "a", "/x", strings.NewReader(""))
	checks["copy_out"] = driver.CopyOut(ctx, f, "a", "/x", &bytes.Buffer{})
	_, checks["forward"] = driver.Forward(ctx, f, "a", 80)
	_, checks["snapshot"] = driver.Snapshot(ctx, f, "a", "s")
	checks["restore"] = driver.Restore(ctx, f, "a", driver.SnapshotRef{})
	_, checks["minimal attach"] = driver.Attach(ctx, minimal{}, "a", driver.AttachRequest{})
	for name, err := range checks {
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s: got %v, want ErrUnsupported", name, err)
		}
	}
}

func TestHelpersCallDeclaredCapability(t *testing.T) {
	ctx := context.Background()
	f := drivertest.New("fake", driver.CapCopy, driver.CapSnapshot, driver.CapForward)
	if _, err := f.Create(ctx, driver.Spec{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := driver.CopyIn(ctx, f, "a", "/w", strings.NewReader("tarbytes")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := driver.CopyOut(ctx, f, "a", "/w", &out); err != nil || out.String() != "tarbytes" {
		t.Fatalf("copy out: %q %v", out.String(), err)
	}
	ref, err := driver.Snapshot(ctx, f, "a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Restore(ctx, f, "a", ref); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Forward(ctx, f, "a", 0); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("port 0: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	r := driver.NewRegistry()
	if err := r.Register(drivertest.New("b")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(drivertest.New("a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(drivertest.New("a")); !errors.Is(err, driver.ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := r.Register(minimal{caps: []driver.Capability{driver.CapAttach}}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("dishonest driver registered: %v", err)
	}
	if err := r.Register(nil); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("nil: %v", err)
	}
	if got := fmt.Sprint(r.Names()); got != "[a b]" {
		t.Fatalf("names %s", got)
	}
	if _, err := r.Get("c"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if d, err := r.Get("a"); err != nil || d.Info().Name != "a" {
		t.Fatalf("get: %v", err)
	}
}

func TestCodes(t *testing.T) {
	for _, k := range []error{driver.ErrUnsupported, driver.ErrNotFound, driver.ErrExists, driver.ErrConflict,
		driver.ErrInvalid, driver.ErrUnavailable, driver.ErrVersion, driver.ErrInternal} {
		err := fmt.Errorf("wrapped: %w", driver.Errorf(k, "x"))
		if got := driver.KindOf(driver.Code(err)); got != k {
			t.Errorf("%v round-trips to %v", k, got)
		}
	}
	if driver.Code(errors.New("plain")) != "internal" {
		t.Error("a plain error must be internal")
	}
	if driver.KindOf("made-up") != driver.ErrInternal {
		t.Error("an unknown code must be internal")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"process", "docker", "micro-vm", "k8s"} {
		if !driver.ValidName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "Docker", "1x", "a_b", "a.b", strings.Repeat("a", 33), "a/b"} {
		if driver.ValidName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExecRequestValidate(t *testing.T) {
	if err := (driver.ExecRequest{}).Validate(); !errors.Is(err, driver.ErrInvalid) {
		t.Error("empty request accepted")
	}
	if err := (driver.ExecRequest{Argv: []string{"x"}, Command: "x"}).Validate(); !errors.Is(err, driver.ErrInvalid) {
		t.Error("both argv and command accepted")
	}
	if err := (driver.ExecRequest{Command: "x"}).Validate(); err != nil {
		t.Error(err)
	}
}
