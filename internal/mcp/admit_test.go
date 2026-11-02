package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// A server Admit refuses is never started, is listed with why, and a restart
// does not start it; one it admits goes on to start as before.
func TestGatewayAdmit(t *testing.T) {
	g := NewGateway()
	defer g.Close()
	asked := map[string]int{}
	g.Admit = func(c ServerConfig, _ []string) error {
		asked[c.Name]++
		if c.Name == "refused" {
			return errors.New("the lists leave it out")
		}
		return nil
	}
	errs := g.Connect(context.Background(), []ServerConfig{
		{Name: "refused", Command: "/nonexistent/should-not-run", Enabled: true},
		{Name: "admitted", Command: "/nonexistent/admitted-server", Enabled: true},
		{Name: "off", Command: "/nonexistent/off", Enabled: false},
	})
	if len(errs) != 2 {
		t.Fatalf("errors: %v", errs)
	}
	var refusedErr, admittedErr error
	for _, err := range errs {
		if errors.Is(err, ErrNotAdmitted) {
			refusedErr = err
		} else {
			admittedErr = err
		}
	}
	if refusedErr == nil || admittedErr == nil {
		t.Fatalf("want one refusal and one start failure: %v", errs)
	}
	if asked["off"] != 0 {
		t.Error("a disabled server was judged")
	}
	var st ServerStatus
	for _, s := range g.Servers() {
		if s.Name == "refused" {
			st = s
		}
	}
	if !st.NotAdmitted || st.Connected || !errors.Is(st.Err, ErrNotAdmitted) {
		t.Fatalf("status: %+v", st)
	}
	if err := g.Restart(context.Background(), "refused"); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("restart: %v", err)
	}
	if asked["refused"] < 2 {
		t.Error("the restart was not judged again")
	}
}

// With PinProgram the program judged is the program started: a same-named
// binary earlier on Abhed's own PATH does not run in place of the one the
// server's PATH finds and the lists admit.
func TestGatewayStartsTheProgramItJudged(t *testing.T) {
	image := "img@sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		name, command string
		args          []string
		digest        string
		// link names the program by an absolute path through a symlink;
		// fileDigest pins that program's contents.
		link, fileDigest bool
	}{
		{name: "plain", command: "probe-mcp"},
		// A digest that pins an engine's image leaves the engine to be judged by path.
		{name: "engine with a pinned image", command: "docker", args: []string{"run", image}, digest: "sha256:" + strings.Repeat("a", 64)},
		// An absolute command through a link (/opt/homebrew/bin/npx) starts, with or without a digest.
		{name: "absolute link", command: "probe-mcp", link: true},
		{name: "absolute link with a file digest", command: "probe-mcp", link: true, fileDigest: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			allowed, other, marks := t.TempDir(), t.TempDir(), t.TempDir()
			write := func(dir, tag string) {
				script := "#!/bin/sh\necho " + tag + " > " + filepath.Join(marks, tag) + "\n"
				if err := os.WriteFile(filepath.Join(dir, c.command), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(allowed, "allowed")
			write(other, "other")
			command, digest := c.command, c.digest
			if c.link {
				links := t.TempDir() // under /var on macOS, itself a link
				command = filepath.Join(links, c.command)
				if err := os.Symlink(filepath.Join(allowed, c.command), command); err != nil {
					t.Fatal(err)
				}
			}
			if c.fileDigest {
				d, err := fileDigest(filepath.Join(allowed, c.command))
				if err != nil {
					t.Fatal(err)
				}
				digest = d
			}
			t.Setenv("PATH", other+string(os.PathListSeparator)+os.Getenv("PATH"))
			realAllowed, _ := filepath.EvalSymlinks(allowed)
			var judged []string
			g := NewGateway()
			defer g.Close()
			g.PinProgram = true
			g.Admit = func(_ ServerConfig, programs []string) error {
				judged = programs
				for _, p := range programs {
					if strings.HasPrefix(p, realAllowed+string(os.PathSeparator)) {
						return nil
					}
				}
				return errors.New("not under the allowed folder")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs := g.Connect(ctx, []ServerConfig{{Name: "probe", Command: command, Args: c.args, Digest: digest,
				Env: []string{"PATH=" + allowed}, Enabled: true}})
			for _, err := range errs {
				if errors.Is(err, ErrNotAdmitted) {
					t.Fatalf("refused: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(marks, "other")); err == nil {
				t.Fatal("the binary on Abhed's own PATH ran in place of the admitted one")
			}
			if _, err := os.Stat(filepath.Join(marks, "allowed")); err != nil {
				t.Fatalf("the admitted program did not run (judged %v): %v", judged, err)
			}
		})
	}
}

// A remote server's redirect to another host is judged by Admit, so an
// admitted host cannot hand the connection to one the lists refuse.
func TestGatewayJudgesCrossHostRedirects(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer target.Close()
	moved := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, moved+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	g := NewGateway()
	defer g.Close()
	g.Egress = egress.Unguarded
	g.Admit = func(c ServerConfig, _ []string) error {
		if strings.Contains(c.URL, "localhost") {
			return errors.New("that host is denied")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := g.Connect(ctx, []ServerConfig{{Name: "remote", URL: origin.URL + "/mcp", Enabled: true}})
	if len(errs) == 0 {
		t.Fatal("connected through a refused redirect")
	}
	if reached.Load() != 0 {
		t.Fatalf("the refused host was reached %d times: %v", reached.Load(), errs)
	}
}

// argv0File, when set, makes this test binary a stand-in server that writes
// the name it was run by and exits.
func init() {
	if f := os.Getenv("ABHED_TEST_ARGV0_FILE"); f != "" {
		_ = os.WriteFile(f, []byte(os.Args[0]), 0o600)
		os.Exit(0)
	}
}

// A pinned program runs at its resolved path but is given the configured
// name as argv[0], as a shell would give it.
func TestPinnedProgramKeepsItsName(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, out := t.TempDir(), filepath.Join(t.TempDir(), "argv0")
	if err := os.Symlink(self, filepath.Join(dir, "multi-call")); err != nil {
		t.Fatal(err)
	}
	g := NewGateway()
	defer g.Close()
	g.PinProgram = true
	g.Admit = func(ServerConfig, []string) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = g.Connect(ctx, []ServerConfig{{Name: "argv", Command: "multi-call", Enabled: true,
		Env: []string{"PATH=" + dir, "ABHED_TEST_ARGV0_FILE=" + out}}})
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the program did not run: %v", err)
	}
	if string(got) != "multi-call" {
		t.Fatalf("argv[0] is %q, want multi-call", got)
	}
}
