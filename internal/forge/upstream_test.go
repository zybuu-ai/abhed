package forge

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
)

// A push goes through the upstream proxy by a relay for the remote's host
// alone, and directly when no_proxy names the remote.
func TestPushProxy(t *testing.T) {
	if cfg, stop, err := pushProxy("https://gitlab.corp.test/g/r.git"); err != nil || cfg != nil {
		t.Fatalf("no proxy: %v %v", cfg, err)
	} else {
		stop()
	}
	corp := upstreamtest.Start(t)
	up := corp.Use(t, "svc", "pw")
	up.NoProxy = []string{".corp.test"}
	cfg, stop, err := pushProxy("https://gitlab.corp.test/g/r.git")
	stop()
	if err != nil || cfg != nil {
		t.Fatalf("a no_proxy remote went through the relay: %v %v", cfg, err)
	}
	cfg, stop, err = pushProxy("https://github.com/o/r.git")
	defer stop()
	if err != nil || len(cfg) != 1 || cfg[0][0] != "http.proxy" || !strings.HasPrefix(cfg[0][1], "http://abhed:") || strings.Contains(cfg[0][1], "pw") {
		t.Fatalf("relay config: %v %v", cfg, err)
	}
}
