package ui

import (
	"strings"
	"testing"
)

// Every colour a theme uses for text reads on the background it is meant
// for: 3:1 at least (the large-text and interface minimum), 4.5:1 for the
// high-contrast theme. Plain yellow and dim on white fail this.
func TestThemeContrast(t *testing.T) {
	bgs := map[string]float64{"dark": luminance(0, 0, 0), "light": luminance(1, 1, 1),
		"high-contrast": luminance(0, 0, 0), "colorblind": luminance(0, 0, 0)}
	for name, p := range palettes {
		min := 3.0
		if name == "high-contrast" {
			min = 4.5
		}
		roles := map[string]string{"accent": p.accent, "code": p.code, "red": p.red, "green": p.green,
			"yellow": p.yellow, "blue": p.blue, "magenta": p.magenta, "cyan": p.cyan, "dim": p.dim}
		for role, code := range roles {
			fg, ok := fgIndex(code)
			if !ok {
				continue // SGR dim has no colour of its own
			}
			if c := contrast(luminance(xterm256(fg)), bgs[name]); c < min {
				t.Errorf("%s %s (%s): contrast %.2f < %.1f", name, role, code, c, min)
			}
		}
		// A diff line is read against its own background.
		for role, code := range map[string]string{"diffAdd": p.diffAdd, "diffDel": p.diffDel} {
			fg, _ := fgIndex(code)
			bg := bgs[name]
			if i := strings.Index(code, "48;5;"); i >= 0 {
				var n int
				for _, c := range code[i+5:] {
					if c < '0' || c > '9' {
						break
					}
					n = n*10 + int(c-'0')
				}
				bg = luminance(xterm256(n))
			} else if strings.Contains(code, "102") || strings.Contains(code, "101") {
				bg = luminance(xterm256(map[bool]int{true: 10, false: 9}[strings.Contains(code, "102")]))
			}
			if c := contrast(luminance(xterm256(fg)), bg); c < min {
				t.Errorf("%s %s (%s): contrast %.2f < %.1f", name, role, code, c, min)
			}
		}
	}
}

func TestThemeDetection(t *testing.T) {
	for reply, want := range map[string]string{
		"\x1b]11;rgb:ffff/ffff/ffff": "light",
		"\x1b]11;rgb:1e1e/1e1e/1e1e": "dark",
		"\x1b]11;rgb:fd/f6/e3":       "light",
		"\x1b]11;rgb:0000/2b2b/3636": "dark",
		"garbage":                    "",
	} {
		if got := themeFromOSC11(reply); got != want {
			t.Errorf("%q: %q, want %q", reply, got, want)
		}
	}
	for v, want := range map[string]string{"15;0": "dark", "0;15": "light", "0;default;7": "light", "": ""} {
		t.Setenv("ABHED_THEME", "")
		t.Setenv("COLORFGBG", v)
		if got := ThemeFromEnv(); got != want {
			t.Errorf("COLORFGBG=%q: %q, want %q", v, got, want)
		}
	}
	t.Setenv("ABHED_THEME", "high-contrast")
	if got := ThemeFromEnv(); got != "high-contrast" {
		t.Errorf("ABHED_THEME: %q", got)
	}
}

// Switching the theme changes the colours of what is drawn next, and
// NO_COLOR still draws none.
func TestSetTheme(t *testing.T) {
	defer func() { _ = SetTheme("dark") }()
	s := Style{enabled: true}
	if err := SetTheme("light"); err != nil {
		t.Fatal(err)
	}
	if got := s.Accent("x"); !strings.Contains(got, "38;5;166") {
		t.Errorf("light accent %q", got)
	}
	if err := SetTheme("nope"); err == nil {
		t.Error("an unknown theme was accepted")
	}
	if got := (Style{}).Accent("x"); got != "x" {
		t.Errorf("styling without colour: %q", got)
	}
}
