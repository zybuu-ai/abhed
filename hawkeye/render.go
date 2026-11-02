package hawkeye

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

//go:embed report.html.tmpl
var reportTmpl string

// The report embeds tool output, which is untrusted by definition. html/template
// escapes by context, so a result containing markup or script renders as text;
// the page also loads nothing from anywhere, so it opens on an air-gapped host.
var page = template.Must(template.New("report").Funcs(template.FuncMap{
	"n":             commas,
	"ms":            dur,
	"pct":           func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"clock":         func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"chart":         chart,
	"steps":         steps,
	"remoteOutcome": remoteOutcome,
	"plan":          func(h string, n int) string { return strings.TrimSpace(planLabel(h, n)) },
	"codes":         codes,
	"join":          strings.Join,
	"latest": func(s []string) string {
		if len(s) == 0 {
			return ""
		}
		return s[len(s)-1]
	},
	"exit": func(p *int) string {
		if p == nil {
			return ""
		}
		return fmt.Sprintf("exit %d", *p)
	},
}).Parse(reportTmpl))

// HTML renders the report as one self-contained page.
func HTML(r Report) (string, error) {
	var b bytes.Buffer
	if err := page.Execute(&b, shown(r)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Text renders the report for a terminal.
func Text(r Report) string {
	r = shown(r)
	var b strings.Builder
	t := r.Totals
	fmt.Fprintf(&b, "HawkEYE · %s\n", r.SessionID)
	fmt.Fprintf(&b, "  %s\n\n", clip(r.Prompt, 100))
	fmt.Fprintf(&b, "  outcome   %s\n", r.Outcome)
	if len(r.Models) > 0 {
		fmt.Fprintf(&b, "  models    %s\n", strings.Join(r.Models, " → "))
	}
	fmt.Fprintf(&b, "  duration  %s  (model %s · tools %s)\n", dur(t.DurationMS), dur(t.ModelMS), dur(t.ToolMS))
	fmt.Fprintf(&b, "  turns     %d   tool calls %d   events %d\n", t.Turns, t.ToolCalls, t.Events)
	cache := fmt.Sprintf("%.0f%% cached", t.CacheHitRate*100)
	if !t.CacheReported {
		cache = "cache not reported"
	}
	fmt.Fprintf(&b, "  tokens    %s in · %s out · %s\n", commas(t.TokensIn), commas(t.TokensOut), cache)
	if t.Window > 0 {
		fmt.Fprintf(&b, "  context   peak %s of %s (%d%%)   compactions %d   offloads %d   recalls %d\n",
			commas(t.PeakContext), commas(t.Window), t.PeakContext*100/t.Window, len(r.Compactions), len(r.Offloads), t.Recalls)
	}
	fmt.Fprintf(&b, "  policy    %d allowed · %d denied · %d asked a reviewer   %s\n",
		r.Policy.Allowed, r.Policy.Denied, r.Policy.Reviewer, steps(r.Policy.ByStep))
	for _, l := range r.PolicyLayers {
		what := fmt.Sprintf("%d deny · %d ask · %d allow", len(l.Deny), len(l.Ask), len(l.Allow))
		if l.Removed {
			what = "removed"
		}
		fmt.Fprintf(&b, "  layer     %s%s at #%d: %s", l.Name, revision(l.Revision), l.Seq, what)
		if n := r.Policy.ByLayer[l.Name]; n > 0 {
			fmt.Fprintf(&b, "   decided %d call(s)", n)
		}
		if len(l.SetAside) > 0 {
			fmt.Fprintf(&b, "   %d allow rule(s) set aside under the managed configuration", len(l.SetAside))
		}
		b.WriteString("\n")
	}
	for _, t := range r.Toolchains {
		fmt.Fprintf(&b, "  toolchain #%d %s %s %s: %s", t.Seq, t.Lang, t.Version, t.Platform, t.Outcome)
		if t.Cached {
			b.WriteString(" (installed already)")
		} else if t.Digest != "" {
			fmt.Fprintf(&b, " (%s, %s)", clip(t.URL, 100), clip(t.Digest, 23))
		}
		if t.Error != "" {
			fmt.Fprintf(&b, " — %s", clip(t.Error, 160))
		}
		b.WriteString("\n")
	}
	for _, l := range r.LocalModel {
		fmt.Fprintf(&b, "  ollama    #%d %s %s: %s", l.Seq, l.Op, l.Model, l.Outcome)
		if l.Command != "" {
			fmt.Fprintf(&b, " (%s)", clip(l.Command, 120))
		}
		if l.Error != "" {
			fmt.Fprintf(&b, " — %s", clip(l.Error, 160))
		}
		b.WriteString("\n")
	}
	if n := r.Network; n != nil {
		fmt.Fprintf(&b, "  network   %d allowed · %d refused", n.Allowed, n.Denied)
		if n.WouldDeny > 0 {
			fmt.Fprintf(&b, " · %d would be refused (monitor)", n.WouldDeny)
		}
		b.WriteString("\n")
	}
	if s := r.Supervisor; s != nil {
		fmt.Fprintf(&b, "  supervisor %d reviewed · %d tightened · %d unavailable · %d corrections · %d re-plans   slowest %s",
			s.Reviewed, s.Tightened, s.Unavailable, s.Corrections, s.Replans, dur(s.MaxLatencyMS))
		if s.Unreviewed > 0 {
			fmt.Fprintf(&b, "   %d not reviewed (turn budget)", s.Unreviewed)
		}
		if len(s.Judges) > 0 {
			fmt.Fprintf(&b, "   judge %s", strings.Join(s.Judges, ", "))
		}
		b.WriteString("\n")
		if len(s.ByCode) > 0 {
			fmt.Fprintf(&b, "             %s\n", codes(s.ByCode))
		}
		for _, c := range s.Concerns {
			fmt.Fprintf(&b, "             #%d %s %s → %s  %s: %s\n", c.Seq, c.Tool, c.Before, c.After, c.Code, clip(oneLine(c.Rationale), 100))
		}
	}
	fmt.Fprintf(&b, "  record    #%d–#%d", r.Integrity.FirstSeq, r.Integrity.LastSeq)
	if len(r.Integrity.Gaps) > 0 {
		fmt.Fprintf(&b, "   GAPS at %v", r.Integrity.Gaps)
	} else {
		b.WriteString("   no gaps")
	}
	if len(r.Integrity.Omitted) > 0 {
		fmt.Fprintf(&b, "   agent.delta omitted by stream-json at %v", r.Integrity.Omitted)
	}
	b.WriteString("\n")

	if len(r.Findings) == 0 {
		b.WriteString("\n  No findings.\n")
	} else {
		fmt.Fprintf(&b, "\n  Findings (%d)\n", len(r.Findings))
		for _, f := range r.Findings {
			at := ""
			if f.Seq > 0 {
				at = fmt.Sprintf("  #%d", f.Seq)
			}
			fmt.Fprintf(&b, "  %-8s %s%s\n           %s\n", strings.ToUpper(string(f.Severity)), f.Title, at, f.Detail)
		}
	}

	if len(r.Answers) > 0 {
		b.WriteString("\n  Answers   ✓ verified · ? unverified · ✗ contradicted, against this session's record\n")
		for _, a := range r.Answers {
			fmt.Fprintf(&b, "  #%-4d %s (%s, %s)", a.Seq, a.Footer(), a.Mode, a.Source)
			if a.Revision != "" {
				b.WriteString(" · revision asked")
			}
			b.WriteString("\n")
			for _, c := range a.Claims {
				mark := "?"
				switch c.Verdict {
				case agent.ClaimVerified:
					mark = "✓"
				case agent.ClaimContradicted:
					mark = "✗"
				}
				fmt.Fprintf(&b, "    %s %-60s %s\n", mark, clip(oneLine(c.Text), 60), clip(oneLine(c.Reason), 90))
			}
		}
	}

	if len(r.RemoteChanges) > 0 {
		b.WriteString("\n  Remote changes\n")
		for _, t := range r.RemoteChanges {
			fmt.Fprintf(&b, "  %s %s\n", t.Kind, t.Name)
			for _, c := range t.Changes {
				fmt.Fprintf(&b, "    #%-4d %-9s %-15s %s%s\n", c.Seq, remoteOutcome(c), clip(c.Decision+byLabel(c.By), 15),
					clip(oneLine(c.What), 70), planLabel(c.PlanHash, c.PlanStep))
			}
		}
	}

	if len(r.Calls) > 0 {
		b.WriteString("\n  Calls   ✓ ran · ! failed, or refused by the sandbox or the network · ✗ denied · - not run\n")
		for _, c := range r.Calls {
			mark := "✓"
			switch {
			case c.Decision == "denied":
				mark = "✗"
			case !c.Ran:
				mark = "-"
			case c.IsError || c.SandboxDenied || c.NetworkDenied > 0:
				mark = "!"
			}
			fmt.Fprintf(&b, "  %s #%-4d %-6s %-52s %-8s %s\n", mark, c.Seq, c.Tool, clip(oneLine(c.Subject), 52), c.Step, dur(c.DurationMS))
			if c.Via != "" {
				fmt.Fprintf(&b, "           via %s\n", clip(oneLine(c.Via), 80))
			}
		}
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func steps(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, " · ")
}

func dur(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%02ds", ms/60_000, ms%60_000/1000)
	}
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// chart draws context occupancy per turn: one bar each, the cached share
// shaded, the window as a rule across the top. Built from numbers only, so
// returning it as trusted markup is safe.
func chart(r Report) template.HTML {
	if len(r.Turns) == 0 {
		return ""
	}
	const w, h, pad = 720.0, 150.0, 22.0
	top := float64(r.Totals.Window)
	if top == 0 {
		top = float64(r.Totals.PeakContext)
	}
	if top == 0 {
		return ""
	}
	bw := (w - pad) / float64(len(r.Turns))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="Context sent to the model on each turn, against the window">`, w, h+pad)
	fmt.Fprintf(&b, `<line x1="%.0f" y1="1" x2="%.0f" y2="1" class="lim"/>`, pad, w)
	for i, t := range r.Turns {
		x := pad + float64(i)*bw
		full := h * float64(t.TokensIn) / top
		cached := h * float64(t.TokensCached) / top
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="cold"><title>turn %d: %s tokens, %s cached</title></rect>`,
			x+1, h-full, max(bw-2, 1), full, t.N, commas(t.TokensIn), commas(t.TokensCached))
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="warm"/>`, x+1, h-cached, max(bw-2, 1), cached)
	}
	for _, c := range r.Compactions {
		for i, t := range r.Turns {
			if t.Seq > c.Seq {
				fmt.Fprintf(&b, `<line x1="%.1f" y1="0" x2="%.1f" y2="%.0f" class="cut"><title>compaction: %s → %s</title></line>`,
					pad+float64(i)*bw, pad+float64(i)*bw, h, commas(c.Before), commas(c.After))
				break
			}
		}
	}
	for _, o := range r.Offloads {
		for i, t := range r.Turns {
			if t.Seq > o.Seq {
				fmt.Fprintf(&b, `<line x1="%.1f" y1="0" x2="%.1f" y2="%.0f" class="off"><title>offloaded %d results: %s → %s</title></line>`,
					pad+float64(i)*bw, pad+float64(i)*bw, h, o.Results, commas(o.Before), commas(o.After))
				break
			}
		}
	}
	fmt.Fprintf(&b, `<text x="0" y="10" class="ax">%s</text><text x="0" y="%.0f" class="ax">0</text>`, short(int(top)), h)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="ax">turn 1</text><text x="%.0f" y="%.0f" class="ax" text-anchor="end">turn %d</text>`,
		pad, h+16, w, h+16, len(r.Turns))
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // numbers and fixed strings only
}

func short(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func revision(r string) string {
	if r == "" {
		return ""
	}
	return " (revision " + r + ")"
}

// remoteOutcome is how a remote change ended: its exit code, or why it has none.
func remoteOutcome(c RemoteChange) string {
	switch {
	case c.Decision == "denied":
		return "denied"
	case !c.Ran:
		return "not run"
	case c.ExitCode != nil:
		return "exit " + strconv.Itoa(*c.ExitCode)
	case c.IsError:
		return "failed"
	}
	return "ok"
}

func byLabel(by string) string {
	if by == "" {
		return ""
	}
	return " " + by
}

// planLabel names the runbook step a call was, or nothing.
func planLabel(hash string, step int) string {
	if hash == "" {
		return ""
	}
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return fmt.Sprintf("   [plan %s step %d]", hash, step)
}
