package tools

import (
	"os"
	"strconv"
	"strings"
)

// gitExtNever is git config, passed through the environment, that turns off
// the ext protocol: a URL from .gitmodules or a repository's own config can
// then not run the command it names. Config the command line gives with -c,
// or GIT_ALLOW_PROTOCOL, still wins; the parser reads both on the line.
const gitExtKey, gitExtValue = "protocol.ext.allow", "never"

// WithGitExtNever is env with protocol.ext.allow=never added as git
// environment config, after any entries env already holds, and the names it
// set so a container can be given them. A nil env is the process's own.
func WithGitExtNever(env []string) ([]string, []string) {
	if env == nil {
		env = os.Environ()
	}
	n := 0
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c > 0 && c < 1<<16 {
				n = c
			}
			continue
		}
		out = append(out, kv)
	}
	i := strconv.Itoa(n)
	names := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_" + i, "GIT_CONFIG_VALUE_" + i}
	out = append(out,
		names[0]+"="+strconv.Itoa(n+1),
		names[1]+"="+gitExtKey,
		names[2]+"="+gitExtValue)
	return out, names
}
