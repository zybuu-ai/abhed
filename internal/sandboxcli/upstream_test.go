package sandboxcli

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
	"github.com/zybuu-ai/abhed/profiles"
)

// With an upstream proxy in force, a REST call goes to the server through it
// and a stream tunnels through it with CONNECT; a proxy named by HTTP_PROXY
// sees neither, and the server's name is never dialled directly.
func TestCallsAndStreamsGoThroughTheUpstreamProxy(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	port := r.url[strings.LastIndex(r.url, ":")+1:]
	f, err := profiles.Load(r.profiles)
	if err != nil {
		t.Fatal(err)
	}
	server := "http://abhed.corp.test:" + port
	for _, p := range f.Profiles {
		p.URL = server
	}
	if err := profiles.Save(r.profiles, f); err != nil {
		t.Fatal(err)
	}
	other := upstreamtest.Start(t)
	t.Setenv("HTTP_PROXY", "http://"+other.Addr())
	corp := upstreamtest.Start(t)
	corp.Use(t, "svc", "pw")

	if res := r.must(t, "list"); !strings.Contains(res.out, sb.ID) {
		t.Fatalf("list: %+v", res)
	}
	if !corp.Saw(server + "/") {
		t.Fatal("the REST call did not go through the upstream proxy")
	}
	// -i: stdin over the WebSocket.
	if res := r.runWith(t, runOpts{in: strings.NewReader("x")}, "exec", "-i", sb.ID, "--", "cat"); res.code != 0 {
		t.Fatalf("exec -i: %+v", res)
	}
	if !corp.SawExactly("abhed.corp.test:" + port) {
		t.Fatal("the stream did not tunnel through the upstream proxy")
	}
	if other.Saw("abhed.corp.test") {
		t.Fatal("the environment's proxy was used")
	}
}
