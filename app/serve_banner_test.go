package app

import (
	"bufio"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

// serve -addr 127.0.0.1:0 names the port the system gave it, not :0, and
// the banner says git's files are held from commands.
func TestServeBannerNamesTheBoundPortAndGit(t *testing.T) {
	_, ws := trustWorkspace(t, `{}`)
	cmd := mainHelper([]string{"-C", ws, "serve", "-addr", "127.0.0.1:0"})
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }) // the helper this test started
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var banner []string
	deadline := time.After(60 * time.Second)
	for len(banner) == 0 || !strings.HasPrefix(strings.TrimSpace(banner[len(banner)-1]), "git") {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("serve ended:\n%s", strings.Join(banner, "\n"))
			}
			banner = append(banner, l)
		case <-deadline:
			t.Fatalf("no banner:\n%s", strings.Join(banner, "\n"))
		}
	}
	all := strings.Join(banner, "\n")
	i := strings.Index(all, "http://")
	if i < 0 {
		t.Fatalf("no URL:\n%s", all)
	}
	u, err := url.Parse(strings.Fields(all[i:])[0])
	if err != nil || u.Port() == "" || u.Port() == "0" {
		t.Fatalf("the banner names port %q:\n%s", u.Port(), all)
	}
	if !strings.Contains(all, "git       held read-only from commands") {
		t.Fatalf("no git line:\n%s", all)
	}
	// And the port it names is the one it serves on.
	var conn net.Conn
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if conn, err = net.Dial("tcp", u.Host); err == nil {
			_ = conn.Close()
			return
		}
	}
	t.Fatalf("nothing answers at %s: %v", u.Host, err)
}
