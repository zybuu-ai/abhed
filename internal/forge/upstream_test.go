package forge

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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

// A remote on a port of its own is relayed on that port and no other.
func TestPushProxyKeepsTheRemotePort(t *testing.T) {
	if got := remoteAuthority("https://git.corp.test:8443/g/r.git"); got != "git.corp.test:8443" {
		t.Fatalf("authority %q", got)
	}
	corp := upstreamtest.Start(t)
	corp.Use(t, "svc", "pw")
	cfg, stop, err := pushProxy("https://git.corp.test:8443/g/r.git")
	defer stop()
	if err != nil || len(cfg) != 1 {
		t.Fatalf("relay config: %v %v", cfg, err)
	}
	pu, _ := url.Parse(cfg[0][1])
	pass, _ := pu.User.Password()
	connect := func(target string) int {
		t.Helper()
		conn, err := net.Dial("tcp", pu.Host)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		auth := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + pass))
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth)
		r, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	if code := connect("git.corp.test:443"); code != http.StatusForbidden || corp.Saw("git.corp.test") {
		t.Fatalf("the default port: %d", code)
	}
	if code := connect("git.corp.test:8443"); code == http.StatusForbidden || !corp.Saw("git.corp.test:8443") {
		t.Fatalf("the remote's port: %d", code)
	}
}
