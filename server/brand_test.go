package server

import (
	"regexp"
	"strings"
	"testing"
)

// A placeholder that misses substitution ships as literal text on a served
// page, and nothing else would notice.
func TestBrandPlaceholdersAreFilled(t *testing.T) {
	external := regexp.MustCompile(`(?i)(src\s*=\s*["']?(https?:)?//|<link[^>]+href\s*=\s*["']?https?:|url\(\s*["']?https?:)`)
	for name, page := range map[string]string{
		"landing": landingHTML,
		"console": consoleHTML,
		"account": accountHTML,
		"ide":     idePage,
	} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(page, "{{BRAND_") || strings.Contains(page, "BRAND_CSS") {
				t.Error("an unfilled brand placeholder is left in the page")
			}
			if !strings.Contains(page, "data:image/webp;base64,") {
				t.Error("the page carries no embedded brand image")
			}
			// Whole, so both --on-accent values are there: without the light
			// one, light-theme buttons fall back to ink on the deep orange.
			if !strings.Contains(page, brandCSS) {
				t.Error("the page does not carry the brand CSS")
			}
			for _, v := range []string{":root{--on-accent:#fff}", "{--on-accent:#0B0B0C}"} {
				if !strings.Contains(page, v) {
					t.Errorf("the page lacks %s", v)
				}
			}
			if m := external.FindString(page); m != "" {
				t.Errorf("the page loads from outside: %q", m)
			}
			// The brand rules come after the page's palette, so the first
			// :root block a reader finds is still the palette.
			if strings.Index(page, brandCSS) < strings.Index(page, ":root{") {
				t.Error("the brand CSS is placed before the page's palette")
			}
		})
	}
}
