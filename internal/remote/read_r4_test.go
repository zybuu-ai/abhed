package remote

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// A program that shows a file's type, size or sum, not its lines, may not be
// given an option that prints another file's lines through a wildcard entry.
func TestSSHReadNoOptionPrintsAnotherFile(t *testing.T) {
	for entry, calls := range map[string][]string{
		"file *":      {"-m /home/u/.ssh/id_rsa x", "--magic-file /home/u/.ssh/id_rsa x", "--magic-file=/etc/shadow x", "-m/etc/shadow x"},
		"wc *":        {"--files0-from=/etc/shadow", "--files0-from /etc/shadow"},
		"du *":        {"--files0-from=/etc/shadow", "-X /etc/shadow /", "--exclude-from=/etc/shadow /"},
		"sha256sum *": {"-c /etc/shadow", "--check /etc/shadow", "-cw /etc/shadow"},
		"md5sum *":    {"-c /etc/shadow"},
		"cksum *":     {"--check /etc/shadow"},
		"findmnt *":   {"-F /etc/shadow", "--tab-file /etc/shadow", "--tab-file=/etc/shadow"},
		"lsusb *":     {"-D /etc/shadow"},
		"date *":      {"-f /etc/shadow", "--file=/etc/shadow"},
		"dmesg *":     {"-F /etc/shadow", "--file /etc/shadow"},
		"rpm *":       {"-qa --qf %{NAME:expand}", "-qa --queryformat=%(id)", "-qa --qf=x%(id)"},
	} {
		e := readsEngine(t, policy.ModePlan, entry)
		prog := entry[:len(entry)-2]
		for _, c := range calls {
			cmd := prog + " " + c
			if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
				t.Errorf("entry %q: %q ran in plan mode: %+v", entry, cmd, res)
			}
		}
	}
	e := readsEngine(t, policy.ModePlan, "file *", "wc *", "du *", "sha256sum *", "findmnt *", "rpm *")
	for _, cmd := range []string{"file -b /bin/ls", "wc -l /var/log/syslog", "du -sh /var/log", "sha256sum /bin/ls",
		"findmnt -n -o SOURCE /", "rpm -qa --qf '%{NAME}'"} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Allow {
			t.Errorf("%q, a read, was refused: %+v", cmd, res)
		}
	}
}

// ip's options come before its object, as ip reads them.
func TestSSHReadIPOptionsBeforeTheObject(t *testing.T) {
	e := readsEngine(t, policy.ModePlan, "ip -br addr show *", "ip addr show *")
	if res := e.Evaluate("ssh", true, sshCall("web1", "ip -br addr show dev eth0")); res.Decision != policy.Allow {
		t.Errorf("an option before the object was refused: %+v", res)
	}
	if res := e.Evaluate("ssh", true, sshCall("web1", "ip addr show dev eth0 -br")); res.Decision == policy.Allow {
		t.Errorf("an option after the object was read: %+v", res)
	}
}

// Runners are found in any case: the remote's names are case-sensitive, so
// this host folding case or not must not decide.
func TestSSHReadRunnersInAnyCase(t *testing.T) {
	for _, e := range []string{"R -e x", "Rscript x.R", "rscript x", "PYTHON3 -V", "Sudo id", "setpriv --reuid 0 id",
		"prlimit --nofile=1 id", "capsh --", "setarch x86_64 id", "linux64 id", "run-parts /etc/cron.d", "start-stop-daemon -S",
		"sshpass -p x ssh h", "m4 /etc/x", "dc -e 1", "julia -e 1", "ansible all -m ping", "runcon t id", "bwrap id",
		"fakeroot id", "chpst id", "proot id", "unbuffer id", "numactl id", "cgexec id", "firejail id", "script -c id"} {
		if _, err := policy.ParseRemoteRead(e); err == nil {
			t.Errorf("%q was accepted", e)
		}
	}
}
