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

// windowsEnvNames are what a Windows program needs to start at all.
var windowsEnvNames = []string{"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES"}

// ServerEnv is a stdio server's environment: the base names from Abhed's own
// environment, then the configured entries, which win. KEY=VALUE sets KEY; a
// bare KEY passes Abhed's own value of it, which is how a person hands a
// server a token from their environment.
func ServerEnv(configured []string) []string {
	names := baseEnvNames
	if runtime.GOOS == "windows" {
		names = append(append([]string{}, names...), windowsEnvNames...)
	}
	out := []string{}
	for _, k := range names {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for _, e := range configured {
		if strings.Contains(e, "=") {
			out = append(out, e)
		} else if v, ok := os.LookupEnv(e); ok && e != "" {
			out = append(out, e+"="+v)
		}
	}
	return out
}
