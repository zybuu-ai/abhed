package remote

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// The round-3 probes: an option before a verb, whose value was read as the
// verb, makes an entry that is not a read form, so it is refused at load.
func TestSSHReadVerbIsTheFirstWord(t *testing.T) {
	for _, e := range []string{
		"systemctl -p status *", "systemctl --property status *", "systemctl --no-pager status *",
		"docker --config ps *", "docker -l ps *", "docker -H x ps *", "podman --url x ps *",
		"apt -t list *", "apt -o x list *", "timedatectl -M show *", "hostnamectl -H x status *",
		"openssl -in x509 *",
	} {
		if _, err := policy.ParseRemoteRead(e); err == nil {
			t.Errorf("%q was accepted", e)
		}
	}
	e := readsEngine(t, policy.ModePlan, "systemctl status *", "docker ps *", "apt list *", "timedatectl show *")
	for _, cmd := range []string{
		"systemctl status -p status stop nginx --now", "systemctl status nginx -H host", "systemctl status nginx --root=/x",
		"docker ps --config x rm x", "docker ps -H tcp://x", "apt list -o APT::Update::Pre-Invoke::=sh", "apt list -c /tmp/x",
		"timedatectl show -M m", "timedatectl show --host x",
	} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
			t.Errorf("%q ran in plan mode: %+v", cmd, res)
		}
	}
	for _, cmd := range []string{"systemctl status nginx --no-pager -l", "systemctl status -p MainPID nginx", "docker ps -a --format x", "apt list --installed", "timedatectl show -p NTP"} {
		if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Allow {
			t.Errorf("%q, a read, was refused: %+v", cmd, res)
		}
	}
}

// For each form, every option its manual page gives as writing a file,
// running a program or changing the host is refused through a wildcard
// entry, written in full, abbreviated, glued or clustered.
func TestSSHReadFormsRefuseWritersAndRunners(t *testing.T) {
	forms := map[string][]string{
		"journalctl *": {"--vacuum-size=1M", "--vacuum-size 1M", "--vacuum-time=1s", "--vacuum-files=1", "--vac=1",
			"--rotate", "--rot", "--flush", "--sync", "--relinquish-var", "--smart-relinquish-var", "--setup-keys",
			"--setup-keys --force", "--interval=1", "--update-catalog", "--cursor-file=/tmp/x", "--cursor-file /tmp/x",
			"--cursor-f=/tmp/x", "-n --vacuum-size=1"},
		"openssl x509 *": {"-in a -out /tmp/x", "-in a --out /tmp/x", "-in a -keyout /tmp/k", "-in a -CAcreateserial",
			"-in a -CAserial /tmp/s", "-in a -CA ca.pem -CAkey k", "-in a -signkey k", "-in a -req", "-in a -set_serial 1",
			"-in a -engine /tmp/e.so", "-in a -provider p", "-in a -provider-path /tmp", "-in a -config /tmp/c",
			"-in a -writerand /tmp/r", "-in a -x509toreq", "-in a -extfile /tmp/e", "-in a -passin pass:x"},
		"openssl s_client *": {"-connect h:443 -sess_out /tmp/s", "-connect h:443 -keylogfile /tmp/k",
			"-connect h:443 -msgfile /tmp/m", "-connect h:443 -engine e", "-connect h:443 -provider p",
			"-connect h:443 -writerand /tmp/r", "-connect h:443 -ssl_config c"},
		"docker ps *":      {"-H tcp://x", "--host x", "--config /tmp", "--context x"},
		"docker inspect *": {"x -H tcp://x", "x --host x"},
		"docker logs *":    {"x --host x", "x -H y"},
		"apt list *":       {"-o APT::Update::Pre-Invoke::=sh", "-c /tmp/conf", "--option x", "--config-file x"},
		"apt-cache show *": {"x -o Dir::Etc=x", "x -c /tmp/c"},
		"dpkg *": {"-i x.deb", "--install x.deb", "-r x", "--remove x", "-P x", "--purge x", "--configure x", "--unpack x",
			"-a", "--set-selections", "--clear-selections", "--root /x", "--admindir /x", "--force-all -l", "--log /tmp/x -l",
			"--instdir /x -l", "-li"},
		"dpkg-query *": {"--admindir /x -l", "--load-avail -l"},
		"rpm *": {"-i x.rpm", "-U x.rpm", "-F x.rpm", "-e x", "--erase x", "--import k", "--setperms x", "--setugids x",
			"--restore x", "--rebuilddb", "--initdb", "--eval x", "-E x", "--define x", "-D x", "--dbpath /x -qa", "--root /x -qa",
			"--pipe sh -qa", "--macros /x -qa", "--rcfile /x -qa", "-qe x", "-qap"},
		"ip addr *": {"add 1.2.3.4/32 dev eth0", "del 1.2.3.4/32 dev eth0", "a 1.2.3.4 dev eth0", "flush dev eth0",
			"change 1.2.3.4 dev eth0", "replace 1.2.3.4 dev eth0", "show -ba x", "show vrf x"},
		"ip link show *": {"-batch /tmp/x", "-b /tmp/x", "-ba /tmp/x", "-force", "-n ns", "-netns ns", "-net ns",
			"netns exec x id", "vrf exec x id", "dev eth0 exec id"},
		"date *": {"-s 2020-01-01", "--set 2020-01-01", "--set=2020", "--se x", "-us x", "010100002020", "-f /tmp/x"},
		"dmesg *": {"-C", "-c", "--clear", "--read-clear", "-D", "--console-off", "-E", "--console-on", "-n 1",
			"--console-level 1", "-Tc", "--cl", "-s 1 -c"},
		"file *": {"-C", "--compile", "-C -m /tmp/x", "-z x", "-Z x", "--uncompress x", "-p x", "--preserve-date x"},
		"rg *":   {"--pre sh x", "--pre=sh x", "--pr=sh x", "--pre-glob '*' x", "-z x", "--search-zip x", "-zi x"},
		"sort *": {"-o /tmp/x a", "--output=/tmp/x a", "--output /tmp/x a", "--out=/tmp/x a", "--compress-program=sh a",
			"--compress-prog=sh a", "-T /tmp a", "--temporary-directory=/tmp a", "--files0-from=/tmp/x", "-no /tmp/x a"},
		"hostname -f":          {},
		"timedatectl show *":   {"-M m", "--host x", "--adjust-system-clock"},
		"hostnamectl status *": {"--host x", "-M m"},
	}
	for entry, bad := range forms {
		e := readsEngine(t, policy.ModePlan, entry)
		prefix := strings.TrimSuffix(entry, "*")
		for _, opt := range bad {
			cmd := prefix + opt
			if res := e.Evaluate("ssh", true, sshCall("web1", cmd)); res.Decision != policy.Deny {
				t.Errorf("entry %q: %q ran in plan mode: %+v", entry, cmd, res)
			}
		}
	}
	// And the verbs that change things are refused as entries.
	for _, e := range []string{
		"systemctl stop *", "systemctl start *", "systemctl restart *", "systemctl kill *", "systemctl enable *",
		"systemctl disable *", "systemctl mask *", "systemctl edit *", "systemctl set-property *", "systemctl daemon-reload *",
		"systemctl isolate *", "systemctl reboot *", "systemctl link *", "systemctl set-environment *", "systemctl revert *",
		"docker run *", "docker exec *", "docker rm *", "docker kill *", "docker cp *", "docker build *", "docker pull *",
		"docker system *", "docker volume *", "docker network *", "docker compose *", "docker plugin *",
		"apt install *", "apt remove *", "apt purge *", "apt update *", "apt upgrade *", "apt edit-sources *",
		"apt-cache gencaches *", "timedatectl set-time *", "timedatectl set-ntp *", "hostnamectl set-hostname *",
		"openssl req *", "openssl genrsa *", "openssl enc *", "openssl ca *", "openssl pkcs12 *", "hostname *",
		"ip *", "ip -batch *", "ip netns *", "ip link set *", "ip route add *",
	} {
		if _, err := policy.ParseRemoteRead(e); err == nil {
			t.Errorf("%q was accepted", e)
		}
	}
}

// A runner named with a digit in its name is found as written, not only
// with the digits dropped: sqlite3 is not sqlite.
func TestSSHReadRunnerByItsOwnName(t *testing.T) {
	for _, e := range []string{"sqlite3 /x .tables", "python3 -V", "python3.12 -V", "pip3 list", "lua5.4 -v", "perl5.36 -v"} {
		if _, err := policy.ParseRemoteRead(e); err == nil {
			t.Errorf("%q was accepted", e)
		}
	}
}
