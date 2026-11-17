package sandbox

import (
	"strings"
	"testing"
)

// A bubblewrap a container image supplied is named wherever the tier is: the
// caveat a doctor and Studio show, and the description of the tier.
func TestImageBwrapCaveat(t *testing.T) {
	p := &Process{backend: "bwrap"}
	t.Setenv(ImageBwrapEnv, "1")
	if c := p.Caveat(); !strings.Contains(c, "image-supplied bubblewrap") {
		t.Fatalf("caveat %q", c)
	}
	t.Setenv(ImageBwrapEnv, "")
	if c := p.Caveat(); strings.Contains(c, "image-supplied") {
		t.Fatalf("caveat without the variable %q", c)
	}
	if c := (&Process{backend: "sandbox-exec"}).Caveat(); c != "" {
		t.Fatalf("sandbox-exec caveat %q", c)
	}
}
