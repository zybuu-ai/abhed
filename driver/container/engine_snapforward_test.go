package container

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/forward"
)

// echoInside starts a line echo server on the container's loopback, with
// the perl the test image carries, and waits until it answers.
func echoInside(t *testing.T, d *Driver, id string, port string) {
	t.Helper()
	server := `nohup perl -MIO::Socket::INET -e '$s=IO::Socket::INET->new(LocalAddr=>"127.0.0.1",LocalPort=>` + port +
		`,Listen=>5,ReuseAddr=>1) or die; $SIG{CHLD}="IGNORE"; while($c=$s->accept){if(!fork){while(<$c>){print $c "echo:$_"} exit} close $c}' >/dev/null 2>&1 &`
	if out, code, err := run(t, d, id, server); err != nil || code != 0 {
		t.Fatalf("starting the server inside: %d %v %s", code, err, out)
	}
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rwc, err := d.Forward(ctx, id, atoi(port))
		cancel()
		if err == nil {
			_ = rwc.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the server inside never answered")
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// Forwarding into a container with no network: the bytes go through the
// engine's exec channel, the container still has loopback only, a host
// listener binds loopback, and both close when the container stops.
func TestForwardUnderNetworkNoneOnARealEngine(t *testing.T) {
	rec := &recorded{}
	d := realDriver(t, Options{Record: rec.record})
	ws := realWorkspace(t)
	create(t, d, driver.Spec{ID: "sbx_engine04", Generation: 1, Workspace: ws})
	echoInside(t, d, "sbx_engine04", "8765")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rwc, err := d.Forward(ctx, "sbx_engine04", 8765)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(rwc, "through-exec\n")
	if line, err := bufio.NewReader(rwc).ReadString('\n'); err != nil || line != "echo:through-exec\n" {
		t.Fatalf("%q %v", line, err)
	}

	l, err := forward.Listen(ctx, d, "sbx_engine04", 8765, forward.ListenOptions{Record: rec.record})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Addr().(*net.TCPAddr).IP.IsLoopback() {
		t.Fatalf("listening on %v", l.Addr())
	}
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(time.Minute))
	_, _ = io.WriteString(c, "via-host-port\n")
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || line != "echo:via-host-port\n" {
		t.Fatalf("%q %v", line, err)
	}

	if out, _, err := run(t, d, "sbx_engine04", "cat /proc/net/dev"); err != nil || strings.Contains(out, "eth0") {
		t.Fatalf("the container gained a network: %v\n%s", err, out)
	}
	if _, err := d.Forward(ctx, "sbx_engine04", 8766); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("a port nothing listens on: %v", err)
	}

	done := rwc.(*forward.Stream).Done()
	if _, err := d.Stop(context.Background(), "sbx_engine04", time.Second); err != nil {
		t.Fatal(err)
	}
	for what, ch := range map[string]<-chan struct{}{"the stream": done, "the listener": l.Done()} {
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s outlived the stop", what)
		}
	}
	_ = c.Close()
	if n := len(rec.named(driver.EventForward)); n < 4 {
		t.Fatalf("forward records: %d", n)
	}
}

// A snapshot commits the container and archives the workspace; a restore
// puts the workspace back and the next start runs the committed image; a
// tampered archive is refused; deleting the sandbox removes the image.
func TestSnapshotRestoreOnARealEngine(t *testing.T) {
	rec := &recorded{}
	snaps := realWorkspace(t)
	d := realDriver(t, Options{Record: rec.record, SnapshotDir: snaps})
	ws := realWorkspace(t)
	create(t, d, driver.Spec{ID: "sbx_engine05", Generation: 1, Workspace: ws})
	if out, code, err := run(t, d, "sbx_engine05", "echo v1 > f.txt && mkdir -p sub && echo deep > sub/g.txt"); err != nil || code != 0 {
		t.Fatalf("%d %v %s", code, err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ref, err := d.Snapshot(ctx, "sbx_engine05", "first")
	if err != nil {
		t.Fatal(err)
	}
	img, _ := d.snapshotImage("sbx_engine05", ref.ID)
	t.Cleanup(func() { d.removeImage(img) })
	if !strings.HasPrefix(ref.Image, "sha256:") || !strings.HasPrefix(ref.Digest, "sha256:") {
		t.Fatalf("ref %+v", ref)
	}
	if got, err := d.inspectImage(ctx, img); err != nil || got != ref.Image {
		t.Fatalf("the committed image: %q %v", got, err)
	}

	if out, code, err := run(t, d, "sbx_engine05", "echo v2 > f.txt && rm -r sub && echo new > new.txt"); err != nil || code != 0 {
		t.Fatalf("%d %v %s", code, err, out)
	}
	if _, err := d.Stop(ctx, "sbx_engine05", time.Second); err != nil {
		t.Fatal(err)
	}
	// The archive on disk changed under the ref: refused, nothing touched.
	file := filepath.Join(snaps, "sbx_engine05", ref.ID+".tar.gz")
	good, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(append([]byte{}, good...), 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(ctx, "sbx_engine05", ref); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a tampered snapshot: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "f.txt")); strings.TrimSpace(string(b)) != "v2" {
		t.Fatalf("a refused restore changed the workspace: %q", b)
	}
	if err := os.WriteFile(file, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(ctx, "sbx_engine05", ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new.txt on the host after the restore: %v", err)
	}
	if _, err := d.Start(ctx, "sbx_engine05"); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, d, "sbx_engine05", "cat f.txt sub/g.txt")
	if err != nil || out != "v1\ndeep\n" {
		t.Fatalf("after the restore: %v\n%s", err, out)
	}
	// An engine in a VM sees the host's folder through a share that may
	// keep a removed entry for a moment.
	gone := false
	for i := 0; i < 20 && !gone; i++ {
		_, code, err := run(t, d, "sbx_engine05", "test ! -e new.txt")
		gone = err == nil && code == 0
		if !gone {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !gone {
		t.Fatal("a file made after the snapshot is still there after the restore")
	}
	if got, _ := d.r.output(ctx, []string{"container", "inspect", "--format", "{{.ImageName}}", d.opts.prefix() + "sbx_engine05"}, nil); !strings.Contains(string(got), "snapshot") {
		t.Logf("the restarted container's image: %s", got)
	}
	if rs := rec.named(driver.EventRestore); len(rs) != 2 || rs[0]["outcome"] != "refused" || rs[1]["outcome"] != "restored" {
		t.Fatalf("restore records %+v", rs)
	}
	if err := d.Delete(context.Background(), "sbx_engine05"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.inspectImage(context.Background(), img); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the snapshot image outlived the sandbox: %v", err)
	}
}
