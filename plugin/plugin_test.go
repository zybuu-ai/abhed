package plugin_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/testenv"
	"github.com/zybuu-ai/abhed/plugin"
	"github.com/zybuu-ai/abhed/plugin/plugintest"
)

func TestMain(m *testing.M) { testenv.Main(m) }

const (
	signer = "release@acme.example"
	issuer = "https://issuer.acme.example"
)

func sample() plugintest.Spec {
	return plugintest.Spec{
		Name: "acme-review", Version: "1.0.0", Publisher: "Acme",
		Files: map[string]string{
			"agents/reviewer.md":   "---\nname: acme-reviewer\ndescription: reviews\ntools: [read, grep]\n---\nReview the change.\n",
			"skills/lint/SKILL.md": "---\nname: acme-lint\ndescription: lints\n---\nRun the linter.\n",
			"skills/lint/run.sh":   "#!/bin/sh\necho lint\n",
			"commands/review.md":   "---\ndescription: review\n---\nReview $ARGUMENTS\n",
			"hooks/no-rm.sh":       "#!/bin/sh\nexit 0\n",
			"docs/README.md":       "docs\n",
			"README.md":            "Acme review\n",
		},
		Hooks:      []plugin.Hook{{Name: "no-rm", Events: []string{"tool_call"}, Command: "sh " + plugin.RootVar + "/hooks/no-rm.sh"}},
		MCPServers: []plugin.MCPServer{{Name: "tickets", URL: "https://tickets.acme.example/mcp"}},
	}
}

func signedBundle(t *testing.T, ss *plugintest.Sigstore) (string, *plugin.TrustedRoot) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	plugintest.Write(dir, sample())
	ss.SignDir(dir, signer, issuer)
	root, err := plugin.ParseTrustedRoot(ss.TrustedRoot())
	if err != nil {
		t.Fatal(err)
	}
	return dir, root
}

func TestSignatureVerifies(t *testing.T) {
	ss := plugintest.NewSigstore()
	dir, root := signedBundle(t, ss)
	b, err := plugin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := plugin.VerifySignature(root, b.ManifestBytes, b.Signature, time.Now())
	if err != nil {
		t.Fatalf("a good signature failed: %v", err)
	}
	if got.Identity != signer || got.Issuer != issuer {
		t.Fatalf("signer = %+v", got)
	}
	needs := b.Needs()
	if strings.Join(needs.Tools, ",") != "grep,read" || strings.Join(needs.Egress, ",") != "tickets.acme.example" ||
		strings.Join(needs.Hooks, ",") != "tool_call" || strings.Join(needs.MCPServers, ",") != "tickets" {
		t.Fatalf("needs = %+v", needs)
	}
}

func TestSignatureFails(t *testing.T) {
	ss := plugintest.NewSigstore()
	dir, root := signedBundle(t, ss)
	b, err := plugin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	other := plugintest.NewSigstore()
	otherRoot, _ := plugin.ParseTrustedRoot(other.TrustedRoot())
	cases := map[string]func() error{
		"another manifest": func() error {
			_, err := plugin.VerifySignature(root, append(append([]byte{}, b.ManifestBytes...), ' '), b.Signature, time.Now())
			return err
		},
		"another trusted root": func() error {
			_, err := plugin.VerifySignature(otherRoot, b.ManifestBytes, b.Signature, time.Now())
			return err
		},
		"no trusted root": func() error {
			_, err := plugin.VerifySignature(nil, b.ManifestBytes, b.Signature, time.Now())
			return err
		},
		"signature swapped for another's": func() error {
			_, err := plugin.VerifySignature(root, b.ManifestBytes, ss.Sign([]byte("other"), signer, issuer), time.Now())
			return err
		},
		"signed by an untrusted authority": func() error {
			_, err := plugin.VerifySignature(root, b.ManifestBytes, other.Sign(b.ManifestBytes, signer, issuer), time.Now())
			return err
		},
		"entry from the future": func() error {
			_, err := plugin.VerifySignature(root, b.ManifestBytes, b.Signature, time.Now().Add(-time.Hour))
			return err
		},
		"tampered entry timestamp": func() error {
			bad := bytes.Replace(b.Signature, []byte(`"integratedTime":"`), []byte(`"integratedTime":"1`), 1)
			_, err := plugin.VerifySignature(root, b.ManifestBytes, bad, time.Now())
			return err
		},
	}
	for name, f := range cases {
		if f() == nil {
			t.Errorf("%s: verified", name)
		}
	}
	// The certificate was valid ten minutes; a log entry outside it fails.
	late := plugintest.NewSigstore()
	late.Now = time.Now().Add(-48 * time.Hour)
	lateRoot, _ := plugin.ParseTrustedRoot(late.TrustedRoot())
	if _, err := plugin.VerifySignature(lateRoot, b.ManifestBytes, late.Sign(b.ManifestBytes, signer, issuer), time.Now()); err == nil {
		t.Error("a certificate issued before the authority was valid verified")
	}
}

func TestTamperedFileRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "b")
	plugintest.Write(dir, sample())
	if err := os.WriteFile(filepath.Join(dir, "hooks", "no-rm.sh"), []byte("#!/bin/sh\nexit 0 # changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Open(dir); err == nil || !strings.Contains(err.Error(), "does not match its sha256") {
		t.Fatalf("a changed file opened: %v", err)
	}
	// An unlisted file is refused too.
	plugintest.Write(dir, sample())
	if err := os.WriteFile(filepath.Join(dir, "agents", "extra.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Open(dir); err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Fatalf("an unlisted file opened: %v", err)
	}
}

func TestLinksAndEscapesRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "b")
	plugintest.Write(dir, sample())
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "docs", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Open(dir); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a folder with a link opened: %v", err)
	}
	for name, hdr := range map[string]*tar.Header{
		"symlink": {Name: "docs/x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"escape":  {Name: "../evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
		"abs":     {Name: "/etc/evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
	} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
		_ = tw.Close()
		_ = gz.Close()
		if _, err := plugin.ReadArchive(&buf); err == nil {
			t.Errorf("%s: archive read", name)
		}
	}
}

func TestUnderDeclaredCapabilitiesRefused(t *testing.T) {
	spec := sample()
	spec.Capabilities = &plugin.Capabilities{Tools: []string{"read", "grep"}, Hooks: []string{"tool_call"}, MCPServers: []string{"tickets"}}
	dir := filepath.Join(t.TempDir(), "b")
	plugintest.Write(dir, spec)
	if _, err := plugin.Open(dir); err == nil || !strings.Contains(err.Error(), "egress tickets.acme.example") {
		t.Fatalf("a bundle hiding its egress opened: %v", err)
	}
}

func TestManifestStrict(t *testing.T) {
	for name, m := range map[string]string{
		"unknown key":    `{"schema":1,"name":"a","version":"1.0.0","publisher":"p","files":[],"capabilities":{},"permissions":{"allow":["bash"]}}`,
		"bad name":       `{"schema":1,"name":"A B","version":"1.0.0","publisher":"p","files":[],"capabilities":{}}`,
		"outside folder": `{"schema":1,"name":"a","version":"1.0.0","publisher":"p","files":[{"path":".abhed/config.json","sha256":"` + strings.Repeat("0", 64) + `"}],"capabilities":{}}`,
		"dot dot":        `{"schema":1,"name":"a","version":"1.0.0","publisher":"p","files":[{"path":"docs/../../x","sha256":"` + strings.Repeat("0", 64) + `"}],"capabilities":{}}`,
		"bad event":      `{"schema":1,"name":"a","version":"1.0.0","publisher":"p","files":[],"hooks":[{"name":"h","events":["approve"],"command":"x"}],"capabilities":{}}`,
	} {
		if _, err := plugin.ParseManifest([]byte(m)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestPolicyAdmit(t *testing.T) {
	s := &plugin.Signer{Identity: signer, Issuer: issuer}
	mine := []plugin.Publisher{{Identity: signer, Issuer: issuer}}
	d := strings.Repeat("a", 64)
	cases := []struct {
		name  string
		p     plugin.Policy
		e     plugin.Evidence
		mine  []plugin.Publisher
		basis string
	}{
		{"signed by my publisher", plugin.Policy{}, plugin.Evidence{Digest: d, Signer: s}, mine, plugin.BasisSignature},
		{"signed, publisher not mine", plugin.Policy{}, plugin.Evidence{Digest: d, Signer: s}, nil, ""},
		{"unsigned, no pin", plugin.Policy{}, plugin.Evidence{Digest: d}, nil, ""},
		{"unsigned, pinned", plugin.Policy{}, plugin.Evidence{Digest: d, Pin: d}, nil, plugin.BasisPin},
		{"unsigned, wrong pin", plugin.Policy{}, plugin.Evidence{Digest: d, Pin: strings.Repeat("b", 64)}, nil, ""},
		{"disabled", plugin.Policy{Disabled: true}, plugin.Evidence{Digest: d, Signer: s}, mine, ""},
		{"signature required, pinned", plugin.Policy{RequireSignature: true}, plugin.Evidence{Digest: d, Pin: d}, nil, ""},
		{"org publishers, mine ignored", plugin.Policy{AllowedPublishers: []plugin.Publisher{{Identity: "other@x", Issuer: issuer}}},
			plugin.Evidence{Digest: d, Signer: s}, mine, ""},
		{"org publisher prefix", plugin.Policy{AllowedPublishers: []plugin.Publisher{{Identity: "release@*", Issuer: issuer}}},
			plugin.Evidence{Digest: d, Signer: s}, nil, plugin.BasisSignature},
		{"org lists, user pin not enough", plugin.Policy{AllowedBundles: []string{strings.Repeat("c", 64)}}, plugin.Evidence{Digest: d, Pin: d}, nil, ""},
		{"org bundle pin", plugin.Policy{AllowedBundles: []string{d}}, plugin.Evidence{Digest: d}, nil, plugin.BasisOrgPin},
		{"org source", plugin.Policy{Sources: []plugin.Source{{Name: "acme", URL: "https://x/v1/plugins/index"}}}, plugin.Evidence{Digest: d, Source: "acme"}, nil, plugin.BasisSource},
		{"source no longer listed", plugin.Policy{}, plugin.Evidence{Digest: d, Source: "acme"}, nil, ""},
	}
	for _, c := range cases {
		basis, err := c.p.Admit(c.e, c.mine)
		if basis != c.basis || (c.basis == "") != (err != nil) {
			t.Errorf("%s: basis %q err %v, want %q", c.name, basis, err, c.basis)
		}
	}
	p := plugin.Policy{Sources: []plugin.Source{{Name: "acme", URL: "https://abhed.acme.example/v1/plugins/index"}}}
	if p.SourceFor("https://abhed.acme.example/v1/plugins/bundles/x.tgz") != "acme" ||
		p.SourceFor("https://evil.example/v1/plugins/x.tgz") != "" || p.SourceFor("http://abhed.acme.example/v1/plugins/x.tgz") != "" {
		t.Error("SourceFor matched wrongly")
	}
}

func TestStoreLifecycle(t *testing.T) {
	home := t.TempDir()
	s := &plugin.Store{Root: filepath.Join(home, "plugins"), StateFile: filepath.Join(home, "plugins.json")}
	src := filepath.Join(t.TempDir(), "v1")
	plugintest.Write(src, sample())
	b, err := plugin.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Read()
	if _, err := s.Stage(st, b, plugin.Install{Basis: plugin.BasisPin, Pin: b.Digest}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(st); err != nil {
		t.Fatal(err)
	}
	// Pending content never loads.
	if got, _ := plugin.LoadAll(s, plugin.Policy{}, "1.3.0"); len(got) != 0 {
		t.Fatalf("pending content loaded: %+v", got)
	}
	st, _ = s.Read()
	if _, _, err := s.Approve(st, "acme-review", strings.Repeat("f", 64)); err == nil {
		t.Fatal("approved with the wrong digest")
	}
	if _, _, err := s.Approve(st, "acme-review", b.Digest); err != nil {
		t.Fatal(err)
	}
	_ = s.Write(st)
	got, warns := plugin.LoadAll(s, plugin.Policy{}, "1.3.0")
	if len(got) != 1 || len(warns) != 0 {
		t.Fatalf("approved plugin: %+v %v", got, warns)
	}
	l := got[0]
	if l.Hooks[0].Name != "acme-review/no-rm" || !strings.Contains(l.Hooks[0].Command, "'"+l.Root+"'/hooks/no-rm.sh") ||
		l.MCPServers[0].Name != "acme-review-tickets" || len(l.Agents) != 1 || len(l.Skills) != 1 || len(l.Commands) != 1 {
		t.Fatalf("loaded = %+v", l)
	}
	// Policy applies at load: disabled loads nothing.
	if got, _ := plugin.LoadAll(s, plugin.Policy{Disabled: true}, "1.3.0"); len(got) != 0 {
		t.Fatal("loaded under plugins.disabled")
	}
	if got, w := plugin.LoadAll(s, plugin.Policy{RequireSignature: true}, "1.3.0"); len(got) != 0 || len(w) != 1 {
		t.Fatal("an unsigned plugin loaded under require_signature")
	}
	// A file changed on disk after approval stops the plugin loading.
	hook := filepath.Join(l.Root, "hooks", "no-rm.sh")
	orig, _ := os.ReadFile(hook)
	_ = os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n#x\n"), 0o600)
	if got, w := plugin.LoadAll(s, plugin.Policy{}, "1.3.0"); len(got) != 0 || len(w) != 1 || !strings.Contains(w[0], "sha256") {
		t.Fatalf("a tampered plugin loaded: %v", w)
	}
	_ = os.WriteFile(hook, orig, 0o600)

	// Upgrade: staged beside the approved one, which keeps loading.
	spec := sample()
	spec.Version = "1.1.0"
	spec.Files["agents/second.md"] = "---\nname: acme-second\ndescription: d\n---\nx\n"
	src2 := filepath.Join(t.TempDir(), "v2")
	plugintest.Write(src2, spec)
	b2, _ := plugin.Open(src2)
	st, _ = s.Read()
	if _, err := s.Stage(st, b2, plugin.Install{Basis: plugin.BasisPin, Pin: b2.Digest}); err != nil {
		t.Fatal(err)
	}
	_ = s.Write(st)
	if got, _ := plugin.LoadAll(s, plugin.Policy{}, "1.3.0"); len(got) != 1 || got[0].Version != "1.0.0" {
		t.Fatalf("during an upgrade the approved version must keep loading: %+v", got)
	}
	diff := strings.Join(plugin.Diff(b, b2), "\n")
	if !strings.Contains(diff, "1.0.0 -> 1.1.0") || !strings.Contains(diff, "+ agents/second.md") {
		t.Fatalf("diff:\n%s", diff)
	}
	st, _ = s.Read()
	if _, _, err := s.Approve(st, "acme-review", ""); err != nil {
		t.Fatal(err)
	}
	_ = s.Write(st)
	if _, err := os.Stat(l.Root); !os.IsNotExist(err) {
		t.Fatal("the old version's folder was left behind")
	}
	// Uninstall removes every file and the entry.
	st, _ = s.Read()
	if _, err := s.Remove(st, "acme-review"); err != nil {
		t.Fatal(err)
	}
	_ = s.Write(st)
	if _, err := os.Stat(filepath.Join(s.Root, "acme-review")); !os.IsNotExist(err) {
		t.Fatal("files left after remove")
	}
	if st, _ := s.Read(); len(st.Plugins) != 0 {
		t.Fatal("entry left after remove")
	}
}

func TestVersionGate(t *testing.T) {
	if !plugin.AtLeast("1.3.0", "1.3.0") || plugin.AtLeast("1.2.9", "1.3.0") || !plugin.AtLeast("dev", "9.9.9") || !plugin.AtLeast("v1.10.0", "1.9.9") {
		t.Fatal("AtLeast")
	}
}
