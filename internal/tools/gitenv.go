package tools

import (
	"os"
	"strconv"
	"strings"
)

// gitSafeConfig is git config, passed through the environment, that a
// repository's own config cannot override: the ext protocol off, so a URL
// from .gitmodules or .git/config cannot run the command it names, and no
// fsmonitor, a command git status would run. Config the command line gives
// with -c, or GIT_ALLOW_PROTOCOL, still wins; the parser reads both.
var gitSafeConfig = [][2]string{{"protocol.ext.allow", "never"}, {"core.fsmonitor", "false"}}

// WithGitSafeConfig is env with gitSafeConfig added as git environment
// config, after any entries env already holds, and the names it set so a
// container can be given them. A nil env is the process's own.
func WithGitSafeConfig(env []string) ([]string, []string) {
	if env == nil {
		env = os.Environ()
	}
	n := 0
	out := make([]string, 0, len(env)+1+2*len(gitSafeConfig))
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c > 0 && c < 1<<16 {
				n = c
			}
			continue
		}
		out = append(out, kv)
	}
	names := []string{"GIT_CONFIG_COUNT"}
	out = append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(n+len(gitSafeConfig)))
	for k, kv := range gitSafeConfig {
		i := strconv.Itoa(n + k)
		names = append(names, "GIT_CONFIG_KEY_"+i, "GIT_CONFIG_VALUE_"+i)
		out = append(out, "GIT_CONFIG_KEY_"+i+"="+kv[0], "GIT_CONFIG_VALUE_"+i+"="+kv[1])
	}
	return out, names
}
