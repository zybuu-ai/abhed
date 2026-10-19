package config

import (
	"strings"
	"testing"
)

// sandbox.image comes from the operator's files: a trusted workspace's is set
// aside, and a value that would read as an engine flag is refused.
func TestSandboxImageIsOperatorOnly(t *testing.T) {
	_, ws := trustHome(t, `{"sandbox":{"image":"docker.io/library/golang:1.26-bookworm"}}`,
		`{"sandbox":{"image":"ghcr.io/evil/image:1"},"statusline":{"command":"./s.sh"}}`)
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Workspace.Trusted || cfg.Statusline.Command != "./s.sh" {
		t.Fatalf("trust did not apply: %+v", cfg.Workspace)
	}
	if cfg.Sandbox.Image != "docker.io/library/golang:1.26-bookworm" {
		t.Fatalf("image %q", cfg.Sandbox.Image)
	}
	aside := false
	for _, k := range cfg.SetAside {
		aside = aside || k.Key == "sandbox.image"
	}
	if !aside {
		t.Fatalf("not reported as set aside: %+v", cfg.SetAside)
	}

	for _, bad := range []string{"--privileged", "img name", "img\n"} {
		c := Default()
		c.Sandbox.Image = bad
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "sandbox.image") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
