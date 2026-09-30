package ui

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// palette is a theme: the SGR codes for each role the renderer uses.
type palette struct {
	name                              string
	dim, accent, code                 string
	red, green, yellow, blue, magenta string
	cyan, diffAdd, diffDel            string
}

// The themes. Dark is the brand's; light swaps SGR dim and plain yellow and
// cyan, which wash out on white, for darker 256-colour greys and hues;
// high-contrast uses bold bright colours and no dimming; colour-blind uses
// blue and orange where the others use green and red.
var palettes = map[string]*palette{
	"dark": {name: "dark", dim: "2", accent: "38;5;202", code: "38;5;208",
		red: "31", green: "32", yellow: "33", blue: "38;5;75", magenta: "35", cyan: "36",
		diffAdd: "38;5;114;48;5;22", diffDel: "38;5;217;48;5;52"},
	"light": {name: "light", dim: "38;5;241", accent: "38;5;166", code: "38;5;130",
		red: "38;5;124", green: "38;5;22", yellow: "38;5;94", blue: "38;5;19", magenta: "38;5;90", cyan: "38;5;24",
		diffAdd: "38;5;22;48;5;194", diffDel: "38;5;88;48;5;224"},
	"high-contrast": {name: "high-contrast", dim: "37", accent: "1;38;5;214", code: "1;38;5;220",
		red: "1;91", green: "1;92", yellow: "1;93", blue: "1;96", magenta: "1;95", cyan: "1;96",
		diffAdd: "1;30;102", diffDel: "1;30;101"},
	"colorblind": {name: "colorblind", dim: "2", accent: "38;5;202", code: "38;5;208",
		red: "38;5;208", green: "38;5;75", yellow: "38;5;220", blue: "38;5;75", magenta: "38;5;177", cyan: "38;5;81",
		diffAdd: "38;5;117;48;5;17", diffDel: "38;5;215;48;5;52"},
}

// ThemeNames are the themes /theme offers, besides "auto".
var ThemeNames = []string{"dark", "light", "high-contrast", "colorblind"}

var current atomic.Pointer[palette]

func active() *palette {
	if p := current.Load(); p != nil {
		return p
	}
	return palettes["dark"]
}

// SetTheme makes name the theme everything draws in.
func SetTheme(name string) error {
	p, ok := palettes[name]
	if !ok {
		return fmt.Errorf("unknown theme %q: want %s or auto", name, strings.Join(ThemeNames, ", "))
	}
	current.Store(p)
	return nil
}

// Theme is the theme in use.
func Theme() string { return active().name }

// ThemeFromEnv chooses a theme from the environment: ABHED_THEME, else
// COLORFGBG ("15;0" is light on dark), else "" when it cannot tell.
func ThemeFromEnv() string {
	if t := strings.TrimSpace(os.Getenv("ABHED_THEME")); t != "" && t != "auto" {
		if _, ok := palettes[t]; ok {
			return t
		}
	}
	if v := os.Getenv("COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		if bg, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			// 0–6 and 8 are dark backgrounds in the 16-colour table; 7 and 9–15 light.
			if bg == 7 || bg >= 9 && bg <= 15 {
				return "light"
			}
			return "dark"
		}
	}
	return ""
}

// themeFromOSC11 reads a terminal's answer to "what is your background":
// rgb:RRRR/GGGG/BBBB. A background lighter than mid-grey is a light theme.
func themeFromOSC11(reply string) string {
	i := strings.Index(reply, "rgb:")
	if i < 0 {
		return ""
	}
	parts := strings.SplitN(reply[i+4:], "/", 3)
	if len(parts) != 3 {
		return ""
	}
	var ch [3]float64
	for k, p := range parts {
		p = strings.TrimRightFunc(p, func(r rune) bool { return !strings.ContainsRune("0123456789abcdefABCDEF", r) })
		if p == "" {
			return ""
		}
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			return ""
		}
		ch[k] = float64(v) / (math.Pow(16, float64(len(p))) - 1)
	}
	if luminance(ch[0], ch[1], ch[2]) > 0.4 {
		return "light"
	}
	return "dark"
}

// uiPrefs are the terminal preferences /theme and /vim save, in
// ~/.abhed/ui.json: the person's own, never a workspace's.
type uiPrefs struct {
	Theme string `json:"theme,omitempty"`
	Vim   bool   `json:"vim,omitempty"`
}

func prefsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".abhed", "ui.json")
}

// LoadPrefs reads the saved preferences; missing or unreadable is none.
func LoadPrefs() (theme string, vim bool) {
	p := prefsPath()
	if p == "" {
		return "", false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	var u uiPrefs
	if json.Unmarshal(data, &u) != nil {
		return "", false
	}
	return u.Theme, u.Vim
}

// SavePrefs writes the preferences.
func SavePrefs(theme string, vim bool) error {
	p := prefsPath()
	if p == "" {
		return fmt.Errorf("no home folder to save preferences in")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(uiPrefs{Theme: theme, Vim: vim}, "", "  ")
	return os.WriteFile(p, append(data, '\n'), 0o600)
}

// probeTimeout bounds how long startup waits for the terminal to say what
// its background is; a terminal that does not answer costs no more.
const probeTimeout = 100 * time.Millisecond

// Colour arithmetic, for choosing a theme and for the contrast tests.

// xterm256 is the RGB of a 256-colour index.
func xterm256(n int) (r, g, b float64) {
	base := [16][3]float64{{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0}, {0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
		{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255}}
	switch {
	case n < 16:
		c := base[n]
		return c[0] / 255, c[1] / 255, c[2] / 255
	case n < 232:
		n -= 16
		lv := func(v int) float64 {
			if v == 0 {
				return 0
			}
			return float64(55+40*v) / 255
		}
		return lv(n / 36), lv(n / 6 % 6), lv(n % 6)
	}
	v := float64(8+10*(n-232)) / 255
	return v, v, v
}

func luminance(r, g, b float64) float64 {
	lin := func(c float64) float64 {
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(a, b float64) float64 {
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

// fgIndex is the 256-colour foreground an SGR code sets, and whether it
// sets one.
func fgIndex(code string) (int, bool) {
	ps := strings.Split(code, ";")
	for i := 0; i < len(ps); i++ {
		n, _ := strconv.Atoi(ps[i])
		switch {
		case n == 38 && i+2 < len(ps) && ps[i+1] == "5":
			v, _ := strconv.Atoi(ps[i+2])
			return v, true
		case n >= 30 && n <= 37:
			return n - 30, true
		case n >= 90 && n <= 97:
			return n - 90 + 8, true
		}
		if n == 48 && i+2 < len(ps) {
			i += 2
		}
	}
	return 0, false
}
