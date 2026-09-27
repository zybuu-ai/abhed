package agent

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
)

// ExportHTML renders a session as a self-contained page.
//
// `/export` already wrote the event stream, but as JSON — useful to a program
// and not to a person. A transcript that exists and a transcript that gets read
// are different things, and the difference is usually whether someone has to
// pipe it through a parser first.
//
// Everything is inlined and nothing is fetched, so the file works from a
// filesystem, an email attachment, or an air-gapped machine. Tool output is
// escaped and never rendered as markup: a transcript is a record of untrusted
// content, and a session that read a hostile file must not produce a page that
// executes it.
func ExportHTML(sessionID string, events []Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, exportHead, html.EscapeString(sessionID))

	var stats SessionEnded
	callArgs := map[string]string{}

	for _, ev := range events {
		switch ev.Type {
		case EvUserMessage:
			var m Message
			if json.Unmarshal(ev.Payload, &m) == nil && strings.TrimSpace(m.Text) != "" {
				row(&b, "user", "you", m.Text)
			}

		case EvAgentMessage:
			var m Message
			if json.Unmarshal(ev.Payload, &m) == nil && strings.TrimSpace(m.Text) != "" {
				row(&b, "agent", "abhed", m.Text)
			}

		case EvAgentReasoning:
			var r Reasoning
			if json.Unmarshal(ev.Payload, &r) == nil {
				fmt.Fprintf(&b, `<details class="think"><summary>reasoning · %d words</summary><pre>%s</pre></details>`,
					len(strings.Fields(r.Text)), html.EscapeString(r.Text))
			}

		case EvActionRequested:
			var a ActionRequested
			if json.Unmarshal(ev.Payload, &a) == nil {
				callArgs[a.CallID] = string(a.Args)
				fmt.Fprintf(&b, `<div class="call"><span class="tool">%s</span><span class="args">%s</span>`,
					html.EscapeString(a.Tool), html.EscapeString(clip(string(a.Args), 200)))
				if a.RequiresApproval {
					fmt.Fprintf(&b, `<span class="ask">approval required</span>`)
				}
				b.WriteString(`</div>`)
			}

		case EvObservation:
			var o Observation
			if json.Unmarshal(ev.Payload, &o) == nil {
				cls := "out"
				if o.IsError {
					cls = "out err"
				}
				fmt.Fprintf(&b, `<details class="%s"><summary>%s · %dms</summary><pre>%s</pre></details>`,
					cls, html.EscapeString(o.Tool), o.DurationMS,
					html.EscapeString(clip(o.Content, 20000)))
			}

		case EvActionDenied:
			var m map[string]string
			if json.Unmarshal(ev.Payload, &m) == nil {
				fmt.Fprintf(&b, `<div class="denied">denied — %s</div>`,
					html.EscapeString(m["reason"]))
			}

		case EvTodoUpdated:
			var l TodoList
			if json.Unmarshal(ev.Payload, &l) == nil {
				b.WriteString(`<div class="todo"><b>task list</b><ul>`)
				for _, it := range l.Items {
					fmt.Fprintf(&b, `<li class="%s">%s</li>`,
						html.EscapeString(it.Status), html.EscapeString(it.Text))
				}
				b.WriteString(`</ul></div>`)
			}

		case EvCompactDone:
			var c Compaction
			if json.Unmarshal(ev.Payload, &c) == nil && c.BeforeTokens > 0 {
				fmt.Fprintf(&b, `<div class="note">context compacted · %s → %s tokens</div>`,
					commas(c.BeforeTokens), commas(c.AfterTokens))
			}

		case EvForked:
			var f Forked
			if json.Unmarshal(ev.Payload, &f) == nil {
				fmt.Fprintf(&b, `<div class="note">forked at step %d · the steps after it, above, were abandoned</div>`, f.ThroughSeq)
			}

		case EvSessionEnded:
			_ = json.Unmarshal(ev.Payload, &stats) // a malformed summary leaves the totals at zero, which the export shows as such
		}
	}

	fmt.Fprintf(&b, `<div class="summary">ended: <b>%s</b> · %d turns · %s tokens in / %s out`,
		html.EscapeString(string(stats.Reason)), stats.Turns,
		commas(stats.TokensIn), commas(stats.TokensOut))
	if stats.Compactions > 0 {
		fmt.Fprintf(&b, ` · %d compactions`, stats.Compactions)
	}
	b.WriteString(`</div></body></html>`)
	return b.String()
}

func row(b *strings.Builder, cls, who, text string) {
	fmt.Fprintf(b, `<div class="msg %s"><div class="who">%s</div><div class="body">%s</div></div>`,
		cls, who, html.EscapeString(text))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n… %d more characters", len(s)-n)
}

func commas(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

const exportHead = `<!doctype html><meta charset="utf-8">
<title>Abhed session %s</title>
<style>
:root{--bg:#fbfbfa;--ink:#1a1a19;--ink2:#55554f;--line:#e4e4e0;--accent:#3b6ea5;
  --err:#b4433a;--warn:#9a6b1f;--sunken:#f4f4f1}
@media (prefers-color-scheme:dark){:root{--bg:#17171a;--ink:#e8e8e4;--ink2:#a0a09a;
  --line:#2c2c30;--accent:#7aa7d9;--err:#e0796e;--warn:#d6a45c;--sunken:#1e1e22}}
body{max-width:52rem;margin:2rem auto;padding:0 1.2rem;background:var(--bg);color:var(--ink);
  font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
.msg{margin:1.1rem 0}
.who{font:600 11px/1 ui-monospace,monospace;letter-spacing:.08em;text-transform:uppercase;
  color:var(--ink2);margin-bottom:.35rem}
.msg.user .body{background:var(--sunken);padding:.7rem .9rem;border-radius:6px}
.body{white-space:pre-wrap;overflow-wrap:anywhere}
.call{font:12px/1.5 ui-monospace,monospace;margin:.6rem 0 .2rem;display:flex;gap:.6rem;
  align-items:baseline;flex-wrap:wrap}
.tool{color:var(--accent);font-weight:600}
.args{color:var(--ink2);overflow-wrap:anywhere}
.ask{color:var(--warn)}
details{margin:.2rem 0 .6rem}
summary{font:11px/1.6 ui-monospace,monospace;color:var(--ink2);cursor:pointer}
details pre{background:var(--sunken);border-left:2px solid var(--line);border-radius:0 5px 5px 0;
  padding:.6rem .8rem;margin:.4rem 0 0;font:11px/1.55 ui-monospace,monospace;
  white-space:pre-wrap;overflow-x:auto;max-height:26rem;overflow-y:auto}
.out.err pre{border-left-color:var(--err);color:var(--err)}
.denied{font:12px/1.6 ui-monospace,monospace;color:var(--err);margin:.3rem 0}
.todo{background:var(--sunken);border-radius:6px;padding:.6rem .9rem;margin:.7rem 0;font-size:13px}
.todo ul{margin:.35rem 0 0;padding-left:1.1rem}
.todo li.done{text-decoration:line-through;color:var(--ink2)}
.todo li.in_progress{font-weight:600}
.note,.summary{font:11px/1.6 ui-monospace,monospace;color:var(--ink2);
  border-top:1px solid var(--line);padding-top:.5rem;margin:1rem 0}
.summary{margin-top:2rem}
</style>
<body><h1 style="font-size:15px;font-family:ui-monospace,monospace;color:var(--ink2)">Abhed session %[1]s</h1>
`
