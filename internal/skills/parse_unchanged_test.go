package skills

import (
	"fmt"
	"strings"
	"testing"
)

// legacyParse is the SKILL.md reader as it was before the shared frontmatter
// reader, kept as the oracle the new one must agree with. It reads: YAML-ish frontmatter between --- markers, then the
// body.
//
// The frontmatter is name/description/allowed-tools and nothing else, so it is
// parsed directly rather than through a YAML library — the same reasoning as
// the kubeconfig reader: a narrow known shape does not justify a large
// dependency in an air-gapped bundle.
func legacyParse(content string) (*Skill, error) {
	s := &Skill{}
	text := strings.ReplaceAll(content, "\r\n", "\n")

	if !strings.HasPrefix(strings.TrimLeft(text, " \t\n"), "---") {
		return nil, fmt.Errorf("missing frontmatter: a SKILL.md starts with a --- block " +
			"containing name and description")
	}
	text = strings.TrimLeft(text, " \t\n")
	rest := text[3:]
	if i := strings.Index(rest, "\n---"); i >= 0 {
		frontmatter := rest[:i]
		s.Body = strings.TrimSpace(rest[i+4:])
		legacyFrontmatter(frontmatter, s)
	} else {
		return nil, fmt.Errorf("frontmatter is not closed with ---")
	}

	if s.Description == "" {
		// Without a description the model has no basis to choose this skill,
		// so it will either never invoke it or invoke it for everything.
		return nil, fmt.Errorf("frontmatter needs a description saying WHEN to use this skill")
	}
	if strings.TrimSpace(s.Body) == "" {
		return nil, fmt.Errorf("skill has no instructions after the frontmatter")
	}
	return s, nil
}

func legacyFrontmatter(fm string, s *Skill) {
	lines := strings.Split(fm, "\n")
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		// Block scalars: "description: >" (folded) or "|" (literal) put the
		// text on the following indented lines. A real skill used this and the
		// description parsed as ">" — one character, which the model would
		// never match a request against. Silently useless is the worst
		// outcome, so it is handled rather than rejected.
		if value == ">" || value == "|" || value == ">-" || value == "|-" {
			var block []string
			indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
			for j := i + 1; j < len(lines); j++ {
				next := lines[j]
				if strings.TrimSpace(next) == "" {
					block = append(block, "")
					continue
				}
				nextIndent := len(next) - len(strings.TrimLeft(next, " \t"))
				if nextIndent <= indent {
					break // dedented: the block ended
				}
				block = append(block, strings.TrimSpace(next))
				i = j
			}
			if value == ">" || value == ">-" {
				// Folded: newlines become spaces, blank lines become breaks.
				value = strings.TrimSpace(strings.Join(block, " "))
				value = strings.Join(strings.Fields(value), " ")
			} else {
				value = strings.TrimSpace(strings.Join(block, "\n"))
			}
		} else {
			value = strings.Trim(value, `"'`)
		}

		switch key {
		case "name":
			s.Name = value
		case "description":
			s.Description = value
		}
	}
}

// Skills parse exactly as before the reader was shared: name, description,
// body and the error, for every shape the old reader met, including the
// quirks (a nested name overrides, a list's items are not keys).
func TestSkillsParseUnchanged(t *testing.T) {
	corpus := []string{
		"---\nname: a\ndescription: b\n---\nbody",
		"---\r\nname: a\r\ndescription: \"quoted\"\r\n---\r\nbody\r\n",
		"\n\n  ---\nname: 'x'\ndescription: y\n---\n\nbody\n",
		"---\nname: a\ndescription: >\n  folded\n  text\n\n  more\nother: 1\n---\nb",
		"---\nname: a\ndescription: |\n  line one\n  line two\n---\nb",
		"---\nname: a\ndescription: >-\n  x\n---\nb",
		"---\nname: a\ndescription: |-\n  x\n---\nb",
		"---\nname: a\ndescription: [one, two]\n---\nb",
		"---\nname: a\ndescription:\n  - one\n  - two\n---\nb",
		"---\nname: a\nmetadata:\n  name: inner\ndescription: d\n---\nb",
		"---\nNAME: Upper\nDescription: D\n---\nb",
		"---\n# comment\nname: a\n- description: listed\ndescription: real\n---\nb",
		"---\nname: a\ndescription: d\nallowed-tools: [read, grep]\ntools:\n- read\n---\nb",
		"---\nname: a\ndescription: d: with colon\n---\nb",
		"---\nname: a\ndescription: d\n---\n",
		"---\nname: a\n---\nbody",
		"no frontmatter",
		"---\nname: a\ndescription: d\n",
		"---\nname: a\ndescription: d\n---extra\nbody",
		"---\n  name: indented\n  description: all\n---\nb",
		"---\nname: a\ndescription: first\ndescription: second\n---\nb",
		"---\nname: a\ndescription: >\nnot indented\n---\nb",
	}
	for i, c := range corpus {
		want, werr := legacyParse(c)
		got, gerr := Parse(c)
		if fmt.Sprint(werr) != fmt.Sprint(gerr) {
			t.Fatalf("case %d: error %v, was %v\n%s", i, gerr, werr, c)
		}
		if werr != nil {
			continue
		}
		if got.Name != want.Name || got.Description != want.Description || got.Body != want.Body {
			t.Fatalf("case %d: got %q/%q/%q, was %q/%q/%q", i, got.Name, got.Description, got.Body,
				want.Name, want.Description, want.Body)
		}
	}
}
