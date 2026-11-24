package hostgit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestE2ParseEscapes(t *testing.T) {
	r := newRemoteRig(t)
	prog := filepath.Join(r.ws, "evil")
	os.WriteFile(prog, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	inc := filepath.Join(r.ws, "inc.conf")
	os.WriteFile(inc, []byte("ProxyCommand "+prog+"\n"), 0o644)
	own := filepath.Join(r.home, "own.conf")
	os.WriteFile(own, []byte("# nothing\n"), 0o600)
	cases := map[string]string{
		"match all exec":       "Match all exec " + prog + "\n",
		"match canonical exec": "Match canonical exec " + prog + "\n",
		"match final !exec":    "Match final !exec \"" + prog + " %h\"\n",
		"match Exec= quoted":   "Match \"Exec\"=\"" + prog + "\"\n",
		"tab separated":        "Host *\n\tProxyCommand\t" + prog + "\n",
		"crlf":                 "Host *\r\n  ProxyCommand " + prog + "\r\n",
		"comment after value":  "Host *\n  ProxyCommand /usr/bin/true # " + prog + "\n",
		"quoted Include multi": "Include " + own + " \"" + inc + "\"\n",
		"quoted LocalCommand":  "\"LocalCommand\" " + prog + "\n",
		"KnownHostsCommand eq": "KnownHostsCommand=" + prog + "\n",
		"upper keyword":        "PROXYCOMMAND " + prog + "\n",
		"leading spaces + =":   "   ProxyCommand = " + prog + "\n",
		"backslash continued":  "Host *\n  ProxyCommand /usr/bin/true \\\n" + prog + "\n",
		"include glob":         "Include " + filepath.Join(r.ws, "*.conf") + "\n",
	}
	for name, body := range cases {
		cfg := filepath.Join(r.home, "sshcfg")
		os.WriteFile(cfg, []byte(body), 0o600)
		run(t, r.home, "config", "--global", "core.sshCommand", "/usr/bin/ssh -F "+cfg)
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		if err == nil {
			t.Errorf("%s: NOT REFUSED", name)
		} else {
			t.Logf("%s: refused", name)
		}
	}
}
