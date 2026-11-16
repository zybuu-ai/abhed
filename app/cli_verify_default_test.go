package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// -p labels its answer unless told not to: the result line carries the
// verdict's outcome and the unverified claim, and the exit code is the run's.
// -verify off and verify.mode off still turn it off.
func TestPrintModeLabelsByDefault(t *testing.T) {
	url, _ := textModel(t, "All 12 tests pass.")
	for _, c := range []struct {
		name  string
		extra string
		args  []string
		want  string // the verdict's mode, "" for none
	}{
		{"unset", "", nil, "label"},
		{"flag off", "", []string{"-verify", "off"}, ""},
		{"config off", `,"verify":{"mode":"off"}`, nil, ""},
		{"flag strict", "", []string{"-verify", "strict"}, "strict"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := bgWorkspace(t, url, c.extra)
			cmd := mainHelper(append([]string{"-C", ws, "-output-format", "json", "-p", "run the tests"}, c.args...))
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			_ = cmd.Run()
			res := resultOf(t, out.String())
			if c.want == "" {
				if res.Verdict != nil {
					t.Fatalf("checked although turned off: %+v", res.Verdict)
				}
				if res.ExitCode != 0 {
					t.Fatalf("exit %d\n%s", res.ExitCode, errOut.String())
				}
				return
			}
			if res.Verdict == nil || string(res.Verdict.Mode) != c.want || res.Verdict.Outcome != "unverified" {
				t.Fatalf("want a %s verdict with outcome unverified, got %+v\n%s", c.want, res.Verdict, out.String())
			}
			found := false
			for _, cl := range res.Verdict.Claims {
				found = found || cl.Verdict == "unverified" && strings.Contains(cl.Text, "12 tests pass")
			}
			if !found {
				t.Fatalf("the unverified claim is not in the result: %+v", res.Verdict.Claims)
			}
			// Labelling changes no exit code; strict fails closed.
			if want := map[string]int{"label": 0, "strict": 8}[c.want]; res.ExitCode != want {
				t.Fatalf("exit %d, want %d", res.ExitCode, want)
			}
		})
	}
}

// abhed rpc labels its answers unless told not to, as -p does.
func TestRPCLabelsByDefault(t *testing.T) {
	url, _ := textModel(t, "All 12 tests pass.")
	for _, c := range []struct {
		name, verify string
		want         bool
	}{
		{"unset", "", true},
		{"off", "off", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			managedConfig(t, "")
			ws := t.TempDir()
			cfg := `{"sandbox":{"min_tier":"none"},"model": {"default": "fake", "providers": {"fake": {"type": "openai-compatible",
				"base_url": "` + url + `", "model": "m"}}}}`
			if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
			start := `{"id":"1","method":"start"}`
			if c.verify != "" {
				start = `{"id":"1","method":"start","verify":"` + c.verify + `"}`
			}
			out := runRPC(t, ws, start, `{"id":"2","method":"prompt","prompt":"run the tests"}`, `{"id":"3","method":"quit"}`)
			var answer map[string]any
			sc := bufio.NewScanner(strings.NewReader(out))
			sc.Buffer(make([]byte, 1<<20), 8<<20)
			for sc.Scan() {
				var l map[string]any
				if json.Unmarshal(sc.Bytes(), &l) == nil && l["type"] == "answer" {
					answer = l
				}
			}
			if answer == nil {
				t.Fatalf("no answer:\n%s", out)
			}
			v, _ := answer["verdict"].(map[string]any)
			if got := v != nil && v["mode"] == "label" && v["outcome"] == "unverified"; got != c.want {
				t.Fatalf("verdict %v, want labelled %v", answer["verdict"], c.want)
			}
		})
	}
}
