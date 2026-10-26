package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// fakeGoSource serves a Go release list with Go 1.22.5 for this host and
// for Linux, and points the toolchain installs at it; the cache is a
// temporary one.
func fakeGoSource(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "#!/bin/sh\necho go1.22.5\n"
	_ = tw.WriteHeader(&tar.Header{Name: "go/bin/go", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	arch := buf.Bytes()
	sum := sha256.Sum256(arch)
	var files []string
	for _, os := range []string{runtime.GOOS, "linux"} {
		files = append(files, fmt.Sprintf(`{"filename":"go1.22.5.%s-%s.tar.gz","os":%q,"arch":%q,"sha256":%q,"size":%d,"kind":"archive"}`,
			os, runtime.GOARCH, os, runtime.GOARCH, hex.EncodeToString(sum[:]), len(arch)))
	}
	list := `[{"version":"go1.22.5","stable":true,"files":[` + strings.Join(files, ",") + `]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			_, _ = w.Write(arch)
			return
		}
		_, _ = w.Write([]byte(list))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	old := toolset.ToolchainEndpoints
	toolset.ToolchainEndpoints = toolchain.Endpoints{GoDL: srv.URL + "/dl/", ExtraHosts: []string{u.Host}}
	t.Cleanup(func() { toolset.ToolchainEndpoints = old })
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Cleanup(func() {
		_ = os.Chmod(cache, 0o755)
		_ = filepath.Walk(cache, func(p string, i os.FileInfo, err error) error { _ = os.Chmod(p, 0o755); return nil })
	})
	return filepath.Join(cache, "abhed", "toolchains")
}

// abhed toolchain install, list and remove, from a terminal or with -yes,
// each install recorded; refused inside an agent's command and when off.
func TestToolchainCommand(t *testing.T) {
	root := fakeGoSource(t)
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := toolchainCmd(ws, args, "", strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, _, errb := run("", "install", "go"); code != 1 || !strings.Contains(errb, "add -yes") {
		t.Fatalf("not at a terminal: %d %q", code, errb)
	}
	code, out, errb := run("", "install", "-yes", "go@1.22")
	if code != 0 || !strings.Contains(out, "Installed Go 1.22.5, verified") || !strings.Contains(out, "sha256:") {
		t.Fatalf("install: %d %q %q", code, out, errb)
	}
	if !strings.Contains(errb, "Recorded as session") {
		t.Fatalf("not recorded: %q", errb)
	}
	if _, err := os.Stat(filepath.Join(root, toolchain.Host().String(), "go", "1.22.5", "bin", "go")); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := run("", "install", "-yes", "-debugger", "-platform", "linux", "go@1.22"); code != 1 || !strings.Contains(errb, "1.25 or later") {
		t.Fatalf("a debugger Go 1.22 cannot build: %d %q", code, errb)
	}
	if code, out, _ := run("", "list", "-json"); code != 0 || !strings.Contains(out, `"version": "1.22.5"`) {
		t.Fatalf("list: %d %s", code, out)
	}
	var listed struct {
		Platform string `json:"platform"`
	}
	_, out, _ = run("", "list", "-json", "-platform", "linux")
	if json.Unmarshal([]byte(out), &listed) != nil || listed.Platform != "linux-"+runtime.GOARCH {
		t.Fatalf("linux list: %s", out)
	}
	if code, out, errb := run("", "remove", "go@1.22"); code != 0 || !strings.Contains(out, "removed go 1.22.5") {
		t.Fatalf("remove: %d %q %q", code, out, errb)
	}
	old := inAgentCommand
	inAgentCommand = func() string { return "ABHED_SESSION is set" }
	if code, _, errb := run("", "install", "-yes", "go"); code != 1 || !strings.Contains(errb, "inside an agent's command") {
		t.Fatalf("inside a command: %d %q", code, errb)
	}
	inAgentCommand = old
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".abhed"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"sandbox":{"toolchains":"off"}}`), 0o600)
	if code, _, errb := run("", "install", "-yes", "go"); code != 1 || !strings.Contains(errb, "turned off") {
		t.Fatalf("off: %d %q", code, errb)
	}
}

// Over ACP: status says whether the session's commands have a language,
// named as a program too, and what an install would fetch; install fetches
// it after Studio's own ask, recorded in the session's record.
func TestStudioToolchains(t *testing.T) {
	fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var st struct {
		Language  string `json:"language"`
		Available bool   `json:"available"`
		CanAct    bool   `json:"can_act"`
		Offer     struct {
			Command, Source, Version, Into, Why, Digest string
			Size                                        int64
			Registries                                  []string `json:"registries"`
			RegistriesNote                              string   `json:"registries_note"`
		} `json:"offer"`
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "gofmt"}, &st)
	if st.Language != "go" || st.Available || !st.CanAct || st.Offer.Version != "1.22.5" || st.Offer.Command != "abhed toolchain install go@1.22.5" ||
		!strings.HasPrefix(st.Offer.Digest, "sha256:") || st.Offer.Size == 0 || !strings.HasSuffix(st.Offer.Into, filepath.Join("go", "1.22.5")) {
		t.Fatalf("status %+v", st)
	}
	t.Logf("offer registries %v note %q", st.Offer.Registries, st.Offer.RegistriesNote)
	if st.Offer.Registries == nil || len(st.Offer.Registries) == 0 && st.Offer.RegistriesNote == "" {
		t.Fatalf("the offer does not say which registries the install opens: %+v", st.Offer)
	}
	r.cl.refused(errParams, "_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go"})
	r.cl.refused(errRefused, "_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "i0",
		"agreed": map[string]any{"version": "1.22.4", "digest": st.Offer.Digest}})
	var done struct {
		Version    string   `json:"version"`
		Dir        string   `json:"dir"`
		Bin        []string `json:"bin"`
		Registries string   `json:"registries"`
	}
	// An agreement that does not list the registries the install would open is not one.
	r.cl.refused(errRefused, "_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "i0b",
		"agreed": map[string]any{"version": st.Offer.Version, "digest": st.Offer.Digest}})
	r.cl.ok("_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "i1",
		"agreed": map[string]any{"version": st.Offer.Version, "digest": st.Offer.Digest, "registries": st.Offer.Registries}}, &done)
	if done.Version != "1.22.5" || len(done.Bin) != 1 {
		t.Fatalf("install %+v", done)
	}
	// What the offer named is what the install opened.
	for _, h := range st.Offer.Registries {
		if !strings.Contains(done.Registries, h) {
			t.Fatalf("the install opened %q, the offer named %v", done.Registries, st.Offer.Registries)
		}
	}
	if got, _ := r.recorded(id, "toolchain.registry"); len(st.Offer.Registries) > 0 && len(got) != 1 {
		t.Fatalf("registry opening recorded %v", got)
	}
	got, _ := r.recorded(id, "toolchain.install")
	if len(got) != 1 || got[0]["outcome"] != "ok" || got[0]["surface"] != "acp" || got[0]["consent"] != "studio" {
		t.Fatalf("recorded %v", got)
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "go"}, &st)
	if !st.Available {
		t.Fatalf("after the install %+v", st)
	}
	r.cl.refused(errParams, "_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "cobol"})
	var c struct {
		Cancelled bool `json:"cancelled"`
	}
	r.cl.ok("_abhed/toolchain/cancel", map[string]any{"installId": "none"}, &c)
	if c.Cancelled {
		t.Fatal("cancelled an install that was not running")
	}

	// The debugger is offered with its language, or refused with why; an
	// install asking for one that cannot be had installs nothing.
	var dbg struct {
		CanAct   bool `json:"can_act"`
		Debugger struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
		} `json:"debugger"`
		Refusal struct{ Kind, Message string } `json:"refusal"`
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "go", "debugger": true}, &dbg)
	if dbg.CanAct || dbg.Debugger.Name != "delve" || dbg.Debugger.Available || dbg.Refusal.Kind != "no_debugger" ||
		!strings.Contains(dbg.Refusal.Message, "Delve") {
		t.Fatalf("debugger status %+v", dbg)
	}
	r.cl.refused(errRefused, "_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "debugger": true, "installId": "i2"})
	// The language server likewise: gopls needs Go 1.26 to build, so Go 1.22's is refused.
	var lsp struct {
		CanAct bool `json:"can_act"`
		LSP    struct {
			Name string `json:"name"`
		} `json:"lsp"`
		Refusal struct{ Kind, Message string } `json:"refusal"`
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "go", "lsp": true}, &lsp)
	if lsp.CanAct || lsp.LSP.Name != "gopls" || lsp.Refusal.Kind != "no_lsp" || !strings.Contains(lsp.Refusal.Message, "gopls") {
		t.Fatalf("lsp status %+v", lsp)
	}
	r.cl.refused(errRefused, "_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "lsp": true, "installId": "i3"})
	if got, _ := r.recorded(id, "toolchain.install"); len(got) != 1 {
		t.Fatalf("a refused debugger was recorded as an install: %v", got)
	}
}

// Studio's lines terminal, which its Run and Test buttons use, finds the
// installed toolchains as the agent's commands do, and a missing program's
// line says which language Abhed installs.
func TestStudioLinesTerminalGetsTheToolchains(t *testing.T) {
	fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var done struct {
		Version string `json:"version"`
	}
	r.cl.ok("_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "i1"}, &done)
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "go version\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "go1.22.5")
	from = r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "cargo build\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "toolchain tool")
}

// An install from an older Studio that sends no agreement is made, recorded
// as unconfirmed, and opens no registries.
func TestStudioToolchainInstallWithoutAgreement(t *testing.T) {
	fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var done struct {
		Version    string `json:"version"`
		Registries string `json:"registries"`
	}
	r.cl.ok("_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "u1"}, &done)
	if done.Version != "1.22.5" || !strings.Contains(done.Registries, "not opened") {
		t.Fatalf("install %+v", done)
	}
	if got, _ := r.recorded(id, "toolchain.registry"); len(got) != 0 {
		t.Fatalf("an unconfirmed install opened registries: %v", got)
	}
	got, _ := r.recorded(id, "toolchain.install")
	if len(got) != 1 || got[0]["consent"] != "studio-unconfirmed" {
		t.Fatalf("recorded %v", got)
	}
}
