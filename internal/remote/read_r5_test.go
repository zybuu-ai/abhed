package remote

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// Every program a wildcard entry may name has a form, and an option not on it
// is refused: written out, abbreviated, glued or in a cluster.
func TestSSHReadEveryFormRefusesUnlistedOptions(t *testing.T) {
	unlisted := map[string][]string{
		"cat": {"--foo", "-Z", "-nX"}, "head": {"--byt=5", "-x"}, "tail": {"--follow=x", "--fol", "-Q"},
		"cut": {"--fiel=1", "-q"}, "cmp": {"--verb", "-x"}, "diff": {"--from-fi=x", "-D x", "--ifdef=x", "-rXq"},
		"zcat": {"-c", "-d", "--stdout", "-r"}, "grep": {"--file=x --foo", "-X", "-rx9"}, "egrep": {"-Y"}, "fgrep": {"-Y"},
		"ls": {"--colo", "-lZq", "--dired", "-k9"}, "stat": {"--form=%n", "-x"}, "df": {"--sync", "-v"},
		"free": {"--sec=1", "-x"}, "uptime": {"-V", "--x"}, "uname": {"-x", "--al"}, "id": {"-Z", "--contex"},
		"whoami": {"--help", "-x"}, "groups": {"--x", "-x"}, "nproc": {"--ign=1", "-x"},
		"ps": {"--info", "-V", "eww", "-ewf"}, "pgrep": {"-F /tmp/x", "--pidfile=/tmp/x", "-9", "--signal=9"},
		"netstat": {"-c", "--continuous", "-M", "-tulpnc"}, "lsblk": {"--sysroot=/x", "-Q x", "--filter=x"},
		"lscpu": {"-s /x", "--sysroot=/x", "--sys=/x"}, "lsmem": {"-s /x", "--sysroot=/x"}, "lsmod": {"-x"},
		"lspci": {"-q", "-Q", "-O net.cache_name=/home/u/.ssh/authorized_keys", "-A x", "-H1", "-F /tmp/x", "-i /tmp/x", "-p /tmp/x",
			"-vq", "-nnQ"},
		"lsusb": {"-D /etc/shadow", "-vD"}, "w": {"-x", "--from=x"}, "who": {"--x", "-Z"}, "getent": {"-s ../x hosts", "--service=x hosts", "-sfiles"},
		"vmstat": {"-x", "--wid"}, "basename": {"--suff=x", "-x"}, "dirname": {"-x"}, "realpath": {"--relative=x", "-x"},
		"readlink": {"--canon", "-x"}, "wc": {"--files0-from=/etc/shadow", "--word"}, "du": {"-X /etc/shadow", "--files0-from=x", "-shX"},
		"md5sum": {"-c x", "--check x"}, "sha1sum": {"-c x"}, "sha224sum": {"-c x"}, "sha256sum": {"-c x", "-cw x"},
		"sha384sum": {"-c x"}, "sha512sum": {"-c x"}, "b2sum": {"-c x"}, "cksum": {"--check x", "-c x"},
		"findmnt": {"-F /etc/shadow", "--tab-file=/x", "--poll"}, "file": {"-m /etc/shadow", "-C", "-z"},
		"rg": {"--pre=sh", "-z", "--pr=sh"}, "sort": {"-o /x", "--out=/x", "-T /x"}, "date": {"-s x", "--se=x", "-f /x"},
		"hostname": {"-F /x", "-b", "--fq"}, "dmesg": {"-C", "--cl", "-Tc"}, "journalctl": {"--vacuum-size=1", "--vac=1", "--cursor-file=/x"},
		"dpkg": {"-i x", "--instal x", "-li"}, "dpkg-query": {"--admindir=/x -l", "--load-avail"}, "rpm": {"-i x", "--qf=%(id)", "-qe"},
	}
	for prog, opts := range unlisted {
		e := readsEngine(t, policy.ModePlan, prog+" *")
		for _, opt := range opts {
			cmd := prog + " " + opt + " /var/log/x"
			if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
				t.Errorf("%q ran in plan mode: %+v", cmd, res)
			}
		}
	}
}

// A glob that could expand to an option (one that starts the word) is not
// read; one after a path is; ps takes only its listed BSD options.
func TestSSHReadGlobsAndOperands(t *testing.T) {
	e := readsEngine(t, policy.ModePlan, "ls *", "cat *", "ps *", "dpkg-query -W *", "rpm *")
	for _, cmd := range []string{"ls *", "ls -l *.conf", "cat -- *", "cat '-'*", "ps eww", "ps auxe", "rpm -qa --qf '%(id)'",
		"rpm -qa --qf '%{NAME:expand}'"} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
			t.Errorf("%q ran in plan mode: %+v", cmd, res)
		}
	}
	for _, cmd := range []string{"ls -l /etc/nginx/*.conf", "cat /var/log/*.log", "ps aux", "ps -ef",
		"dpkg-query -W -f '${Package} ${Version} ' nginx", "rpm -qa --qf '%{NAME}-%{VERSION} '"} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Allow {
			t.Errorf("%q, a read, was refused: %+v", cmd, res)
		}
	}
	h := readsEngine(t, policy.ModePlan, "hostname *", "uptime *", "whoami *")
	for _, cmd := range []string{"hostname newname", "hostname -F /etc/x", "uptime x", "whoami x"} {
		if res := h.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
			t.Errorf("%q ran in plan mode: %+v", cmd, res)
		}
	}
	for _, r := range []string{"tini -- id", "dumb-init id"} {
		if _, err := policy.ParseRemoteRead(r); err == nil {
			t.Errorf("%q was accepted", r)
		}
	}
}

// A backslash inside single quotes means one thing to a POSIX shell and
// another to fish, which reads \' as a quote kept: such a call is not a read,
// whatever the remote login shell. ps's e prints environments.
func TestSSHReadShellNeutralQuotingAndPsEnv(t *testing.T) {
	e := readsEngine(t, policy.ModePlan, "dpkg-query -W *", "rpm *", "cat *", "ps *")
	for _, cmd := range []string{
		`dpkg-query -W -f '\' 'x (id) y' '\'`,
		`dpkg-query -W -f '${Package}\n' nginx`,
		`rpm -qa --qf '%{NAME}\n'`,
		`cat '\' /etc/hostname`,
		"ps e", "ps auxe", "ps eaux", "ps axe",
	} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
			t.Errorf("%q ran in plan mode: %+v", cmd, res)
		}
	}
	if _, err := policy.ParseRemoteRead(`dpkg-query -W -f '\' 'x ; id' '\'`); err == nil {
		t.Error("an entry with a backslash in single quotes around shell text was accepted")
	}
}
