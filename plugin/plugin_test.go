package plugin_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// A publisher's .git folder beside the bundle is not part of it.
func TestDotFoldersLeftOut(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "b")
	plugintest.Write(dir, sample())
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := plugin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for p := range b.Files {
		if strings.HasPrefix(p, ".git") {
			t.Fatalf("%s was read", p)
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
	other := &plugin.Signer{Identity: "other@acme.example", Issuer: issuer}
	mine := []plugin.Publisher{{Identity: signer, Issuer: issuer}}
	d, x := strings.Repeat("a", 64), strings.Repeat("c", 64)
	pubs := func(ids ...string) []plugin.Publisher {
		var out []plugin.Publisher
		for _, id := range ids {
			out = append(out, plugin.Publisher{Identity: id, Issuer: issuer})
		}
		return out
	}
	managedPubs := plugin.AllowList{Layer: "managed", Publishers: pubs(signer), Grants: true}
	managedBundles := plugin.AllowList{Layer: "managed", Bundles: []string{x}, Grants: true}
	orgPubs := func(grants bool, ids ...string) plugin.AllowList {
		return plugin.AllowList{Layer: "org", Publishers: pubs(ids...), Grants: grants}
	}
	orgBundles := func(grants bool, ds ...string) plugin.AllowList {
		return plugin.AllowList{Layer: "org", Bundles: ds, Grants: grants}
	}
	src := []plugin.Source{{Name: "acme", URL: "https://x/v1/plugins/index", Trusted: true}}
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
		{"signature required, signed by a stranger, pinned", plugin.Policy{RequireSignature: true}, plugin.Evidence{Digest: d, Signer: other, Pin: d}, nil, ""},

		// The machine's managed lists grant.
		{"managed publisher, nobody of mine", plugin.Policy{Lists: []plugin.AllowList{managedPubs}}, plugin.Evidence{Digest: d, Signer: s}, nil, plugin.BasisSignature},
		{"managed publisher list, my own other publisher", plugin.Policy{Lists: []plugin.AllowList{managedPubs}}, plugin.Evidence{Digest: d, Signer: other},
			pubs("other@acme.example"), ""},
		{"managed bundle digest", plugin.Policy{Lists: []plugin.AllowList{managedBundles}}, plugin.Evidence{Digest: x}, nil, plugin.BasisOrgPin},
		{"managed bundles, my pin of another", plugin.Policy{Lists: []plugin.AllowList{managedBundles}}, plugin.Evidence{Digest: d, Pin: d}, nil, ""},

		// Managed publishers and org bundles: a bundle must pass both.
		{"managed publishers + org bundles, signed, not listed", plugin.Policy{Lists: []plugin.AllowList{managedPubs, orgBundles(false, x)}},
			plugin.Evidence{Digest: d, Signer: s}, nil, ""},
		{"managed publishers + org bundles, listed, unsigned", plugin.Policy{Lists: []plugin.AllowList{managedPubs, orgBundles(false, x)}},
			plugin.Evidence{Digest: x}, nil, ""},
		{"managed publishers + org bundles, both", plugin.Policy{Lists: []plugin.AllowList{managedPubs, orgBundles(false, x)}},
			plugin.Evidence{Digest: x, Signer: s}, nil, plugin.BasisSignature},
		// Managed bundles and org publishers: both, the other way round.
		{"managed bundles + org publishers, org signer, not listed", plugin.Policy{Lists: []plugin.AllowList{managedBundles, orgPubs(true, signer)}},
			plugin.Evidence{Digest: d, Signer: s}, nil, ""},
		{"managed bundles + org publishers, both", plugin.Policy{Lists: []plugin.AllowList{managedBundles, orgPubs(true, signer)}},
			plugin.Evidence{Digest: x, Signer: s}, nil, plugin.BasisSignature},

		// An org whose keys are not pinned only narrows.
		{"unpinned org publisher, nobody of mine", plugin.Policy{Lists: []plugin.AllowList{orgPubs(false, signer)}}, plugin.Evidence{Digest: d, Signer: s}, nil, ""},
		{"unpinned org publisher, mine too", plugin.Policy{Lists: []plugin.AllowList{orgPubs(false, signer)}}, plugin.Evidence{Digest: d, Signer: s}, mine, plugin.BasisSignature},
		{"unpinned org publisher, my other publisher", plugin.Policy{Lists: []plugin.AllowList{orgPubs(false, signer)}}, plugin.Evidence{Digest: d, Signer: other},
			pubs("other@acme.example"), ""},
		{"unpinned org bundles, unsigned, listed, no pin", plugin.Policy{Lists: []plugin.AllowList{orgBundles(false, d)}}, plugin.Evidence{Digest: d}, nil, ""},
		{"unpinned org bundles, unsigned, listed, my pin", plugin.Policy{Lists: []plugin.AllowList{orgBundles(false, d)}}, plugin.Evidence{Digest: d, Pin: d}, nil, plugin.BasisPin},
		{"unpinned org bundles, my pin of another", plugin.Policy{Lists: []plugin.AllowList{orgBundles(false, x)}}, plugin.Evidence{Digest: d, Pin: d}, nil, ""},
		// Pinned org keys grant, as the managed file does.
		{"pinned org publisher, nobody of mine", plugin.Policy{Lists: []plugin.AllowList{orgPubs(true, signer)}}, plugin.Evidence{Digest: d, Signer: s}, nil, plugin.BasisSignature},
		{"pinned org bundle", plugin.Policy{Lists: []plugin.AllowList{orgBundles(true, d)}}, plugin.Evidence{Digest: d}, nil, plugin.BasisOrgPin},

		// Sources.
		{"trusted source", plugin.Policy{Sources: src}, plugin.Evidence{Digest: d, Source: "acme"}, nil, plugin.BasisSource},
		{"untrusted source only narrows", plugin.Policy{Sources: []plugin.Source{{Name: "acme", URL: "https://x/i"}}}, plugin.Evidence{Digest: d, Source: "acme"}, nil, ""},
		{"source no longer listed", plugin.Policy{}, plugin.Evidence{Digest: d, Source: "acme"}, nil, ""},
		{"source, but a managed publisher list", plugin.Policy{Sources: src, Lists: []plugin.AllowList{managedPubs}}, plugin.Evidence{Digest: d, Source: "acme"}, nil, ""},
		{"signed by a stranger, from a trusted source", plugin.Policy{Sources: src}, plugin.Evidence{Digest: d, Signer: other, Source: "acme"}, nil, plugin.BasisSource},
		// What a trusted source vouched for at install, kept for that digest.
		{"source approval, signed, require_signature", plugin.Policy{RequireSignature: true}, plugin.Evidence{Digest: d, Signer: other, SourcePin: d}, nil, plugin.BasisSource},
		{"source approval, unsigned, require_signature", plugin.Policy{RequireSignature: true}, plugin.Evidence{Digest: d, SourcePin: d}, nil, ""},
		{"source approval of another digest", plugin.Policy{}, plugin.Evidence{Digest: d, SourcePin: x}, nil, ""},
		{"source approval, a managed list it fails", plugin.Policy{Lists: []plugin.AllowList{managedPubs}}, plugin.Evidence{Digest: d, Signer: other, SourcePin: d}, nil, ""},
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

// '*' ends a URI identity after '/' or '@' only, and never an email.
func TestPublisherWildcard(t *testing.T) {
	gh := "https://token.actions.githubusercontent.com"
	for _, c := range []struct {
		identity string
		valid    bool
		matches  []string
		not      []string
	}{
		{"https://github.com/acme/plugins/.github/workflows/release.yml@*", true,
			[]string{"https://github.com/acme/plugins/.github/workflows/release.yml@refs/tags/v1.2.0"},
			[]string{"https://github.com/acme/plugins/.github/workflows/release.yml.evil@x"}},
		{"https://github.com/acme/*", true, []string{"https://github.com/acme/plugins/x"}, []string{"https://github.com/acme-evil/x"}},
		{"https://github.com/acme*", false, nil, []string{"https://github.com/acme-evil/x"}},
		{"https://github.com/*", false, nil, nil},
		{"release@acme*", false, nil, []string{"release@acme.evil.example"}},
		{"release@*", false, nil, nil},
		{"*", false, nil, nil},
		{"release@acme.example", true, []string{"release@acme.example"}, []string{"release@acme.example.evil"}},
	} {
		p := plugin.Publisher{Identity: c.identity, Issuer: gh}
		if (p.Valid() == nil) != c.valid {
			t.Errorf("%s: valid %v, want %v", c.identity, p.Valid(), c.valid)
		}
		for _, id := range c.matches {
			if !p.Matches(plugin.Signer{Identity: id, Issuer: gh}) {
				t.Errorf("%s does not match %s", c.identity, id)
			}
		}
		for _, id := range c.not {
			if p.Matches(plugin.Signer{Identity: id, Issuer: gh}) {
				t.Errorf("%s matches %s", c.identity, id)
			}
		}
	}
	if (plugin.Publisher{Identity: "release@acme.example", Issuer: "http://issuer"}).Valid() == nil {
		t.Error("an http issuer is valid")
	}
}

// A server's headers_env is a capability the approval shows, and a
// credential's name is never one.
func TestHeadersEnv(t *testing.T) {
	spec := sample()
	spec.MCPServers[0].HeadersEnv = map[string]string{"Authorization": "ACME_TICKETS_HEADER"}
	dir := filepath.Join(t.TempDir(), "b")
	plugintest.Write(dir, spec)
	b, err := plugin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(plugin.Describe(b), "\n")
	if !strings.Contains(out, "header Authorization <- $ACME_TICKETS_HEADER") || !strings.Contains(out, "env      ACME_TICKETS_HEADER") {
		t.Fatalf("the approval does not show the header:\n%s", out)
	}
	// Under-declared: the manifest must request the variable.
	spec.Capabilities = &plugin.Capabilities{Tools: []string{"grep", "read"}, Egress: []string{"tickets.acme.example"}, Hooks: []string{"tool_call"}, MCPServers: []string{"tickets"}}
	plugintest.Write(dir, spec)
	if _, err := plugin.Open(dir); err == nil || !strings.Contains(err.Error(), "env ACME_TICKETS_HEADER") {
		t.Fatalf("an undeclared env opened: %v", err)
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "ABHED_SESSION", "OPENAI_API_KEY", "MY_PASSWORD",
		"GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_ID", "SOME_SECRET", "CLAUDE_CODE_OAUTH_TOKEN", "acme_api_key"} {
		if !plugin.SensitiveEnv(name) {
			t.Errorf("%s is not sensitive", name)
		}
		m := `{"schema":1,"name":"a","version":"1.0.0","publisher":"p","files":[],"capabilities":{"env":["` + name + `"],"mcp_servers":["s"],"egress":["x.example"]},` +
			`"mcp_servers":[{"name":"s","url":"https://x.example/mcp","headers_env":{"Authorization":"` + name + `"}}]}`
		if _, err := plugin.ParseManifest([]byte(m)); err == nil {
			t.Errorf("headers_env %s was taken", name)
		}
	}
	// An upgrade that adds one says so.
	old, _ := plugin.Open(writeSample(t, sample()))
	diff := strings.Join(plugin.Diff(old, b), "\n")
	if !strings.Contains(diff, "+ requests env ACME_TICKETS_HEADER") || !strings.Contains(diff, "header Authorization <- $ACME_TICKETS_HEADER") {
		t.Fatalf("the diff hides the header:\n%s", diff)
	}
}

func writeSample(t *testing.T, spec plugintest.Spec) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "s")
	plugintest.Write(dir, spec)
	return dir
}

// A manifest string a terminal would not print as itself is refused, and
// what Describe prints is escaped all the same.
func TestTerminalEscapesRefused(t *testing.T) {
	for _, m := range []string{
		`{"schema":1,"name":"a","version":"1.0.0","publisher":"p","description":"fine\u001b[1A\u001b[2Khook gone","files":[],"capabilities":{}}`,
		`{"schema":1,"name":"a","version":"1.0.0","publisher":"p\r","files":[],"capabilities":{}}`,
		`{"schema":1,"name":"a","version":"1.0.0","publisher":"p","files":[],"capabilities":{"hooks":["stop"]},"hooks":[{"name":"h","events":["stop"],"command":"true\u001b[2K"}]}`,
		`{"schema":1,"name":"a","version":"1.0.0","publisher":"\u202eevil","files":[],"capabilities":{}}`,
	} {
		if _, err := plugin.ParseManifest([]byte(m)); err == nil || !strings.Contains(err.Error(), "does not print as itself") {
			t.Errorf("taken: %s (%v)", m, err)
		}
	}
	b := &plugin.Bundle{Manifest: &plugin.Manifest{Schema: 1, Name: "a", Version: "1.0.0", Publisher: "p\x1b[2K",
		Description: "d\x1b[1A", Hooks: []plugin.Hook{{Name: "h", Events: []string{"stop"}, Command: "x\x1b[2Ky"}}}, Files: map[string][]byte{}}
	for _, l := range plugin.Describe(b) {
		if strings.ContainsRune(l, 0x1b) {
			t.Fatalf("an escape reached the terminal: %q", l)
		}
	}
}

// Two paths a case-folding file system takes for one are refused.
func TestCaseClashRefused(t *testing.T) {
	spec := sample()
	spec.Files["docs/A/x.md"] = "x\n"
	spec.Files["docs/a/y.md"] = "y\n"
	dir := filepath.Join(t.TempDir(), "b")
	if err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		plugintest.Write(dir, spec)
		return nil
	}(); err != nil {
		if !strings.Contains(err.Error(), "folds case") {
			t.Fatal(err)
		}
		return
	}
	if _, err := plugin.Open(dir); err == nil || !strings.Contains(err.Error(), "folds case") {
		t.Fatalf("a case clash opened: %v", err)
	}
}

// An archive that inflates past the bound is refused while reading.
func TestArchiveInflationBounded(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Many directory entries with PAX padding: no file, all decompressed bytes.
	pad := strings.Repeat("p", 60000)
	for i := 0; i < 1500; i++ {
		_ = tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("docs/d%d/", i), Typeflag: tar.TypeDir, Mode: 0o700,
			PAXRecords: map[string]string{"comment": pad}, Format: tar.FormatPAX})
	}
	_ = tw.Close()
	_ = gz.Close()
	if _, err := plugin.ReadArchive(&buf); err == nil || !strings.Contains(err.Error(), "decompresses to more than") {
		t.Fatalf("an inflating archive was read: %v", err)
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
	if got, _ := plugin.LoadAll(s, plugin.Policy{}, "1.3.0", plugin.Roots{}); len(got) != 0 {
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
	got, warns := plugin.LoadAll(s, plugin.Policy{}, "1.3.0", plugin.Roots{})
	if len(got) != 1 || len(warns) != 0 {
		t.Fatalf("approved plugin: %+v %v", got, warns)
	}
	l := got[0]
	if l.Hooks[0].Name != "acme-review/no-rm" || !strings.Contains(l.Hooks[0].Command, "'"+l.Root+"'/hooks/no-rm.sh") ||
		l.MCPServers[0].Name != "acme-review-tickets" || len(l.Agents) != 1 || len(l.Skills) != 1 || len(l.Commands) != 1 {
		t.Fatalf("loaded = %+v", l)
	}
	// Policy applies at load: disabled loads nothing.
	if got, _ := plugin.LoadAll(s, plugin.Policy{Disabled: true}, "1.3.0", plugin.Roots{}); len(got) != 0 {
		t.Fatal("loaded under plugins.disabled")
	}
	if got, w := plugin.LoadAll(s, plugin.Policy{RequireSignature: true}, "1.3.0", plugin.Roots{}); len(got) != 0 || len(w) != 1 {
		t.Fatal("an unsigned plugin loaded under require_signature")
	}
	// A file changed on disk after approval stops the plugin loading.
	hook := filepath.Join(l.Root, "hooks", "no-rm.sh")
	orig, _ := os.ReadFile(hook)
	_ = os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n#x\n"), 0o600)
	if got, w := plugin.LoadAll(s, plugin.Policy{}, "1.3.0", plugin.Roots{}); len(got) != 0 || len(w) != 1 || !strings.Contains(w[0], "sha256") {
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
	if got, _ := plugin.LoadAll(s, plugin.Policy{}, "1.3.0", plugin.Roots{}); len(got) != 1 || got[0].Version != "1.0.0" {
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

// The best-effort list of names a plugin never sends.
func TestSensitiveEnvNames(t *testing.T) {
	for _, n := range []string{"DB_PASS", "AZ_PWD", "GITLAB_PAT", "CI_JOB_JWT", "SENTRY_DSN", "SLACK_WEBHOOK_URL", "DATABASE_URL",
		"CONN_STR", "MY_CONNECTION_STRING", "AUTH_HEADER", "BEARER", "NETRC", "REDIS_URL", "APP_DB_URL", "X_PASSPHRASE"} {
		if !plugin.SensitiveEnv(n) {
			t.Errorf("%s passes", n)
		}
	}
	for _, n := range []string{"ACME_TICKETS_HEADER", "PATH_PREFIX", "TICKETS_URL", "PASSAGE_ID", "ACME_REGION"} {
		if plugin.SensitiveEnv(n) {
			t.Errorf("%s is refused", n)
		}
	}
}

// A signer identity that walks out of a prefix never matches it.
func TestIdentityNormalised(t *testing.T) {
	iss := "https://token.actions.githubusercontent.com"
	p := plugin.Publisher{Identity: "https://github.com/acme/*", Issuer: iss}
	for _, id := range []string{"https://github.com/acme/../evil/r", "https://github.com/acme/%2e%2e/evil", "https://github.com/acme/%2Fevil",
		"https://github.com/acme/./x", "https://github.com/acme//evil"} {
		if p.Matches(plugin.Signer{Identity: id, Issuer: iss}) {
			t.Errorf("%s matched", id)
		}
	}
	if !p.Matches(plugin.Signer{Identity: "https://github.com/acme/plugins/.github/workflows/r.yml@refs/tags/v1", Issuer: iss}) {
		t.Error("a plain identity did not match")
	}
	for _, id := range []string{"https://github.com/acme/../*", "https://github.com/acme/%2e/*"} {
		if (plugin.Publisher{Identity: id, Issuer: iss}).Valid() == nil {
			t.Errorf("%s is valid", id)
		}
	}
}

// A certificate naming two identities, two different issuers, or an issuer
// extension that does not read is refused.
func TestCertificateIdentityStrict(t *testing.T) {
	ss := plugintest.NewSigstore()
	root, _ := plugin.ParseTrustedRoot(ss.TrustedRoot())
	m := []byte("manifest")
	for name, o := range map[string]plugintest.CertOptions{
		"email and URI":    {Identities: []string{signer, "https://github.com/acme/x"}, Issuer: issuer},
		"two emails":       {Identities: []string{signer, "other@acme.example"}, Issuer: issuer},
		"v1 and v2 differ": {Identities: []string{signer}, Issuer: issuer, IssuerV1: "https://evil.example"},
		"malformed v2":     {Identities: []string{signer}, RawV2: []byte{0x0c, 0x05, 'a'}, IssuerV1: issuer},
		"no issuer at all": {Identities: []string{signer}, NoV2: true},
	} {
		if _, err := plugin.VerifySignature(root, m, ss.SignWith(m, o), time.Now()); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	for name, o := range map[string]plugintest.CertOptions{
		"v1 only":         {Identities: []string{signer}, NoV2: true, IssuerV1: issuer},
		"v1 and v2 agree": {Identities: []string{signer}, Issuer: issuer, IssuerV1: issuer},
	} {
		if s, err := plugin.VerifySignature(root, m, ss.SignWith(m, o), time.Now()); err != nil || s.Issuer != issuer {
			t.Errorf("%s: %v %+v", name, err, s)
		}
	}
}

// A log whose keyId is not its key's hash is left out of a trusted root.
func TestTrustedRootKeyIDBound(t *testing.T) {
	var r map[string]any
	_ = json.Unmarshal(plugintest.NewSigstore().TrustedRoot(), &r)
	r["tlogs"].([]any)[0].(map[string]any)["logId"] = map[string]any{"keyId": base64.StdEncoding.EncodeToString(make([]byte, 32))}
	b, _ := json.Marshal(r)
	// The log is left out; with no other, the root names no log at all.
	if _, err := plugin.ParseTrustedRoot(b); err == nil || !strings.Contains(err.Error(), "transparency log") {
		t.Fatalf("a mismatched keyId was taken: %v", err)
	}
	if !plugin.PublicGoodRootStale(plugin.PublicGoodRootSnapshot.Add(400*24*time.Hour)) || plugin.PublicGoodRootStale(plugin.PublicGoodRootSnapshot.Add(24*time.Hour)) {
		t.Fatal("staleness of the built-in root")
	}
	// The public-good root, with its Rekor v2 log left out, parses.
	if plugin.PublicGoodRoot() == nil {
		t.Fatal("no built-in root")
	}
}
