package webfetch

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// skipped are elements whose content is never text a reader sees.
var skipped = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"svg": true, "iframe": true, "object": true, "canvas": true, "head": true,
}

// blocks start a new line; the rest of the markup is inline.
var blocks = map[string]bool{
	"p": true, "div": true, "br": true, "hr": true, "section": true, "article": true,
	"header": true, "footer": true, "nav": true, "main": true, "aside": true,
	"blockquote": true, "ul": true, "ol": true, "dl": true, "dt": true, "dd": true,
	"table": true, "tr": true, "form": true, "figure": true, "figcaption": true,
	"address": true, "details": true, "summary": true, "body": true, "html": true,
}

var (
	reHref   = regexp.MustCompile(`(?is)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	reSpaces = regexp.MustCompile(`[ \t\f\v\r\n]+`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
)

// htmlText reduces a page to readable text in a light Markdown: headings,
// list items and links survive, markup and scripts do not. It is a reader,
// not a parser: broken markup yields imperfect text, never an error. Links
// are made absolute against base so the model can fetch one next.
func htmlText(page string, base *url.URL) (title, text string) {
	var b strings.Builder
	var link struct {
		href string
		text strings.Builder
		open bool
	}
	skip, pre := "", 0
	inTitle := false
	var titleB strings.Builder

	write := func(s string) {
		if link.open {
			link.text.WriteString(s)
			return
		}
		b.WriteString(s)
	}
	text2 := func(raw string) {
		s := html.UnescapeString(raw)
		if pre == 0 {
			s = reSpaces.ReplaceAllString(s, " ")
		}
		write(s)
	}

	for i := 0; i < len(page); {
		lt := strings.IndexByte(page[i:], '<')
		if lt < 0 {
			if skip == "" {
				text2(page[i:])
			}
			break
		}
		if lt > 0 {
			switch {
			case inTitle:
				titleB.WriteString(page[i : i+lt])
			case skip == "":
				text2(page[i : i+lt])
			}
		}
		i += lt
		rest := page[i:]
		// Comments, doctypes and CDATA carry nothing to read.
		if strings.HasPrefix(rest, "<!--") {
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		gt := tagEnd(rest)
		if gt < 0 {
			break
		}
		tag := rest[1:gt]
		i += gt + 1
		if tag == "" || tag[0] == '!' || tag[0] == '?' {
			continue
		}
		closing := tag[0] == '/'
		name := strings.ToLower(strings.TrimLeft(tag, "/"))
		if n := strings.IndexAny(name, " \t\r\n/"); n >= 0 {
			name = name[:n]
		}

		if skip != "" {
			switch {
			case closing && name == skip:
				skip = ""
			case name == "title" && skip == "head":
				inTitle = !closing
			case name == "body" && skip == "head" && !closing:
				// </head> may be left out; the body still starts.
				skip = ""
			}
			continue
		}
		if !closing && skipped[name] && !strings.HasSuffix(tag, "/") {
			skip = name
			continue
		}
		if name == "title" {
			inTitle = !closing
			continue
		}

		switch {
		case name == "a" && !closing:
			link.open, link.href = true, hrefOf(tag, base)
			link.text.Reset()
		case name == "a" && closing && link.open:
			link.open = false
			t := strings.TrimSpace(link.text.String())
			switch {
			case link.href != "" && t != "":
				write("[" + t + "](" + link.href + ")")
			default:
				write(t)
			}
		case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
			if closing {
				write("\n\n")
			} else {
				write("\n\n" + strings.Repeat("#", int(name[1]-'0')) + " ")
			}
		case name == "li" && !closing:
			write("\n- ")
		case name == "pre":
			if closing {
				pre = max(0, pre-1)
				write("\n```\n")
			} else {
				pre++
				write("\n```\n")
			}
		case name == "td" || name == "th":
			if !closing {
				write(" | ")
			}
		case name == "p" || name == "blockquote" || name == "table":
			write("\n\n")
		case blocks[name]:
			write("\n")
		}
	}
	if link.open {
		b.WriteString(link.text.String())
	}

	// Indentation is kept only inside preformatted blocks, where it means something.
	lines := strings.Split(b.String(), "\n")
	fenced := false
	for i, l := range lines {
		l = strings.TrimRight(l, " \t")
		if strings.HasPrefix(strings.TrimLeft(l, " "), "```") {
			fenced = !fenced
			l = "```"
		} else if !fenced {
			l = strings.TrimLeft(l, " ")
		}
		lines[i] = l
	}
	text = strings.TrimSpace(reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	title = strings.TrimSpace(reSpaces.ReplaceAllString(html.UnescapeString(titleB.String()), " "))
	return title, text
}

// tagEnd finds the > that ends the tag s starts with, skipping any inside a
// quoted attribute value, or -1.
func tagEnd(s string) int {
	var quote byte
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

// hrefOf is a link's target made absolute, or "" for one that is not a page:
// an anchor on this page, script, or mail.
func hrefOf(tag string, base *url.URL) string {
	m := reHref.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	raw := html.UnescapeString(m[1] + m[2] + m[3])
	if raw == "" || strings.HasPrefix(raw, "#") {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}
