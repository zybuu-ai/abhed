package plugin_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/plugin"
	"github.com/zybuu-ai/abhed/plugin/plugintest"
)

// realAt is a fixed time after the real bundle's log entry.
var realAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/sigstore/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A bundle the official tools made, against the public-good trusted root.
func TestRealSigstoreBundleVerifies(t *testing.T) {
	root, err := plugin.ParseTrustedRoot(readTestdata(t, "trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	artifact, bundle := readTestdata(t, "cosign_checksums.txt"), readTestdata(t, "cosign_checksums.txt.sigstore.json")
	s, err := plugin.VerifySignature(root, artifact, bundle, realAt)
	if err != nil {
		t.Fatalf("the real bundle did not verify: %v", err)
	}
	if s.Identity != "keyless@projectsigstore.iam.gserviceaccount.com" || s.Issuer != "https://accounts.google.com" {
		t.Fatalf("signer %+v", s)
	}
	// Against another root it does not.
	other, _ := plugin.ParseTrustedRoot(plugintest.NewSigstore().TrustedRoot())
	if _, err := plugin.VerifySignature(other, artifact, bundle, realAt); err == nil {
		t.Fatal("verified against another trusted root")
	}
	// Nor over other bytes.
	if _, err := plugin.VerifySignature(root, append(artifact, '\n'), bundle, realAt); err == nil {
		t.Fatal("verified over another artifact")
	}
}

// Each field of the real bundle changed on its own fails.
func TestRealSigstoreBundleTampered(t *testing.T) {
	root, _ := plugin.ParseTrustedRoot(readTestdata(t, "trusted_root.json"))
	artifact := readTestdata(t, "cosign_checksums.txt")
	orig := readTestdata(t, "cosign_checksums.txt.sigstore.json")
	edit := func(f func(b map[string]any, e map[string]any)) []byte {
		var b map[string]any
		if err := json.Unmarshal(orig, &b); err != nil {
			t.Fatal(err)
		}
		vm := b["verificationMaterial"].(map[string]any)
		e := vm["tlogEntries"].([]any)[0].(map[string]any)
		f(b, e)
		out, _ := json.Marshal(b)
		return out
	}
	flip := func(s string) string {
		raw, _ := base64.StdEncoding.DecodeString(s)
		raw[len(raw)/2] ^= 0x01
		return base64.StdEncoding.EncodeToString(raw)
	}
	cases := map[string]struct {
		bundle []byte
		want   string
	}{
		"body hash": {edit(func(_ map[string]any, e map[string]any) {
			body, _ := base64.StdEncoding.DecodeString(e["canonicalizedBody"].(string))
			body = []byte(strings.Replace(string(body), `"value":"e`, `"value":"f`, 1))
			e["canonicalizedBody"] = base64.StdEncoding.EncodeToString(body)
		}), "signed entry timestamp"},
		"signed entry timestamp": {edit(func(_ map[string]any, e map[string]any) {
			p := e["inclusionPromise"].(map[string]any)
			p["signedEntryTimestamp"] = flip(p["signedEntryTimestamp"].(string))
		}), "signed entry timestamp"},
		"log index":       {edit(func(_ map[string]any, e map[string]any) { e["logIndex"] = "3341269698" }), "signed entry timestamp"},
		"integrated time": {edit(func(_ map[string]any, e map[string]any) { e["integratedTime"] = "1792519255" }), "signed entry timestamp"},
		"certificate": {edit(func(b map[string]any, _ map[string]any) {
			c := b["verificationMaterial"].(map[string]any)["certificate"].(map[string]any)
			c["rawBytes"] = flip(c["rawBytes"].(string))
		}), ""},
		"signature": {edit(func(b map[string]any, _ map[string]any) {
			m := b["messageSignature"].(map[string]any)
			m["signature"] = flip(m["signature"].(string))
		}), "another signature"},
		"message digest": {edit(func(b map[string]any, _ map[string]any) {
			d := b["messageSignature"].(map[string]any)["messageDigest"].(map[string]any)
			d["digest"] = flip(d["digest"].(string))
		}), "message digest"},
		"body kind": {edit(func(_ map[string]any, e map[string]any) {
			e["kindVersion"] = map[string]any{"kind": "intoto", "version": "0.0.1"}
		}), "not hashedrekord"},
		"Rekor v2 entry": {edit(func(_ map[string]any, e map[string]any) {
			delete(e, "inclusionPromise")
			e["kindVersion"] = map[string]any{"kind": "hashedrekord", "version": "0.0.2"}
		}), "Rekor v2"},
		"bundle v0.1": {edit(func(b map[string]any, _ map[string]any) {
			b["mediaType"] = "application/vnd.dev.sigstore.bundle+json;version=0.1"
		}), "not supported"},
		"cosign legacy bundle": {[]byte(`{"base64Signature":"MEUC","cert":"LS0t","rekorBundle":{"SignedEntryTimestamp":"MEUC","Payload":{}}}`), "legacy bundle"},
	}
	for name, c := range cases {
		_, err := plugin.VerifySignature(root, artifact, c.bundle, realAt)
		if err == nil {
			t.Errorf("%s: a tampered real bundle verified", name)
			continue
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
