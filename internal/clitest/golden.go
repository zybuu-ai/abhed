package clitest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/clitest/vt"
)

// update rewrites goldens: go test ./... -run X -update.
var update = func() *bool {
	if f := flag.Lookup("update"); f != nil {
		v := f.Value.String() == "true"
		return &v
	}
	return flag.Bool("update", false, "rewrite the clitest golden files")
}()

var (
	reDur    = regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|us|ms|s|m|h)(\d+(\.\d+)?(ms|s|m))*\b`)
	reTokens = regexp.MustCompile(`\b\d[\d,]*( (in|out|tokens?|cached|turns?)\b)`)
	reSHA    = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	reID     = regexp.MustCompile(`\bs-[0-9a-f]{24}\b`)
	reSpin   = regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]`)
	// The banner's tier depends on whether the sandbox was chosen by then.
	reTier = regexp.MustCompile(`(sandbox +)(process or stronger \(checking\)|(process|container|vm|none)\b)`)
)

// Normalize makes text stable across runs: the run's directories, times,
// token counts, ids, hashes and the spinner become placeholders.
func (h *run) Normalize(text string) string {
	p := h.placeholder()
	text = strings.NewReplacer(h.ws, "‹ws›", h.home, "‹home›", h.root, "‹tmp›",
		p+"/ws", "‹ws›", p+"/home", "‹home›", p, "‹tmp›").Replace(text)
	return NormalizeText(text)
}

// NormalizeText is Normalize without the run's paths.
func NormalizeText(text string) string {
	text = reSHA.ReplaceAllString(text, "‹sha›")
	text = reID.ReplaceAllString(text, "‹id›")
	text = reDur.ReplaceAllString(text, "‹dur›")
	text = reTokens.ReplaceAllString(text, "‹n›$1")
	text = reTier.ReplaceAllString(text, "${1}‹tier›")
	return reSpin.ReplaceAllString(text, "‹spin›")
}

// replay draws the run's output again with its directory replaced by a
// placeholder of the same width, so the screen is the same on every run.
func (h *run) replay() *vt.Snapshot {
	h.mu.Lock()
	raw := bytes.ReplaceAll(h.raw, []byte(h.root), []byte(h.placeholder()))
	chunks := append([]chunk(nil), h.chunks...)
	h.mu.Unlock()
	term := vt.New(h.o.Cols, h.o.Rows, nil)
	term.NoSync = h.o.NoSyncOutput
	for _, c := range chunks {
		if c.cols > 0 {
			term.Resize(c.cols, c.rows)
			continue
		}
		_, _ = term.Write(raw[c.off : c.off+c.n])
	}
	return term.Snapshot()
}

func (h *run) placeholder() string {
	return "/RUN" + strings.Repeat("_", max(0, len(h.root)-4))
}

// ScreenGolden is the final screen after the scrollback, normalized.
func (h *run) ScreenGolden() string {
	s := h.replay()
	var b strings.Builder
	for _, l := range s.Scrollback {
		b.WriteString(strings.TrimRight(l, " ") + "\n")
	}
	b.WriteString("──── screen ────\n")
	b.WriteString(strings.TrimRight(s.Text(), "\n") + "\n")
	return h.Normalize(b.String())
}

// AttrsGolden is each row's style runs: "row: col-col attrs" for every run
// of cells that is not in the default style.
func (h *run) AttrsGolden() string {
	s := screen{h.replay()}
	var b strings.Builder
	for y := 0; y < s.Rows(); y++ {
		var runs []string
		start, cur := -1, Attrs{}
		flush := func(end int) {
			if start >= 0 && cur != (Attrs{}) {
				runs = append(runs, fmt.Sprintf("%d-%d %s", start, end-1, attrString(cur)))
			}
		}
		for x := 0; x < s.Cols(); x++ {
			a := s.Cell(y, x).Attrs
			if x == 0 || a != cur {
				flush(x)
				start, cur = x, a
			}
		}
		flush(s.Cols())
		if len(runs) > 0 {
			fmt.Fprintf(&b, "%02d: %s\n", y, strings.Join(runs, ", "))
		}
	}
	return b.String()
}

func attrString(a Attrs) string {
	var p []string
	if a.FG != "" {
		p = append(p, "fg="+a.FG)
	}
	if a.BG != "" {
		p = append(p, "bg="+a.BG)
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{a.Bold, "bold"}, {a.Dim, "dim"}, {a.Italic, "italic"}, {a.Underline, "underline"}, {a.Reverse, "reverse"}} {
		if f.on {
			p = append(p, f.name)
		}
	}
	return strings.Join(p, " ")
}

// goldenKeys are the payload fields a record golden keeps.
var goldenKeys = []string{"from", "to", "by", "via", "scope", "op", "list", "rule", "name", "reason",
	"tool", "decision", "to_mode", "source", "kind", "access", "verdict", "event", "extension", "provider", "model"}

// RecordGolden reduces events to "type · actor · key fields", the form a
// record golden holds. Streaming fragments are left out: how text was cut
// into deltas is not governance.
func RecordGolden(evs []agent.Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Type == agent.EvAgentDelta || ev.Type == agent.EvAgentReasoningDelta {
			continue
		}
		line := string(ev.Type) + " · " + string(ev.Actor)
		var p map[string]any
		_ = json.Unmarshal(ev.Payload, &p)
		var kv []string
		for _, k := range goldenKeys {
			if v, ok := p[k]; ok {
				switch v := v.(type) {
				case string, float64, bool:
					kv = append(kv, fmt.Sprintf("%s:%v", k, v))
				}
			}
		}
		sort.Strings(kv)
		if len(kv) > 0 {
			line += " · " + strings.Join(kv, " ")
		}
		b.WriteString(NormalizeText(line) + "\n")
	}
	return b.String()
}

// Golden compares got with testdata/golden/<scenario>/<file>, or writes it
// under -update. Every golden is scanned first: see ScanGolden.
func Golden(t testing.TB, scenario, file, got string) {
	t.Helper()
	if problems := ScanGolden(got); len(problems) > 0 {
		t.Fatalf("clitest: golden %s/%s: %s", scenario, file, strings.Join(problems, "; "))
	}
	path := filepath.Join("testdata", "golden", scenario, file)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- testdata, committed to the repository
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { // #nosec G306 -- testdata, committed to the repository
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- a golden under the package's testdata
	if err != nil {
		t.Fatalf("clitest: %v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("clitest: %s differs from the golden (run with -update to accept):\n%s", path, diffLines(string(want), got))
	}
}

// AssertGoldens checks the screen and attribute goldens for a run, and the
// record golden when evs is not nil.
func (h *run) AssertGoldens(scenario string, evs []agent.Event) {
	h.t.Helper()
	Golden(h.t, scenario, "screen.golden", h.ScreenGolden())
	Golden(h.t, scenario, "attrs.golden", h.AttrsGolden())
	if evs != nil {
		Golden(h.t, scenario, "record.golden", RecordGolden(evs))
	}
}

func diffLines(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for i := 0; i < max(len(w), len(g)); i++ {
		var a, c string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			c = g[i]
		}
		if a != c {
			fmt.Fprintf(&b, "line %d:\n- %s\n+ %s\n", i+1, a, c)
		}
	}
	return b.String()
}

// CanaryPrefix starts every secret a test plants; none may reach a golden.
const CanaryPrefix = "abhed-canary-"

// otherProducts are other agent products and their memory files, as the
// first 16 hex digits of the sha256 of the lowercase word, so this file
// names none of them.
var otherProducts = map[string]bool{
	"c857d09db23e6822": true, "c70eca6b0f88f44d": true, "57de4cf40144bdf7": true, "3ea125d0bff386e6": true,
	"ebde709e306badca": true, "84829dbd81531188": true, "5d0c0ab127fdea24": true, "ead627e2b11190fd": true,
	"62f8e1ec095e1857": true, "eb4c0fbd9fe2bd9f": true, "97f128115e33c769": true, "5792d2981981be5a": true,
}

// ScanGolden lists what a golden may not hold: another product's name, an
// absolute home path, or a planted canary.
func ScanGolden(text string) []string {
	var out []string
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	seen := map[string]bool{}
	for _, w := range words {
		sum := sha256.Sum256([]byte(w))
		if h := hex.EncodeToString(sum[:8]); otherProducts[h] && !seen[h] {
			seen[h] = true
			out = append(out, "names another product")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" && strings.Contains(text, home) {
		out = append(out, "holds the home directory of the person running the tests")
	}
	for _, p := range []string{"/Users/", "/home/"} {
		if strings.Contains(text, p) {
			out = append(out, "holds an absolute home path")
			break
		}
	}
	if strings.Contains(text, CanaryPrefix) {
		out = append(out, "holds an unredacted canary")
	}
	return out
}
