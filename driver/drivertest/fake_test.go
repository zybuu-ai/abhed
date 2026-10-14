package drivertest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := drivertest.New("fake", driver.CapAttach, driver.CapCopy, driver.CapForward, driver.CapSnapshot)
	if err := driver.Check(f); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(ctx, driver.Spec{}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.Create(ctx, driver.Spec{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := f.Exec(ctx, "a", driver.ExecRequest{Argv: []string{"echo", "x"}, Stdin: strings.NewReader("in"), Stdout: &out}); err != nil || out.String() != "echo x\nin" {
		t.Fatalf("%q %v", out.String(), err)
	}
	resize := make(chan driver.WinSize, 1)
	resize <- driver.WinSize{Rows: 1, Cols: 2}
	close(resize)
	out.Reset()
	if _, err := f.Attach(ctx, "a", driver.AttachRequest{Stdin: strings.NewReader("hi"), Stdout: &out, Resize: resize}); err != nil || out.String() != "hi" {
		t.Fatalf("%q %v", out.String(), err)
	}
	if len(f.Resizes) != 1 {
		t.Fatalf("resizes %v", f.Resizes)
	}
	rw, err := f.Forward(ctx, "a", 1)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = rw.Write([]byte("ping")) }()
	b := make([]byte, 4)
	if _, err := io.ReadFull(rw, b); err != nil || string(b) != "ping" {
		t.Fatalf("%q %v", b, err)
	}
	_ = rw.Close()
	if err := f.CopyOut(ctx, "a", "/none", &out); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	if err := f.Restore(ctx, "a", driver.SnapshotRef{ID: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.Stop(ctx, "a", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Exec(ctx, "a", driver.ExecRequest{Command: "x"}); !errors.Is(err, driver.ErrConflict) {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{f.Delete(ctx, "a"), f.Restore(ctx, "a", driver.SnapshotRef{})} {
		if !errors.Is(err, driver.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if _, err := f.Start(ctx, "a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.Snapshot(ctx, "a", "s"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
}
