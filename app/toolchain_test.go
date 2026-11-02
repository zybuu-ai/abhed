package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/loopport"
	"github.com/zybuu-ai/abhed/internal/sandbox"
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
	// Flags may follow the language, as abhed doctor writes them.
	if code, out, errb := run("", "install", "go@1.22", "-yes"); code != 0 || !strings.Contains(out, "Installed Go 1.22.5") {
		t.Fatalf("flags after the language: %d %q %q", code, out, errb)
	}
	// After "--" a word is a language, not a flag.
	if code, _, errb := run("", "install", "-yes", "--", "-platform"); code != 2 || !strings.Contains(errb, `"-platform" is not a language`) {
		t.Fatalf("a flag after --: %d %q", code, errb)
	}
	// clean removes the whole cache, read-only as it is; it asks unless -yes.
	if code, _, errb := run("", "clean"); code != 1 || !strings.Contains(errb, "add -yes") {
		t.Fatalf("clean not at a terminal: %d %q", code, errb)
	}
	if code, out, errb := run("", "clean", "-yes"); code != 0 || !strings.Contains(out, "removed the toolchain cache") {
		t.Fatalf("clean: %d %q %q", code, out, errb)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("the cache outlived clean")
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
	// An agreement naming other registries than offered is not one.
	r.cl.refused(errRefused, "_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "i0b",
		"agreed": map[string]any{"version": st.Offer.Version, "digest": st.Offer.Digest, "registries": []string{"evil.example"}}})
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

// An agreement covers the language a language server runs on that comes
// first: a newer Node.js than the one shown is a change.
func TestAgreementCoversWhatComesFirst(t *testing.T) {
	plan := toolchain.Plan{Spec: toolchain.Spec{Lang: "python"}, Installed: &toolchain.Installed{Version: "3.12.7", Digest: "sha256:py"}}
	node := toolchain.Plan{Spec: toolchain.Spec{Lang: "node"}, Artifact: toolchain.Artifact{Version: "24.9.0", Digest: "sha256:n1"}}
	lp := &toolchain.ExtraPlan{Extra: toolchain.Extra{Version: "1.40.2"}, Digest: "sha512:b", Needs: &node}
	a := &toolchainAgreed{Version: "3.12.7", Digest: "sha256:py", LSPVersion: "1.40.2", LSPDigest: "sha512:b",
		NeedsVersion: "24.9.0", NeedsDigest: "sha256:n1", Registries: &[]string{"pypi.org"}}
	if a.changedFrom(plan, nil, lp, []string{"pypi.org"}) {
		t.Fatal("what was shown is a change")
	}
	moved := node
	moved.Artifact = toolchain.Artifact{Version: "24.10.0", Digest: "sha256:n2"}
	lp.Needs = &moved
	if !a.changedFrom(plan, nil, lp, []string{"pypi.org"}) {
		t.Fatal("a newer Node.js than shown was not a change")
	}
	lp.Needs = nil
	if !a.changedFrom(plan, nil, lp, []string{"pypi.org"}) {
		t.Fatal("an agreement to install Node.js first matched a plan without it")
	}
	a.NeedsVersion, a.NeedsDigest = "", ""
	if a.changedFrom(plan, nil, lp, []string{"pypi.org"}) || !a.changedFrom(plan, nil, lp, []string{"pypi.org", "files.pythonhosted.org"}) {
		t.Fatal("registries")
	}
}

// Studio's agreement as it sends it today: version and digest, no
// registries. The install is made, the same offer is not "changed", and no
// registries open without being agreed to.
func TestStudioToolchainAgreementWithoutRegistries(t *testing.T) {
	fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var st struct {
		Offer struct {
			Version, Digest string
		} `json:"offer"`
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "go"}, &st)
	var done struct {
		Version    string `json:"version"`
		Registries any    `json:"registries"`
	}
	r.cl.ok("_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "s1",
		"agreed": map[string]any{"version": st.Offer.Version, "digest": st.Offer.Digest}}, &done)
	if done.Version != st.Offer.Version {
		t.Fatalf("install %+v", done)
	}
	if got, _ := r.recorded(id, "toolchain.registry"); len(got) != 0 {
		t.Fatalf("registries opened without agreement: %v", got)
	}
	got, _ := r.recorded(id, "toolchain.install")
	if len(got) != 1 || got[0]["consent"] != "studio" {
		t.Fatalf("recorded %v", got)
	}
}

// toolchain.manage: open an installed language's registries as offered,
// remove a language's toolchain, and clean the whole cache once the person
// says so; each recorded.
func TestStudioToolchainManage(t *testing.T) {
	root := fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var st struct {
		Offer struct {
			Version, Digest string
			Registries      []string `json:"registries"`
		} `json:"offer"`
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "go"}, &st)
	r.cl.ok("_abhed/toolchain/install", map[string]any{"sessionId": id, "language": "go", "installId": "m1",
		"agreed": map[string]any{"version": st.Offer.Version, "digest": st.Offer.Digest}}, nil)
	if len(st.Offer.Registries) > 0 {
		r.cl.refused(errRefused, "_abhed/toolchain/registries", map[string]any{"sessionId": id, "language": "go", "registries": []string{"evil.example"}})
		var opened struct {
			Hosts []string `json:"hosts"`
		}
		r.cl.ok("_abhed/toolchain/registries", map[string]any{"sessionId": id, "language": "go", "registries": st.Offer.Registries}, &opened)
		if len(opened.Hosts) != len(st.Offer.Registries) {
			t.Fatalf("opened %v", opened)
		}
		if got, _ := r.recorded(id, "toolchain.registry"); len(got) != 1 {
			t.Fatalf("registry opening recorded %v", got)
		}
	}
	var rm struct {
		Removed []string `json:"removed"`
	}
	r.cl.ok("_abhed/toolchain/remove", map[string]any{"sessionId": id, "language": "go"}, &rm)
	if len(rm.Removed) != 1 || rm.Removed[0] != "1.22.5" {
		t.Fatalf("removed %+v", rm)
	}
	r.cl.refused(errParams, "_abhed/toolchain/remove", map[string]any{"sessionId": id, "language": "go"})
	// Clean asks the person; kept on a no, removed on a yes.
	var cl struct {
		Cleaned bool `json:"cleaned"`
	}
	r.cl.ok("_abhed/toolchain/clean", map[string]any{"sessionId": id}, &cl)
	if cl.Cleaned {
		t.Fatal("cleaned without the person's yes")
	}
	r.cl.answering(func(method string, params json.RawMessage) any { return chosen(params, "allow_once") })
	r.cl.ok("_abhed/toolchain/clean", map[string]any{"sessionId": id}, &cl)
	if !cl.Cleaned {
		t.Fatal("not cleaned on the person's yes")
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("the cache outlived clean")
	}
	got, _ := r.recorded(id, "toolchain.install")
	var outcomes []string
	for _, g := range got {
		outcomes = append(outcomes, g["lang"].(string)+":"+g["outcome"].(string))
	}
	if strings.Join(outcomes, " ") != "go:ok go:removed *:declined *:removed" {
		t.Fatalf("recorded %v", outcomes)
	}
	old := inAgentCommand
	inAgentCommand = func() string { return "ABHED_SESSION is set" }
	defer func() { inAgentCommand = old }()
	r.cl.refused(errPolicy, "_abhed/toolchain/clean", map[string]any{"sessionId": id})
}

// A command in Studio's terminal refused a package registry is told which
// language's it was and how to open it, once, Studio is sent the offer, and
// the terminal's header says what it can reach.
func TestTerminalNamesARefusedRegistry(t *testing.T) {
	fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var term struct {
		TerminalID  string   `json:"terminalId"`
		Registries  []string `json:"registries"`
		NetworkNote string   `json:"networkNote"`
	}
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	if term.Registries == nil || !strings.Contains(term.NetworkNote, "network:") {
		t.Fatalf("header %+v", term)
	}
	// The typed line, echoed back, is not watched: only what commands print.
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": `: 403 Forbidden - GET https://registry.npmjs.org/x` + "\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "$")
	time.Sleep(200 * time.Millisecond)
	for _, m := range r.cl.since(from) {
		if m.Method == "_abhed/toolchain/registry_refused" {
			t.Fatal("offered for the echo of a typed line")
		}
	}
	from = r.cl.mark()
	refuse := `printf '%s 403 Forbidden - GET https://%s/left-pad\n' "npm error" registry.npmjs.org; exit 1`
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": refuse + "\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "Node.js's package registry (registry.npmjs.org) is closed")
	r.cl.waitFor(from, "the registry offer", func(m rpcMessage) bool {
		return m.Method == "_abhed/toolchain/registry_refused" && strings.Contains(string(m.Params), `"language":"node"`)
	})
	// Once per language per terminal.
	from = r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": refuse + "\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "left-pad")
	time.Sleep(200 * time.Millisecond)
	for _, m := range r.cl.since(from) {
		if m.Method == "_abhed/toolchain/registry_refused" {
			t.Fatal("offered twice")
		}
	}
}

// A language's registries open on the person's agreement with no toolchain
// installed, as the host's own tools need: the opening is recorded, and a
// new terminal's header names them.
func TestRegistriesOpenWithoutAToolchain(t *testing.T) {
	fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var st struct {
		Available bool `json:"available"`
		Offer     struct {
			Registries []string `json:"registries"`
		} `json:"offer"`
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id, "language": "go"}, &st)
	if st.Available {
		t.Fatal("go installed before the test")
	}
	if len(st.Offer.Registries) == 0 {
		t.Skip("this sandbox opens no registries")
	}
	var opened struct {
		Hosts []string `json:"hosts"`
	}
	r.cl.ok("_abhed/toolchain/registries", map[string]any{"sessionId": id, "language": "go", "registries": st.Offer.Registries}, &opened)
	if !slices.Equal(opened.Hosts, st.Offer.Registries) {
		t.Fatalf("opened %v, offered %v", opened.Hosts, st.Offer.Registries)
	}
	got, _ := r.recorded(id, "toolchain.registry")
	if len(got) != 1 || got[0]["lang"] != "go" || got[0]["by"] != "user" || got[0]["surface"] != "acp" {
		t.Fatalf("recorded %v", got)
	}
	var term struct {
		Registries  []string `json:"registries"`
		NetworkNote string   `json:"networkNote"`
	}
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	for _, h := range st.Offer.Registries {
		if !slices.Contains(term.Registries, h) || !strings.Contains(term.NetworkNote, h) {
			t.Fatalf("header %+v lacks %s", term, h)
		}
	}
}

// A server refused its port in Studio's terminal is named, with the offer
// to let commands listen; once the person agrees for the workspace, a new
// terminal's server is reachable, its address sent to Studio once, and a
// new session starts with it, until the person takes it back.
func TestTerminalLetsAServerListen(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("listening is switched only on the macOS process tier")
	}
	free := func() int {
		for {
			ports, err := sandbox.FreeLoopbackPorts(1)
			if err != nil {
				t.Fatal(err)
			}
			if p := int(ports[0]); !loopport.In(p) {
				return p
			}
		}
	}
	devPort, hostPort := free(), free()
	r := newStudioRig(t, fmt.Sprintf(`,"sandbox":{"listen_ports":["%d","%d"]}`, devPort, hostPort))
	id := r.open()
	type termReply struct {
		TerminalID string `json:"terminalId"`
		Tier       string `json:"tier"`
		Serves     bool   `json:"serves"`
	}
	term := func(id string) termReply {
		var tr termReply
		r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &tr)
		return tr
	}
	input := func(tid, line string) int {
		from := r.cl.mark()
		r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": tid, "data": line + "\r"}, nil)
		return from
	}
	first := term(id)
	if first.Tier != "process" {
		t.Skipf("listening is switched only on the macOS process tier, not %s", first.Tier)
	}
	if first.Serves {
		t.Fatal("commands listen before the person agreed")
	}
	from := input(first.TerminalID, `printf 'Error: listen %s: operation not permitted 127.0.0.1:5173\n' EPERM`)
	offer := r.cl.waitFor(from, "the listen offer", func(m rpcMessage) bool {
		return m.Method == "_abhed/terminal/listen_refused" && strings.Contains(string(m.Params), `"canAllow":true`)
	})
	// The offer says whom the agreement covers, and on which ports.
	if p := string(offer.Params); !strings.Contains(p, "every session of this workspace, the agent's too") || !strings.Contains(p, strconv.Itoa(devPort)) {
		t.Fatalf("offer %s", p)
	}
	r.cl.refused(errParams, "_abhed/sandbox/listen", map[string]any{"sessionId": id})
	var got struct {
		Listens bool `json:"listens"`
	}
	r.cl.ok("_abhed/sandbox/listen", map[string]any{"sessionId": id, "allow": true}, &got)
	if !got.Listens {
		t.Skip("listening is not available here: the Mac's listeners cannot be read, or this build cannot tell a sandboxed process")
	}
	if changed, _ := r.recorded(id, agent.EvPermissionChanged); len(changed) != 1 || changed[0]["rule"] != listenRule || changed[0]["op"] != "add" {
		t.Fatalf("recorded %v", changed)
	}
	second := term(id)
	if !second.Serves {
		t.Fatal("a new terminal does not serve")
	}
	// A host program on the other listen port: its address is never offered.
	host, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	// Split, so the typed line's echo holds no address.
	from = input(second.TerminalID, fmt.Sprintf(`printf '  Local:   http://%%s:%d/app\n' localhost; printf 'again http://localhost:%d/app\n'; printf 'host http://localhost:%d/\n'; printf 'out http://localhost:9/\n'`, devPort, devPort, hostPort))
	want := fmt.Sprintf(`"url":"http://localhost:%d/app"`, devPort)
	r.cl.waitFor(from, "the serving address", func(m rpcMessage) bool {
		return m.Method == "_abhed/terminal/serving" && strings.Contains(string(m.Params), want)
	})
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, m := range r.cl.since(from) {
		if m.Method == "_abhed/terminal/serving" {
			n++
			if !strings.Contains(string(m.Params), want) {
				t.Fatalf("offered %s", m.Params)
			}
		}
	}
	if n != 1 {
		t.Fatalf("sent %d times", n)
	}
	// A server refused a port outside the listen ports is told which it may use.
	from = input(second.TerminalID, `printf 'Error: listen %s: operation not permitted 127.0.0.1:9\n' EPERM`)
	waitOutput(r.cl, from, second.TerminalID, "may listen only on TCP ports")

	// A program of the person's that listens on a port a sandboxed server
	// holds ends that server, and the person and the record are told. An
	// interactive terminal, so the server runs while the test goes on.
	var shell termReply
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "interactive", "cols": 120, "rows": 24}, &shell)
	from = input(shell.TerminalID, fmt.Sprintf(`python3 -I -c 'import socket,time; s=socket.socket(); s.bind(("127.0.0.1", %d)); s.listen(); print("serv"+"ing", flush=True); time.sleep(30)'`, devPort))
	waitOutput(r.cl, from, shell.TerminalID, "serving")
	taker, err := net.Listen("tcp", fmt.Sprintf(":%d", devPort))
	if err != nil {
		t.Fatalf("the host could not listen beside the sandboxed server: %v", err)
	}
	defer taker.Close()
	r.cl.waitFor(from, "the ended listener", func(m rpcMessage) bool {
		return m.Method == "_abhed/sandbox/listener_ended" && strings.Contains(string(m.Params), fmt.Sprintf(`"port":%d`, devPort))
	})
	if ended, actors := r.recorded(id, agent.EvSandboxListenerEnded); len(ended) == 0 || actors[0] != agent.ActorSystem {
		t.Fatalf("recorded %v %v", ended, actors)
	}

	// Kept for the workspace: a new session starts listening, recorded.
	id2 := r.open()
	if !term(id2).Serves {
		t.Fatal("a new session of the workspace does not listen")
	}
	if changed, actors := r.recorded(id2, agent.EvPermissionChanged); len(changed) != 1 || actors[0] != agent.ActorSystem {
		t.Fatalf("recorded at the start %v %v", changed, actors)
	}
	r.cl.ok("_abhed/sandbox/listen", map[string]any{"sessionId": id2, "allow": false}, &got)
	if got.Listens || term(id).Serves || term(r.open()).Serves {
		t.Fatal("still listening after the person took it back")
	}
	old := inAgentCommand
	inAgentCommand = func() string { return "ABHED_SESSION is set" }
	defer func() { inAgentCommand = old }()
	r.cl.refused(errPolicy, "_abhed/sandbox/listen", map[string]any{"sessionId": id, "allow": true})
}

// Under sandbox.listen allow the person cannot take listening back, so the
// record never shows a revoke that did not happen.
func TestListenAllowCannotBeTakenBack(t *testing.T) {
	r := newStudioRig(t, `,"sandbox":{"listen":"allow"}`)
	id := r.open()
	r.cl.refused(errPolicy, "_abhed/sandbox/listen", map[string]any{"sessionId": id, "allow": false})
	if changed, _ := r.recorded(id, agent.EvPermissionChanged); len(changed) != 0 {
		t.Fatalf("recorded %v", changed)
	}
}

// An interactive terminal names the project's venv as not in use; once the
// person types abhed_venv, Studio is told it is.
func TestTerminalVenvInUseAfterAbhedVenv(t *testing.T) {
	r := newStudioRig(t, "")
	v := filepath.Join(r.ws, ".venv")
	r.write(".venv/pyvenv.cfg", "home = /usr/bin\n")
	if err := os.MkdirAll(filepath.Join(v, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/python3", filepath.Join(v, "bin", "python")); err != nil {
		t.Fatal(err)
	}
	id := r.open()
	var term struct {
		TerminalID string `json:"terminalId"`
		Venv       *struct {
			Path  string `json:"path"`
			InUse bool   `json:"inUse"`
		} `json:"venv"`
	}
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "interactive", "cols": 120, "rows": 24}, &term)
	if term.Venv == nil || term.Venv.InUse || term.Venv.Path != v {
		t.Fatalf("venv %+v", term.Venv)
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "abhed_venv\r"}, nil)
	r.cl.waitFor(from, "the venv in use", func(m rpcMessage) bool {
		return m.Method == "_abhed/terminal/venv" && strings.Contains(string(m.Params), `"inUse":true`)
	})
}
