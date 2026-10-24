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

// Over ACP: status says what the session's commands have, offer what an
// install would fetch, and install fetches it after Studio's own ask,
// recorded in the session's record.
func TestStudioToolchains(t *testing.T) {
	fakeGoSource(t)
	r := newStudioRig(t, "")
	id := r.open()
	var st struct {
		Enabled  bool   `json:"enabled"`
		Platform string `json:"platform"`
	}
	r.cl.ok("_abhed/toolchain/status", map[string]any{"sessionId": id}, &st)
	if !st.Enabled || st.Platform == "" {
		t.Fatalf("status %+v", st)
	}
	var offer struct {
		Version string `json:"version"`
		Plan    struct {
			Why      string `json:"why"`
			Artifact struct {
				URL, Digest, Source string
			} `json:"artifact"`
		} `json:"plan"`
	}
	r.cl.ok("_abhed/toolchain/offer", map[string]any{"sessionId": id, "lang": "go"}, &offer)
	if offer.Version != "1.22.5" || !strings.HasPrefix(offer.Plan.Artifact.Digest, "sha256:") || offer.Plan.Why != "latest stable" {
		t.Fatalf("offer %+v", offer)
	}
	r.cl.refused(errParams, "_abhed/toolchain/install", map[string]any{"sessionId": id, "lang": "go"})
	var done struct {
		Dir string   `json:"dir"`
		Bin []string `json:"bin"`
	}
	r.cl.ok("_abhed/toolchain/install", map[string]any{"sessionId": id, "lang": "go", "installId": "i1"}, &done)
	if !strings.HasSuffix(done.Dir, filepath.Join("go", "1.22.5")) || len(done.Bin) != 1 {
		t.Fatalf("install %+v", done)
	}
	got, _ := r.recorded(id, "toolchain.install")
	if len(got) != 1 || got[0]["outcome"] != "ok" || got[0]["surface"] != "acp" || got[0]["consent"] != "studio" {
		t.Fatalf("recorded %v", got)
	}
	r.cl.refused(errParams, "_abhed/toolchain/offer", map[string]any{"sessionId": id, "lang": "cobol"})
	var c struct {
		Cancelled bool `json:"cancelled"`
	}
	r.cl.ok("_abhed/toolchain/cancel", map[string]any{"sessionId": id, "installId": "none"}, &c)
	if c.Cancelled {
		t.Fatal("cancelled an install that was not running")
	}
}
