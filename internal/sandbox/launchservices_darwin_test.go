package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// lookupProbe asks launchd for each service by name and prints the result:
// 0 found, 1100 refused by the sandbox, 1102 no such service. It opens nothing.
const lookupProbe = `import ctypes, sys
lib = ctypes.CDLL("/usr/lib/libSystem.B.dylib")
bp = ctypes.c_uint.in_dll(lib, "bootstrap_port")
for n in sys.argv[1:]:
    p = ctypes.c_uint(0)
    print("LOOKUP", n, lib.bootstrap_look_up(bp, n.encode(), ctypes.byref(p)), flush=True)
`

// launchServices are the services `open` and osascript reach an app through.
var launchServices = []string{"com.apple.coreservices.launchservicesd", "com.apple.lsd.open",
	"com.apple.lsd.mapdb", "com.apple.lsd.modifydb", "com.apple.coreservices.appleevents",
	"com.apple.CoreServices.coreservicesd"}

// unhandledURL has a scheme no app claims, so open launches nothing even outside the sandbox.
const unhandledURL = "abhed-unclaimed-scheme-7f3d://probe"

// probeSource is the probe, once launchd is seen to find each service outside the sandbox.
func probeSource(t *testing.T) []byte {
	t.Helper()
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	f := filepath.Join(t.TempDir(), "probe.py")
	if err := os.WriteFile(f, []byte(lookupProbe), 0o600); err != nil {
		t.Fatal(err)
	}
	// The positive control: outside the sandbox launchd finds each one.
	out, err := exec.Command("/usr/bin/python3", append([]string{f}, launchServices...)...).CombinedOutput()
	if err != nil {
		t.Skipf("the probe does not run here: %v %s", err, out)
	}
	for _, n := range launchServices {
		if !strings.Contains(string(out), "LOOKUP "+n+" 0\n") {
			t.Fatalf("%s is not found outside the sandbox, so the test proves nothing:\n%s", n, out)
		}
	}
	return []byte(lookupProbe)
}

// A command reaches no LaunchServices, which would open a URL or an app
// outside the sandbox: with the network off, on, and under the allowlist.
func TestSeatbeltRefusesLaunchServices(t *testing.T) {
	src := probeSource(t)
	check := func(ctx context.Context, t *testing.T, s *Process, ws string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, "probe.py"), src, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := "/usr/bin/python3 probe.py " + strings.Join(launchServices, " ")
		out, _ := s.Command(ctx, ws, cmd).CombinedOutput()
		// The workbench terminal, an interactive shell, is held the same.
		sh := s.Shell(ctx, ws)
		sh.Stdin = strings.NewReader(cmd + "\nexit\n")
		term, _ := sh.CombinedOutput()
		for _, n := range launchServices {
			if !strings.Contains(string(out), "LOOKUP "+n+" 1100\n") {
				t.Errorf("ESCAPE: %s is not refused to a command:\n%s", n, out)
			}
			if !strings.Contains(string(term), "LOOKUP "+n+" 1100") {
				t.Errorf("ESCAPE: %s is not refused to the terminal:\n%s", n, term)
			}
		}
	}
	// Outside, LaunchServices answers that no app claims the scheme; inside it cannot.
	host, _ := exec.Command("/usr/bin/open", unhandledURL).CombinedOutput()
	openControl := strings.Contains(string(host), "kLSApplicationNotFoundErr")
	if !openControl {
		t.Logf("open gives no kLSApplicationNotFoundErr outside the sandbox here, so its end effect is not checked:\n%s", host)
	}
	checkOpen := func(ctx context.Context, t *testing.T, s *Process, ws string) {
		t.Helper()
		out, err := s.Command(ctx, ws, "/usr/bin/open "+unhandledURL).CombinedOutput()
		if err == nil || strings.Contains(string(out), "kLSApplicationNotFoundErr") {
			t.Errorf("ESCAPE: open reached LaunchServices from a command (err %v):\n%s", err, out)
		}
	}
	for _, allowNet := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_network=%v", allowNet), func(t *testing.T) {
			ws := workspace(t)
			s := processSandbox(t, ws, allowNet).(*Process)
			check(context.Background(), t, s, ws)
			if openControl {
				checkOpen(context.Background(), t, s, ws)
			}
		})
	}
	t.Run("allowlist", func(t *testing.T) {
		s, ws := egressProcess(t)
		ctx := WithLaunch(context.Background(), Launch{CallID: "c-ls", Session: "sess-ls"})
		check(ctx, t, s, ws)
		if openControl {
			checkOpen(ctx, t, s, ws)
		}
	})
}

// Ordinary tools still work under the allowlist with LaunchServices denied:
// git, curl through the proxy, python3, and node and go where installed.
func TestSeatbeltToolsWorkWithoutLaunchServices(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "egress ok "+r.URL.Path)
	}))
	defer srv.Close()
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())
	old := egressResolve
	egressResolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "allowed.test" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}
	t.Cleanup(func() { egressResolve = old })

	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.Egress = egressPolicy(t, egress.Config{Rules: []egress.Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.Close() })
	var rec eventLog
	l := Launch{CallID: "c-tools", Session: "sess-tools", Record: rec.record}

	tools := map[string]string{
		"git":  `git init -q r && cd r && echo x > f && git add f && git -c user.name=t -c user.email=t@t -c commit.gpgsign=false commit -qm m && git log --oneline | wc -l | tr -d ' ' && echo TOOL-OK`,
		"curl": fmt.Sprintf("curl -sS -m 20 http://allowed.test:%d/tools | grep -q 'egress ok /tools' && echo TOOL-OK", port),
	}
	if _, err := os.Stat("/usr/bin/python3"); err == nil {
		tools["python3"] = `/usr/bin/python3 -c 'import json,subprocess;print(json.dumps(1));subprocess.run(["true"],check=True)' && echo TOOL-OK`
	}
	if node, err := exec.LookPath("node"); err == nil {
		tools["node"] = node + ` -e 'require("fs").writeFileSync("n.txt","1");console.log("TOOL-OK")'`
	}
	if goBin, err := exec.LookPath("go"); err == nil {
		if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() { println(\"built\") }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		tools["go build"] = "GOTOOLCHAIN=local " + goBin + " build -o m . && ./m && echo TOOL-OK"
	}
	for name, cmd := range tools {
		if out := runLaunched(t, s, ws, l, cmd); !strings.Contains(out, "TOOL-OK") {
			t.Errorf("%s fails in the sandbox:\n%s", name, out)
		}
	}
}
