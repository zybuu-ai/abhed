package remote

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// A wildcard entry loads only for a program on the read-only list, in a
// read form of it; every other program is declared by its exact command.
func TestSSHReadWildcardOnlyForKnownReads(t *testing.T) {
	refused := []string{
		// The reviewer's probes: programs that run commands or code.
		"socat *", "nc *", "ncat *", "netcat *", "java *", "java -jar *", "go run *", "go *", "sqlite3 *",
		"psql *", "mysql *", "mariadb *", "redis-cli *", "npm exec *", "npm *", "npx *", "cargo *",
		"/lib64/ld-linux-x86-64.so.2 *", "/lib/ld-musl-x86_64.so.1 *", "ld.so *", "ip netns exec *", "ip *",
		"sort --compress-program=*", "kubectl *", "curl *", "wget *", "find *", "find /var/log *", "tee *",
		"nginx -t *", "nginx -t", "mount *", "su *", "sudo *", "bash *", "python3 *", "awk *", "sed *", "tar *",
		// Listed programs, in a form that changes things or names no read verb.
		"systemctl *", "systemctl stop *", "systemctl restart *", "journalctl --vacuum-size=*", "journalctl --rotate *",
		"date -s *", "date --set=*", "hostname *", "dmesg -C *", "dmesg --clear *", "ss *", "sort -o *",
		"openssl x509 -out *", "openssl req *", "openssl *", "ip addr add *", "ip -batch *", "docker run *",
		"docker exec *", "docker *", "rpm -e *", "rpm -i *", "dpkg -i *", "dpkg --configure *", "apt install *",
		"rg --pre=* *", "file -C *", "hostnamectl set-hostname *", "timedatectl set-time *",
	}
	for _, e := range refused {
		if _, err := policy.ParseRemoteRead(e); err == nil {
			t.Errorf("%q was accepted", e)
		}
	}
	accepted := []string{
		"cat *", "tail -n 100 /var/log/*", "ls -la *", "df *", "du -sh *", "grep -r * /etc/nginx", "rg * /etc",
		"systemctl status *", "systemctl is-active *", "journalctl -u * -n 50", "ip addr show *", "ip -br link *",
		"ip route *", "openssl x509 -enddate -noout -in *", "openssl s_client -connect *", "date +*", "date *",
		"hostname -f", "dmesg -T *", "sort *", "docker ps *", "docker inspect *", "podman ps *",
		"rpm -qa *", "dpkg -l *", "apt list *", "sha256sum *", "uptime", "nproc",
	}
	for _, e := range accepted {
		if _, err := policy.ParseRemoteRead(e); err != nil {
			t.Errorf("%q: %v", e, err)
		}
	}
}

// A call a wildcard entry's pattern matches is a read only in a read form of
// its program: a dangerous option, a changing verb or an expansion that could
// become one is asked about, and in plan mode refused.
func TestSSHReadWildcardCallsInReadFormsOnly(t *testing.T) {
	e := readsEngine(t, policy.ModePlan, "journalctl *", "date *", "dmesg *", "sort *", "systemctl status *",
		"ip addr *", "openssl x509 *", "rg *", "file *", "docker ps *", "rpm *", "hostname -f", "cat *")
	for _, cmd := range []string{
		"journalctl --vacuum-size=1M", "journalctl --vacuum-time 1s", "journalctl --vac=1", "journalctl --rotate",
		"journalctl --flush", "journalctl --setup-keys", "journalctl --update-catalog",
		"date -s 2020-01-01", "date --set=x", "date --se x", "date -us x", "date 010100002020",
		"dmesg -C", "dmesg -c", "dmesg -Tc", "dmesg --clear", "dmesg --read-clear", "dmesg -n 1", "dmesg -D",
		"sort -o /etc/passwd x", "sort --output=/etc/passwd x", "sort --compress-program=sh x", "sort --compress-prog=sh x", "sort -T /tmp x",
		"systemctl status $(id)", "systemctl status nginx $X", "systemctl status *",
		"ip addr add 1.2.3.4/32 dev eth0", "ip addr del 1.2.3.4/32 dev eth0", "ip addr a 1.2.3.4 dev eth0", "ip addr flush dev eth0",
		"ip addr show -ba x",
		"openssl x509 -in a -out /etc/x", "openssl x509 --out /etc/x -in a", "openssl x509 -engine /tmp/e.so -in a", "openssl x509 -signkey k -in a",
		"rg --pre sh x", "rg --pre=sh x", "rg -z x", "rg --search-zip x",
		"file -C -m /tmp/x", "file --compile",
		"docker ps; docker run x",
		"rpm -qa --eval %(id)", "rpm -qe x",
		"hostname -F /etc/x", "hostname -b x",
	} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
			t.Errorf("%q ran in plan mode: %+v", cmd, res)
		}
	}
	for _, cmd := range []string{
		"journalctl -u nginx -n 50 --no-pager", "date +%s", "date -d yesterday +%F", "dmesg -T",
		"sort -k2 /etc/passwd", "systemctl status nginx", "ip addr show dev eth0", "ip addr show", "openssl x509 -enddate -noout -in /etc/ssl/a.pem",
		"rg -n listen /etc/nginx", "file /bin/ls", "docker ps -a", "rpm -qa", "hostname -f", "cat /etc/os-release",
	} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Allow {
			t.Errorf("%q, a read, was refused: %+v", cmd, res)
		}
	}
}
