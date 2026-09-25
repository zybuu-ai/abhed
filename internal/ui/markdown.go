package ui

import (
	"fmt"
	"strings"
)

// Markdown renders a model's reply for a terminal.
//
// Models answer in markdown whether or not anything asked them to, so a raw
// print shows the reader `**z/OS**` and a table drawn in pipes. The content is
// right and the presentation is noise — and a table is the worst of it, since
// the alignment that makes it readable is exactly what a monospace terminal
// could provide and does not.
//
// This is deliberately a small renderer rather than a dependency. It handles
// what a model actually emits — headings, bold, italic, inline code, fenced
// blocks, lists, tables, rules, links — and leaves anything it does not
// recognise as written, which is the safe direction: an unrendered line is
// readable, a mangled one is not.
func Markdown(s Style, text string) string {
	var out strings.Builder
	lines := strings.Split(text, "\n")

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// A fenced block is reproduced verbatim, indented. Its whole purpose is
		// that the characters inside it mean nothing else.
		if fence := strings.TrimSpace(line); strings.HasPrefix(fence, "```") {
			lang := strings.TrimSpace(strings.TrimPrefix(fence, "```"))
			if lang != "" {
				fmt.Fprintf(&out, "  %s\n", s.Dim(lang))
			}
			for i++; i < len(lines); i++ {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
					break
				}
				fmt.Fprintf(&out, "  %s\n", s.Accent(lines[i]))
			}
			continue
		}

		// A table is collected whole so the columns can be measured. Rendering
		// it row by row would reproduce the input's ragged pipes.
		if isTableRow(line) && i+1 < len(lines) && isTableDivider(lines[i+1]) {
			rows := [][]string{splitRow(line)}
			i++ // skip the divider
			for i+1 < len(lines) && isTableRow(lines[i+1]) {
				i++
				rows = append(rows, splitRow(lines[i]))
			}
			out.WriteString(renderTable(s, rows))
			continue
		}

		out.WriteString(renderLine(s, line))
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}

func renderLine(s Style, line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]

	// A horizontal rule.
	if t := strings.TrimSpace(line); t == "---" || t == "***" || t == "___" {
		return s.Dim(strings.Repeat("─", 60))
	}

	// Headings: the level is shown by weight rather than by hashes, which are
	// markup the reader did not ask to see.
	if h := strings.TrimLeft(trimmed, "#"); len(h) < len(trimmed) && strings.HasPrefix(h, " ") {
		level := len(trimmed) - len(h)
		title := inline(s, strings.TrimSpace(h))
		switch level {
		case 1:
			return "\n" + s.Bold(strings.ToUpper(title))
		case 2:
			return "\n" + s.Bold(title)
		default:
			return "\n" + s.Accent(title)
		}
	}

	// Bullets, with a real bullet character in place of the marker.
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, marker) {
			return indent + s.Dim("•") + " " + inline(s, trimmed[2:])
		}
	}

	// Numbered items keep their number, which carries meaning.
	if n, rest, ok := numberedItem(trimmed); ok {
		return indent + s.Dim(n+".") + " " + inline(s, rest)
	}

	// Block quotes.
	if strings.HasPrefix(trimmed, "> ") {
		return indent + s.Dim("│ "+stripInline(trimmed[2:]))
	}

	return indent + inline(s, trimmed)
}

// inline renders emphasis and code spans within a line.
func inline(s Style, text string) string {
	text = replacePairs(text, "**", s.Bold)
	text = replacePairs(text, "`", s.Accent)
	// Single asterisk last, so it cannot consume the halves of a bold pair.
	text = replacePairs(text, "*", s.Bold)
	text = renderLinks(s, text)
	return text
}

// replacePairs rewrites text between matching delimiters.
func replacePairs(text, delim string, f func(string) string) string {
	var out strings.Builder
	for {
		start := strings.Index(text, delim)
		if start < 0 {
			out.WriteString(text)
			return out.String()
		}
		rest := text[start+len(delim):]
		end := strings.Index(rest, delim)
		if end < 0 {
			out.WriteString(text)
			return out.String()
		}
		inner := rest[:end]
		// An empty or whitespace-only span is punctuation, not emphasis.
		if strings.TrimSpace(inner) == "" {
			out.WriteString(text[:start+len(delim)])
			text = text[start+len(delim):]
			continue
		}
		out.WriteString(text[:start])
		out.WriteString(f(inner))
		text = rest[end+len(delim):]
	}
}

// renderLinks shows the text and dims the URL, rather than hiding either.
func renderLinks(s Style, text string) string {
	for {
		open := strings.Index(text, "](")
		if open < 0 {
			return text
		}
		start := strings.LastIndex(text[:open], "[")
		if start < 0 {
			return text
		}
		close := strings.Index(text[open:], ")")
		if close < 0 {
			return text
		}
		label := text[start+1 : open]
		url := text[open+2 : open+close]
		text = text[:start] + label + " " + s.Dim("("+url+")") + text[open+close+1:]
	}
}

func stripInline(text string) string {
	for _, d := range []string{"**", "`", "*"} {
		text = strings.ReplaceAll(text, d, "")
	}
	return text
}

func numberedItem(s string) (string, string, bool) {
	for i, c := range s {
		if c >= '0' && c <= '9' {
			continue
		}
		if c == '.' && i > 0 && i+1 < len(s) && s[i+1] == ' ' {
			return s[:i], s[i+2:], true
		}
		return "", "", false
	}
	return "", "", false
}

func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|") && len(t) > 1
}

func isTableDivider(line string) bool {
	if !isTableRow(line) {
		return false
	}
	for _, c := range strings.TrimSpace(line) {
		if c != '|' && c != '-' && c != ':' && c != ' ' {
			return false
		}
	}
	return true
}

func splitRow(line string) []string {
	t := strings.Trim(strings.TrimSpace(line), "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// renderTable aligns the columns, which is the whole reason a table was used
// and the one thing a raw print cannot give.
func renderTable(s Style, rows [][]string) string {
	width := 0
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	widths := make([]int, width)
	for _, r := range rows {
		for i, cell := range r {
			if n := len(stripInline(cell)); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var out strings.Builder
	for n, r := range rows {
		out.WriteString("  ")
		for i := 0; i < width; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			pad := widths[i] - len(stripInline(cell))
			rendered := inline(s, cell)
			if n == 0 {
				rendered = s.Bold(stripInline(cell))
			}
			out.WriteString(rendered)
			if i < width-1 {
				out.WriteString(strings.Repeat(" ", pad+2))
			}
		}
		out.WriteByte('\n')
		if n == 0 {
			out.WriteString("  ")
			for i := 0; i < width; i++ {
				out.WriteString(s.Dim(strings.Repeat("─", widths[i])))
				if i < width-1 {
					out.WriteString("  ")
				}
			}
			out.WriteByte('\n')
		}
	}
	return out.String()
}
