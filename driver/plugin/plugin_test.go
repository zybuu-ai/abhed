package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

// sockDir is short enough for a Unix socket path on every platform.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "abp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func serve(t *testing.T, d driver.Driver) string {
	t.Helper()
	path := filepath.Join(sockDir(t), "d.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, d) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return path
}

// rawServer answers each connection with handle, for tests of what the
// client does with a driver that misbehaves.
func rawServer(t *testing.T, handle func(r *bufio.Scanner, w net.Conn)) string {
	t.Helper()
	path := filepath.Join(sockDir(t), "raw.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(nil, 4*MaxLine)
				handle(sc, c)
			}()
		}
	}()
	return path
}

const exit0 = `{"type":"result","result":{"exit_code":0}}`

const helloOK = `{"type":"hello","version":1,"info":{"name":"raw","version":"1","capabilities":["copy"]}}`

func line(w io.Writer, s string) { _, _ = io.WriteString(w, s+"\n") }

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := drivertest.New("fake", driver.CapAttach, driver.CapCopy, driver.CapForward, driver.CapSnapshot)
	cl, err := Dial(ctx, "fake", serve(t, fake))
	if err != nil {
		t.Fatal(err)
	}
	// command is the client's own, through exec and attach.
	if got := cl.Info(); got.Name != "fake" || len(got.Capabilities) != 5 || !got.Has(driver.CapCommand) {
		t.Fatalf("info %+v", got)
	}
	st, err := cl.Create(ctx, driver.Spec{ID: "a", MinTier: "process"})
	if err != nil || st.ID != "a" || st.Tier != "process" || st.Phase != driver.PhaseRunning {
		t.Fatalf("create %+v %v", st, err)
	}
	if _, err := cl.Create(ctx, driver.Spec{ID: "a"}); !errors.Is(err, driver.ErrExists) {
		t.Fatalf("second create: %v", err)
	}

	big := strings.Repeat("x", 3*MaxChunk+7)
	var out bytes.Buffer
	res, err := cl.Exec(ctx, "a", driver.ExecRequest{Command: "echo hi", Stdin: strings.NewReader(big), Stdout: &out})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("exec %v %v", res, err)
	}
	if out.String() != "echo hi\n"+big {
		t.Fatalf("exec output is %d bytes, want %d", out.Len(), len("echo hi\n")+len(big))
	}

	fake.ExecFunc = func(_ context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		_, _ = io.WriteString(req.Stderr, "boom")
		return driver.ExecResult{ExitCode: 3}, nil
	}
	var errb bytes.Buffer
	res, err = cl.Exec(ctx, "a", driver.ExecRequest{Argv: []string{"false"}, Stderr: &errb})
	if err != nil || res.ExitCode != 3 || errb.String() != "boom" {
		t.Fatalf("exit code %v %v %q", res, err, errb.String())
	}
	fake.ExecFunc = nil

	resize := make(chan driver.WinSize, 1)
	resize <- driver.WinSize{Rows: 40, Cols: 100}
	var tty bytes.Buffer
	if _, err := cl.Attach(ctx, "a", driver.AttachRequest{Stdin: strings.NewReader("ls\n"), Stdout: &tty, Resize: resize}); err != nil {
		t.Fatal(err)
	}
	if tty.String() != "ls\n" {
		t.Fatalf("attach output %q", tty.String())
	}

	if err := cl.CopyIn(ctx, "a", "/w", strings.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := cl.CopyOut(ctx, "a", "/w", &out); err != nil || out.String() != big {
		t.Fatalf("copy out %d bytes, %v", out.Len(), err)
	}
	if err := cl.CopyOut(ctx, "a", "/missing", &out); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("copy out of a missing path: %v", err)
	}

	fw, err := cl.Forward(ctx, "a", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(fw, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("forward echo %q %v", buf, err)
	}
	_ = fw.Close()

	ref, err := cl.Snapshot(ctx, "a", "s1")
	if err != nil || ref.Name != "s1" {
		t.Fatalf("snapshot %+v %v", ref, err)
	}
	if err := cl.Restore(ctx, "a", ref); err != nil {
		t.Fatal(err)
	}
	if st, err := cl.Stop(ctx, "a", time.Second); err != nil || st.Phase != driver.PhaseStopped {
		t.Fatalf("stop %+v %v", st, err)
	}
	if _, err := cl.Exec(ctx, "a", driver.ExecRequest{Command: "x"}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("exec while stopped: %v", err)
	}
	if st, err := cl.Start(ctx, "a"); err != nil || st.Phase != driver.PhaseRunning {
		t.Fatalf("start %+v %v", st, err)
	}
	if err := cl.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Status(ctx, "a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("status after delete: %v", err)
	}
}

func TestUndeclaredCapabilityNeverSent(t *testing.T) {
	var calls atomic.Int32
	path := rawServer(t, func(r *bufio.Scanner, w net.Conn) {
		r.Scan()
		line(w, helloOK)
		if r.Scan() {
			calls.Add(1)
		}
	})
	ctx := context.Background()
	cl, err := Dial(ctx, "raw", path)
	if err != nil {
		t.Fatal(err)
	}
	_, e1 := cl.Attach(ctx, "a", driver.AttachRequest{})
	_, e2 := cl.Forward(ctx, "a", 80)
	_, e3 := cl.Snapshot(ctx, "a", "s")
	e4 := cl.Restore(ctx, "a", driver.SnapshotRef{})
	for i, err := range []error{e1, e2, e3, e4} {
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d undeclared calls reached the driver", n)
	}
}

// The server checks capabilities too, for a client that does not.
func TestServerRefusesUnsupportedAndUnknown(t *testing.T) {
	path := serve(t, drivertest.New("fake"))
	for call, want := range map[string]string{
		`{"type":"call","method":"copy_out","params":{"id":"a","path":"/x"}}`:        "unsupported",
		`{"type":"call","method":"forward","params":{"id":"a","port":80}}`:           "unsupported",
		`{"type":"call","method":"snapshot","params":{"id":"a"}}`:                    "unsupported",
		`{"type":"call","method":"attach","params":{"id":"a","attach":{"size":{}}}}`: "unsupported",
		`{"type":"call","method":"teleport","params":{"id":"a"}}`:                    "unsupported",
		`{"type":"call","method":"create","params":{}}`:                              "invalid",
		`{"type":"call","method":"exec","params":{"id":"a"}}`:                        "invalid",
		`{"type":"stdin"}`: "invalid",
	} {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(c)
		line(c, `{"type":"hello","versions":[1]}`)
		sc.Scan()
		line(c, call)
		var f frame
		if !sc.Scan() || json.Unmarshal(sc.Bytes(), &f) != nil || f.Error == nil || f.Error.Code != want {
			t.Errorf("%s answered %s, want %s", call, sc.Text(), want)
		}
		_ = c.Close()
	}
}

func TestVersionNegotiation(t *testing.T) {
	path := serve(t, drivertest.New("fake"))
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	line(c, `{"type":"hello","versions":[7,9]}`)
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		t.Fatal("no reply")
	}
	var f frame
	if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if f.Type != tError || f.Error == nil || f.Error.Code != "version" {
		t.Fatalf("no common version answered with %s", sc.Text())
	}
	if negotiate([]int{0, 1, 2}) != 1 || negotiate(nil) != 0 {
		t.Fatal("negotiate")
	}
}

// Each case is a driver that answers wrongly; every one must be an error.
func TestClientFailsClosed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		hello string
		reply string
		want  error
	}{
		"version not offered": {`{"type":"hello","version":2,"info":{"name":"raw","version":"1"}}`, "", driver.ErrVersion},
		"wrong name":          {`{"type":"hello","version":1,"info":{"name":"other","version":"1"}}`, "", driver.ErrInvalid},
		"no info":             {`{"type":"hello","version":1}`, "", driver.ErrVersion},
		"garbage hello":       {`not json`, "", driver.ErrUnavailable},
		"error hello":         {`{"type":"error","error":{"code":"version","message":"no"}}`, "", driver.ErrVersion},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := rawServer(t, func(r *bufio.Scanner, w net.Conn) {
				r.Scan()
				line(w, tc.hello)
			})
			if _, err := Dial(ctx, "raw", path); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}

	calls := map[string]struct {
		reply string
		want  error
	}{
		"closed without result": {"", driver.ErrUnavailable},
		"exit code missing":     {`{"type":"result","result":{}}`, driver.ErrInternal},
		"unknown error code":    {`{"type":"error","error":{"code":"weird","message":"m"}}`, driver.ErrInternal},
		"error without error":   {`{"type":"error"}`, driver.ErrInternal},
		"unexpected frame":      {`{"type":"data","data":"aGk="}`, driver.ErrInternal},
		"oversized frame":       {`{"type":"stdout","data":"` + strings.Repeat("A", MaxLine) + `"}` + "\n" + exit0, driver.ErrUnavailable},
		"oversized chunk":       {`{"type":"stdout","data":"` + base64.StdEncoding.EncodeToString(make([]byte, MaxChunk+1)) + `"}` + "\n" + exit0, driver.ErrUnavailable},
		"malformed result":      {`{"type":"result","result":{"exit_code":"zero"}}`, driver.ErrInternal},
	}
	for name, tc := range calls {
		t.Run(name, func(t *testing.T) {
			path := rawServer(t, func(r *bufio.Scanner, w net.Conn) {
				r.Scan()
				line(w, helloOK)
				r.Scan()
				if tc.reply != "" {
					line(w, tc.reply)
				}
			})
			cl, err := Dial(ctx, "raw", path)
			if err != nil {
				t.Fatal(err)
			}
			res, err := cl.Exec(ctx, "a", driver.ExecRequest{Command: "x"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v (exit %d), want %v", err, res.ExitCode, tc.want)
			}
		})
	}

	t.Run("copy out without eof", func(t *testing.T) {
		path := rawServer(t, func(r *bufio.Scanner, w net.Conn) {
			r.Scan()
			line(w, helloOK)
			r.Scan()
			line(w, `{"type":"data","data":"aGk="}`)
			line(w, `{"type":"result","result":{}}`)
		})
		cl, err := Dial(ctx, "raw", path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.CopyOut(ctx, "a", "/x", io.Discard); !errors.Is(err, driver.ErrInternal) {
			t.Fatalf("truncated copy accepted: %v", err)
		}
	})
}

func TestDriverChangedAfterDialRefused(t *testing.T) {
	var n atomic.Int32
	path := rawServer(t, func(r *bufio.Scanner, w net.Conn) {
		r.Scan()
		if n.Add(1) == 1 {
			line(w, helloOK)
			return
		}
		line(w, `{"type":"hello","version":1,"info":{"name":"raw","version":"1","capabilities":["copy","forward"]}}`)
		r.Scan()
		line(w, `{"type":"result","result":{"id":"a","phase":"running","tier":"none"}}`)
	})
	ctx := context.Background()
	cl, err := Dial(ctx, "raw", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Status(ctx, "a"); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("a driver that changed was trusted: %v", err)
	}
}

func TestCancelReachesDriver(t *testing.T) {
	fake := drivertest.New("fake")
	stopped := make(chan struct{})
	started := make(chan struct{})
	fake.ExecFunc = func(ctx context.Context, _ string, _ driver.ExecRequest) (driver.ExecResult, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return driver.ExecResult{}, ctx.Err()
	}
	path := serve(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cl, err := Dial(ctx, "fake", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Create(ctx, driver.Spec{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := cl.Exec(ctx, "a", driver.ExecRequest{Command: "sleep"})
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exec returned %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the driver's command was not cancelled")
	}
}

func TestListenRefusesSharedFolder(t *testing.T) {
	d := sockDir(t)
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(d, "s.sock")); err == nil {
		t.Fatal("socket made in a folder others can write")
	}
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(filepath.Join(d, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	info, err := os.Stat(filepath.Join(d, "s.sock"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", info.Mode(), err)
	}
}

func TestServeRefusesDishonestDriver(t *testing.T) {
	l, err := Listen(filepath.Join(sockDir(t), "x.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := Serve(context.Background(), l, drivertest.New("x", "teleport")); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

// A process handle cannot cross the socket, so a served driver that can
// hand one out is offered without that capability; the client declares its
// own, through its bridge to exec and attach, and still registers.
func TestCommandIsNeverOffered(t *testing.T) {
	ctx := context.Background()
	fake := drivertest.New("fake", driver.CapAttach, driver.CapCommand)
	cl, err := Dial(ctx, "fake", serve(t, fake))
	if err != nil {
		t.Fatal(err)
	}
	if cl.info.Has(driver.CapCommand) || !cl.info.Has(driver.CapAttach) || !cl.Info().Has(driver.CapCommand) {
		t.Fatalf("offered %+v, declared %+v", cl.info, cl.Info())
	}
	if err := driver.NewRegistry().Register(cl); err != nil {
		t.Fatal(err)
	}
}
