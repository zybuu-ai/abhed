//go:build !linux

package probe

import (
	"context"
	"strings"
	"testing"
)

func TestRunIsNotSupportedOffLinux(t *testing.T) {
	r := Run(context.Background(), Requirements{})
	if r.Qualified || !strings.Contains(r.Summary, "not supported on this OS") {
		t.Fatalf("got %+v", r)
	}
}
