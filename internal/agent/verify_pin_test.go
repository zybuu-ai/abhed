package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

// The regression pin: every corpus case by name and a hash of its text and
// steps, and every strict answer the reviews raised. ABHED_WRITE_VERIFY_PIN=1 re-pins.
type verifyPin struct {
	Corpus []pinCase   `json:"corpus"`
	Strict []pinStrict `json:"strict"`
}

type pinCase struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Hash string `json:"hash"`
}

// caseHash is a hash of what a case says and the calls it rests on.
func caseHash(c CorpusCase) string {
	b, _ := json.Marshal(struct {
		Kind, Want, Text string
		Claim            *Claim
		Steps            []CorpusStep
	}{c.Kind, c.Want, c.Text, c.Claim, c.Steps})
	return fmt.Sprintf("%x", sha256.Sum256(b))[:16]
}

type pinStrict struct {
	Scenario string `json:"scenario"`
	Reply    string `json:"reply"`
	Want     string `json:"want"`
}

const pinPath = "testdata/verify_pin.json"

func TestRegressionPin(t *testing.T) {
	raw, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatal(err)
	}
	var pin verifyPin
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatal(err)
	}
	rep := RunCorpus()
	got := map[string]CorpusResult{}
	for _, r := range rep.Results {
		got[r.Name] = r
	}
	if os.Getenv("ABHED_WRITE_VERIFY_PIN") == "1" {
		have := map[string]int{}
		for i, c := range pin.Corpus {
			have[c.Name] = i
		}
		for _, r := range rep.Results {
			if r.Kind != "planted" && r.Kind != "control" {
				continue
			}
			if i, ok := have[r.Name]; ok {
				pin.Corpus[i].Hash = caseHash(r.CorpusCase)
			} else {
				pin.Corpus = append(pin.Corpus, pinCase{Name: r.Name, Kind: r.Kind, Hash: caseHash(r.CorpusCase)})
			}
		}
		out, _ := json.MarshalIndent(pin, "", "  ")
		if err := os.WriteFile(pinPath, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range pin.Corpus {
		r, ok := got[c.Name]
		switch {
		case !ok:
			t.Errorf("pinned case %q is gone from the corpus", c.Name)
		case c.Kind != r.Kind || c.Hash != caseHash(r.CorpusCase):
			t.Errorf("pinned case %q changed (kind, text or steps); re-pin it on purpose with ABHED_WRITE_VERIFY_PIN=1", c.Name)
		case c.Kind == "planted" && (r.Got == ClaimVerified || !r.LoadBearing || r.Explains):
			t.Errorf("planted %q: %s (load-bearing %v, explanation %v): %s", c.Name, r.Got, r.LoadBearing, r.Explains, r.Reason)
		case c.Kind == "control" && r.Got != ClaimVerified:
			t.Errorf("control %q stopped verifying: %s", c.Name, r.Reason)
		}
	}
	for _, s := range pin.Strict {
		if reason := pinRun(t, s.Scenario, s.Reply); string(reason) != s.Want {
			t.Errorf("strict %s %q: ended %s, want %s", s.Scenario, s.Reply, reason, s.Want)
		}
	}
}

// pinRun runs one strict-mode answer after the calls its scenario makes,
// with the real read and edit tools.
func pinRun(t *testing.T, scenario, reply string) TerminalReason {
	t.Helper()
	sum := "package calc\n\nfunc Sum(nums []int) int {\n\ttotal := 0\n\tfor _, n := range nums {\n\t\tif n < 0 {\n\t\t\tcontinue\n\t\t}\n\t\ttotal += n\n\t}\n\treturn total\n}\n"
	plain := "package calc\n\n// Sum adds the numbers in nums.\nfunc Sum(nums []int) int {\n\ttotal := 0\n\tfor _, n := range nums {\n\t\ttotal += n\n\t}\n\treturn total\n}\n"
	type edit struct{ old, new string }
	scenarios := map[string]struct {
		file, text string
		edit       *edit
		bash       string
	}{
		"read-sum":     {file: "sum.go", text: sum},
		"explain-sum":  {file: "sum.go", text: plain},
		"add-continue": {file: "sum.go", text: plain, edit: &edit{"\t\ttotal += n\n", "\t\tif n < 0 {\n\t\t\tcontinue\n\t\t}\n\t\ttotal += n\n"}},
		"flip-debug":   {file: "app.env", text: "DEBUG=false\nCACHE=false\nTIMEOUT=30\n", edit: &edit{"DEBUG=false", "DEBUG=true"}},
		"routes-orders": {file: "routes.go", text: "package web\n\nfunc routes(r chi.Router) {\n\tr.Get(\"/admin\", admin)\n\tr.Get(\"/orders\", orders)\n}\n",
			edit: &edit{"\tr.Get(\"/orders\", orders)", "\tr.With(requireAuth).Get(\"/orders\", orders)"}},
		"run-12": {bash: "printf '12 tests passed\\n'"},
		"delete-panic": {file: "sum.go", text: "package calc\n\nfunc Sum(nums []int, max int) int {\n\ttotal := 0\n\tfor _, n := range nums {\n\t\ttotal += n\n\t\tif total > max {\n\t\t\tpanic(\"overflow\")\n\t\t}\n\t}\n\treturn total\n}\n\nfunc Product(nums []int) int {\n\tp := 1\n\tfor _, n := range nums {\n\t\tp *= n\n\t}\n\treturn p\n}\n",
			edit: &edit{"\t\tif total > max {\n\t\t\tpanic(\"overflow\")\n\t\t}\n", ""}},
		"routes-context": {file: "routes.go", text: "package web\n\nfunc routes(r chi.Router) {\n\tr.Get(\"/admin\", admin)\n\tr.Get(\"/orders\", orders)\n}\n",
			edit: &edit{"\tr.Get(\"/admin\", admin)\n\tr.Get(\"/orders\", orders)", "\tr.Get(\"/admin\", admin)\n\tr.With(requireAuth).Get(\"/orders\", orders)"}},
	}
	sc, ok := scenarios[scenario]
	if !ok {
		t.Fatalf("no scenario %q", scenario)
	}
	l, _, ad, dir := verifyHarness(t, VerifyStrict, false, nil)
	var turns []scriptedTurn
	if sc.file != "" {
		path := filepath.Join(dir, sc.file)
		if err := os.WriteFile(path, []byte(sc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, scriptedTurn{calls: []model.ToolCall{call("read", map[string]string{"path": path})}})
		if sc.edit != nil {
			turns = append(turns, scriptedTurn{calls: []model.ToolCall{call("edit", map[string]string{"path": path, "old_string": sc.edit.old, "new_string": sc.edit.new})}})
		}
	}
	if sc.bash != "" {
		turns = append(turns, scriptedTurn{calls: []model.ToolCall{call("bash", map[string]string{"command": sc.bash})}})
	}
	turns = append(turns, scriptedTurn{text: reply}, scriptedTurn{text: reply})
	ad.turns = turns
	reason, _ := l.Run(context.Background(), "do it")
	return reason
}

// Reports worded like advice or a plan are claims: a label before a colon,
// a step told in order, "I can confirm", "I couldn't find", a subject that
// starts like an imperative.
func TestReportsAreKept(t *testing.T) {
	for _, s := range []string{
		"Test results: 12 passed, 0 failed.", "Test results: all 12 tests passed.", "Run results: all 12 tests passed.",
		"I can confirm the tests pass.", "I can confirm all 12 tests pass.", "I can confirm `Sum` now skips negatives with `continue`.",
		"I couldn't find any references to `DEBUG` in the codebase.", "I could not find any other callers of `Sum`.",
		"I can't find any callers of `Sum` outside `sum.go`.",
		"Next, I set `DEBUG` to `true` in `config/app.env`.", "Next, I put `requireAuth` in front of `/admin`.",
		"Test coverage is now 92% for `Sum`.", "Test suite passes with `go test ./...`.",
		"You can see `Sum` now skips negatives with `continue`.", "You will notice `Sum` now skips negatives with `continue`.",
		"If you look at `sum.go`, `Sum` now skips negatives with `continue`.", "Check `sum.go`: `Sum` now skips negatives with `continue`.",
		"See `sum.go`: `Sum` now skips negatives with `continue`.", "Review the diff: `Sum` now skips negatives with `continue`.",
		"I'll note that `Sum` now skips negatives with `continue`.",
	} {
		if got := splitClaims(s); len(got) != 1 {
			t.Errorf("%q: kept %q, want one claim", s, got)
		}
	}
	for _, s := range []string{"Next, I would refactor `Sum` to use `range`.", "Run `go test ./...` to confirm.", "I'll update the docs next.",
		"Run `go test ./...` to confirm everything passes.", "Run `go test ./...` and check that all tests pass.",
		"Verify the build passes in CI.", "Use `go test -run TestSum` to see that it passes.",
		"Next steps: add tests for negatives and update the docs.", "Recommendation: add a test for negatives.",
		"Follow-up: add a test for the negative case.", "I'll point out the change in the release notes.",
		"Test it with `go test ./...`; it should pass.", "Check that `TestSum` passes after you pull.", "TODO: add a test for negatives."} {
		if got := splitClaims(s); len(got) != 0 {
			t.Errorf("%q: kept %q", s, got)
		}
	}
}
