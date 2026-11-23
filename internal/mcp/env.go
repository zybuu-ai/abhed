package mcp

import (
	"os"
	"runtime"
	"strings"
)

// baseEnvNames pass from Abhed's environment to every stdio server: what a
// program needs to find its tools, its home and a temporary folder. None
// holds a credential; a server that needs one is given it in its env.
var baseEnvNames = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TZ"}

// narrowingEnvNames say who is working (app.EnvGuest, app.EnvAccount), so an
// Abhed a server starts works as the same person or narrower. They always
// come from Abhed's own environment; a configured entry cannot set or clear
// them.
var narrowingEnvNames = []string{"ABHED_GUEST", "ABHED_ACCOUNT"}

// windowsEnvNames are what a Windows program needs to start at all.
var windowsEnvNames = []string{"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES"}

// ServerEnv is a stdio server's environment: the base names from Abhed's own
// environment, then the configured entries, which win. KEY=VALUE sets KEY; a
// bare KEY passes Abhed's own value of it, which is how a person hands a
// server a token from their environment. Who is working is Abhed's alone:
// configured entries naming ABHED_GUEST or ABHED_ACCOUNT are dropped.
func ServerEnv(configured []string) []string {
	names := append(append([]string{}, baseEnvNames...), narrowingEnvNames...)
	if runtime.GOOS == "windows" {
		names = append(names, windowsEnvNames...)
	}
	out := []string{}
	for _, k := range names {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for _, e := range configured {
		if narrowing(e) {
			continue
		}
		if strings.Contains(e, "=") {
			out = append(out, e)
		} else if v, ok := os.LookupEnv(e); ok && e != "" {
			out = append(out, e+"="+v)
		}
	}
	return out
}

// narrowing reports whether a configured entry names a variable that says
// who is working. Names are matched as Windows does too, ignoring case.
func narrowing(entry string) bool {
	k, _, _ := strings.Cut(entry, "=")
	for _, n := range narrowingEnvNames {
		if strings.EqualFold(strings.TrimSpace(k), n) {
			return true
		}
	}
	return false
}
