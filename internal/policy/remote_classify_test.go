package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

func remoteCall(host, command string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"host": host, "command": command})
	return b
}

// declared is an engine whose operator declared app1, and nothing else.
func declared(mode Mode) *Engine {
	e := New(mode)
	e.RemoteHosts = func(h string) bool { return h == "app1" }
	return e
}

// withContents is declared with ssh.read_contents.
func withContents(mode Mode) *Engine {
	e := declared(mode)
	e.RemoteContents = true
	return e
}

// Metadata reads, and lines a metadata program pipes on, run unasked on a
// host the operator declared: the parity run's listings among them.
var remoteMetadataReads = []string{
	"ls -lah /var/log/app",
	"ls /var/log/app",
	"ls -lh /var/log/app/core*",
	"ls -la /root/.ssh",
	"du -ah /var/log/app | sort -rh | head -n 20",
	"du -sh /var/log/app/* | sort -hr",
	"du -sh /var/log | sort -h | tail -n 3",
	"find /var/log/app -name 'core*' -exec ls -lh {} +",
	"find /var/log/app -name core.1 -exec ls -l {} \\;",
	"find /var/log -type f -size +10M -mtime -2 -print",
	"find /var/log -maxdepth 1 -newermt 2026-11-01 -ls",
	"find /var/log -xdev -type f -name '*.log' -exec wc -l {} +",
	"find /var/log/app -exec stat -c '%s %n' {} +",
	"find / -xdev -type f -exec wc -c {} +",
	"find / -mount -type f -name '*.log' -exec file {} +",
	"find / -name core -exec ls -l {} +",
	"df -h",
	"df -hT /var",
	"free -m",
	"uptime",
	"ps aux | grep -i java | head",
	"ps -eo pid,rss,comm --sort=-rss | head -n 5",
	"systemctl status nginx -n 0 --no-pager",
	"systemctl status --lines=0 nginx",
	"systemctl status -n0 nginx",
	"systemctl is-active nginx",
	"findmnt",
	"lsusb",
	"wc -l /var/log/app/app.log",
	"wc -l /etc/shadow",
	"ls|sort",
	"ls /var/log | grep -c log",
	"ls /var/log | grep -e app -e sys",
	"stat -c '%s %n' /var/log/app/app.log",
	"docker ps -aq",
	"vmstat",
}

// Content reads run unasked only where the operator set ssh.read_contents.
var remoteContentReads = []string{
	"grep -i error /var/log/app/app.log | tail -n 20",
	"grep -c 'db pool' /var/log/app/app.log",
	"grep -rn 'pool exhausted' /var/log/app",
	"tail -n 50 /var/log/app/app.log",
	"head -n 5 /var/log/app/app.log",
	"journalctl -u nginx -n 100 --no-pager | grep -i error",
	"cat /etc/os-release",
	"sort -k2 -n /tmp/x | head",
	"find /var/log/app -xdev -type f -name '*.log' -exec tail -n 5 {} +",
	"rg --one-file-system -n pool /var/log/app",
	"grep -d recurse error /var/log/app",
	"grep -r error ~/app/logs",
	"grep x app.log",
	"docker logs -n 50 app",
	"docker inspect app",
	"systemctl cat nginx",
	"systemctl show nginx -p Environment",
	"dmesg -T | tail",
	"ls | grep -f /var/log/app/patterns",
	"ls | head /var/log/app/app.log",
	// systemctl status prints the unit's journal unless -n 0 and no -o.
	"systemctl status nginx --no-pager",
	"systemctl status app -n 100000",
	"systemctl status app --lines=100000",
	"systemctl status app -n 0 -o verbose",
	"systemctl status app -o json",
	"systemctl status -n 1000",
	// A checksum of a short secret gives it back to a guess.
	"md5sum /var/log/app/app.log",
	"sha256sum /etc/app/pin",
	"cksum /srv/app/build.tar",
	"find /root -mount -type f -exec md5sum {} +",
	// findmnt's fstab holds mount options, a CIFS password among them.
	"findmnt -s",
	"findmnt --fstab -o SOURCE,OPTIONS",
	"find / -xdev -type f -name '*.log' -exec tail -n 1 {} +",
	"grep -r error /var/log/app",
}

// Every evasion of the read-only form is not a read, whatever the opt-in:
// it asks in default mode and is refused in plan mode.
var remoteReadsRefused = []string{
	// Writes, deletes and runs.
	"rm /var/log/app/core.1",
	"rm -rf /var/log/app/core*",
	"find /var/log/app -name 'core*' -delete",
	"find /var/log/app -name core -exec rm {} +",
	"find /var/log/app -exec sh -c 'rm $1' _ {} \\;",
	"find /var/log/app -execdir rm {} \\;",
	"find /var/log/app -ok rm {} \\;",
	"find /var/log/app -fprint /tmp/out",
	"find /var/log/app -fls /tmp/out",
	"find /var/log/app -fprintf /tmp/out %p",
	"find /var/log/app -exec find / -delete \\;",
	"find /var/log/app -exec ls {} \\; -delete",
	"sort -o /etc/passwd /tmp/x",
	"sort --output=/etc/passwd /tmp/x",
	"sort -ro /etc/passwd /tmp/x",
	"sort -T /tmp /tmp/x",
	"sort --compress-program=sh /tmp/x",
	"du -ah /var/log | sort -rh -o /tmp/out",
	"ls | tee /tmp/out",
	"cat /etc/passwd | tee -a /etc/motd",
	"ls /var/log | xargs rm",
	"find /var/log -name core | xargs ls -l",
	"awk 'BEGIN{system(\"rm -rf /\")}'",
	"ls | awk '{print $1}'",
	"sed -i s/a/b/ /etc/hosts",
	"sed -n 1p /etc/hosts",
	"less /var/log/app/app.log",
	"more /var/log/app/app.log",
	"ls ! rm x",
	"! ls",
	// Redirections and lists.
	"ls > /tmp/out",
	"ls >> /tmp/out",
	"ls 2>/dev/null",
	"cat < /etc/shadow",
	"ls; rm x",
	"ls && rm x",
	"ls || rm x",
	"ls & rm x",
	"ls |& cat",
	"ls\nrm x",
	"(ls)",
	"{ ls; }",
	// Substitutions and expansions.
	"ls $(rm x)",
	"ls `rm x`",
	"ls ${HOME}",
	"ls $HOME",
	"ls \"$HOME\"",
	"ls \"`id`\"",
	"cat <(rm x)",
	"ls ~/{a,b}",
	"ls \"a\\b\"",
	"grep 'a\\' b' /etc/hosts",
	// Shell differences: fish's ^ stderr redirect, zsh's =prog, rc's double quotes, comments.
	"grep ^ERROR /var/log/app/app.log",
	"ls =rm",
	"grep \"x | rm -rf ~\" /etc/hosts",
	"grep \"a;b\" /etc/hosts",
	"ls # rm x",
	// Globs the shell could expand into an option.
	"ls *",
	"sort *.log",
	"ls -*",
	"find . -name x *",
	// Programs: off the list, by path, by runner, assigned, capitalised.
	"/tmp/evil/ls -la",
	"./ls",
	"LS -la",
	"sudo ls /root",
	"env ls",
	"command ls",
	"busybox ls",
	"FOO=bar ls",
	"timeout 5 ls",
	"nice ls",
	"echo hi",
	"curl http://example.com",
	"tar -cf /tmp/x.tar /etc",
	"python3 -c 'print(1)'",
	"vi /etc/hosts",
	// Read programs in a form that is not a read, busybox's included.
	"dmesg -c",
	"dmesg -n 1",
	"date -s 2020-01-01",
	"date -us '2020-01-01'",
	"hostname -F /etc/hostname",
	"hostname newname",
	"systemctl restart nginx",
	"journalctl --vacuum-size=1M",
	"journalctl --rotate",
	"ls --hide=x --format=long --color=never --dired=1",
	"file -C -m /tmp/magic",
	"wc --files0-from=/etc/shadow",
	"docker -H tcp://evil ps",
	"docker run alpine",
	"ip netns exec x rm y",
	"ps e",
	// Files that commonly hold credentials ask, as the read tool's do.
	"cat /etc/shadow",
	"cat /home/ops/.ssh/id_ed25519",
	"head /srv/app/.env",
	"grep -r password /srv/app/.env.production",
	"cat /etc/nginx/tls/site.key",
	"tail /home/ops/.aws/credentials",
	// Globs and other names for those files, and more of them.
	"cat /home/ops/.ss?/id_*",
	"tail -n +1 /home/ops/.ss?/*",
	"cat /etc/shado?",
	"cat /etc/shado[w]",
	"grep -f /etc/shado? x",
	"diff /etc/shado? /etc/hosts",
	"find / -type f -name 'id_*' -exec cat {} +",
	"find / -type f -regex '.*' -exec cat {} +",
	"cat /etc/ssh/ssh_host_ed25519_key",
	"cat /proc/1/environ",
	"cat /proc/*/env*",
	"cat /home/ops/.my.cnf",
	"cat ~/.npmrc",
	"cat /home/ops/.vault-token",
	"cat /home/ops/.config/gcloud/credentials.db",
	"cat /srv/tls/site.p12",
	"cat /home/ops/.bash_history",
	"cat /etc/kubernetes/admin.conf",
	"cat /var/log/app/*",
	// rc has no double quotes: these are more words, and options, there.
	`find /srv/data -name "x -o -fprint /home/ops/.profile -name y"`,
	`find /srv/data -name "x -o -delete -name y"`,
	`sort -k "1 -o /tmp/pwn" /etc/hosts`,
	`grep "error" /var/log/app/app.log`,
	// GNU find ends -exec … + only at {} +: this is one grep with more arguments.
	"find /tmp -exec grep + -o -exec ls {} +",
	// Devices: opening one can reboot the machine (watchdog), rewind a tape or toggle a line.
	"cat /dev/watchdog",
	"head -c 1 /dev/watchdog",
	"cat /*/watchdog",
	"wc -c /dev/st0",
	"grep -r -D read x /var",
	"grep -rD read x /var",
	"grep --devices=read -r x /var",
	"find /dev -exec head {} +",
	"find / -exec md5sum {} +",
	"find / -type f -o -name x -exec cat {} +",
	"find / -not -type f -exec wc {} +",
	"file -s /dev/sda",
	"ls /dev",
	// A find from / walks /proc and /sys unless -xdev keeps it on one device,
	// and a link followed can lead there.
	"find / -type f -exec wc -c {} +",
	"find / -exec file {} +",
	"find /. -type f -exec wc -c {} +",
	"find // -type f -exec file {} +",
	"find /var /./ -type f -exec md5sum {} +",
	"find -L /var/log -type f -exec wc -c {} +",
	// Any root may hold a proc mount: a login folder that is /, or a chroot.
	"find . -type f -exec wc -c {} +",
	"find ~ -type f -exec wc -c {} +",
	"find /srv -type f -exec wc -c {} +",
	"find /srv/chroot -type f -name '*.conf' -exec cat {} +",
	"find /var/log -type f -exec file {} +",
	"grep -d recurse x /",
	"grep --directories=recurse x /",
	"grep -drecurse x /.",
	"grep --directories recurse x //",
	"rg x /var/log/app",
	// The login folder may be /, so a recursive grep from it, or from a
	// path relative to it, may search /.
	"grep -r x .",
	"grep -R x ./",
	"grep -r x logs",
	"grep -rn x app/logs",
	"grep -d recurse x .",
	"grep --directories=recurse x ~",
	"grep -drecurse x ~/",
	"grep -r x /var/log/app .",
	"find -H /var/log -xdev -type f -exec wc -c {} +",
	"find /var/log -follow -type f -exec file {} +",
	"find /var/log -exec file {} +",
	"grep -r x /",
	"grep -R x /.",
	"rg x /",
	// Cleaned paths and /proc, /sys: /./dev is /dev, /proc/self/root/dev is too.
	"wc -c /./dev/watchdog",
	"wc -c //dev/watchdog",
	"md5sum /proc/self/root/dev/watchdog",
	"stat /proc/self/root/dev/watchdog",
	"wc -c /proc/kmsg",
	"file /proc/kmsg",
	"md5sum /proc/kcore",
	"find /proc -type f -exec md5sum {} +",
	"find /proc -type f -exec ls {} +",
	"ls /sys/class",
	"ls /p?oc",
	// Checksums of credential files ask even under read_contents.
	"md5sum /etc/shadow",
	"cksum /root/.ssh/id_rsa",
	// Repeats or rewrites a cache.
	"free -c 100000",
	"free --count=3",
	"vmstat -c 3",
	"apt list --installed",
	"apt-cache policy nginx",
	"lsusb -v",
	// Automounted network paths, and climbing out.
	"ls /net/secret.example.com",
	"ls /misc/x",
	"cat ../../dev/watchdog",
	"ls ..",
	"ls /srv/../dev",
	// Never ends: follows, repeats, or reads the command's input.
	"tail -f /var/log/app/app.log",
	"tail -F /var/log/app/app.log",
	"tail --follow=name /var/log/app/app.log",
	"tail -n 5 --retry /var/log/app/app.log",
	"tail -fn 5 /var/log/app/app.log",
	"journalctl -f",
	"journalctl -u nginx --follow",
	"dmesg -w",
	"dmesg -W",
	"docker logs -f app",
	"free -s 1",
	"vmstat 1",
	"cat",
	"cat -",
	"grep error",
	"wc -l",
	"ls | cat /var/log/app/app.log -",
	// Control characters, quoted or not.
	"ls 'a\x01b'",
	"ls '\x1b[2J'",
	"ls\x00",
	"ls\tfoo\x7f",
	// Reads that reach off the host.
	"getent hosts secret.example.com",
	"openssl s_client -connect example.com:443",
	// Unclosed or empty.
	"ls 'a",
	"ls \"a",
	"",
	"ls |",
}

func TestRemoteReadOnlyClassifier(t *testing.T) {
	for _, e := range []*Engine{declared(ModeDefault), withContents(ModeDefault)} {
		for _, cmd := range remoteMetadataReads {
			res := e.Evaluate("ssh", true, remoteCall("app1", cmd))
			if res.Decision != Allow || res.Rule != ReadOnlyRule || !strings.Contains(res.Reason, ReadOnlyRule) {
				t.Errorf("%q on a declared host (contents %v): %+v", cmd, e.RemoteContents, res)
			}
		}
		for _, cmd := range remoteReadsRefused {
			if res := e.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision == Allow {
				t.Errorf("%q was allowed (contents %v): %+v", cmd, e.RemoteContents, res)
			}
			if _, why := ClassifyRemoteRead(cmd, e.RemoteContents); why == "" {
				t.Errorf("%q classified as a read (contents %v)", cmd, e.RemoteContents)
			}
		}
	}
}

// A program that prints files runs unasked only under ssh.read_contents,
// and says so when it asks.
func TestRemoteContentReadsNeedTheOptIn(t *testing.T) {
	off, on := declared(ModeDefault), withContents(ModeDefault)
	for _, cmd := range remoteContentReads {
		res := off.Evaluate("ssh", true, remoteCall("app1", cmd))
		if res.Decision != Ask {
			t.Errorf("%q without read_contents: %+v", cmd, res)
		}
		if _, why := ClassifyRemoteRead(cmd, false); !strings.Contains(why, "ssh.read_contents") {
			t.Errorf("%q: %q does not name ssh.read_contents", cmd, why)
		}
		if res := on.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision != Allow || res.Rule != ReadOnlyRule {
			t.Errorf("%q with read_contents: %+v", cmd, res)
		}
	}
}

// Plan mode follows the same rules: metadata (and content under the opt-in)
// is allowed, the rest refused.
func TestRemoteReadOnlyInPlanMode(t *testing.T) {
	off, on := declared(ModePlan), withContents(ModePlan)
	for _, cmd := range remoteMetadataReads {
		if res := off.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision != Allow || res.Step != "mode" {
			t.Errorf("%q in plan mode: %+v", cmd, res)
		}
	}
	for _, cmd := range remoteContentReads {
		if res := off.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision != Deny {
			t.Errorf("%q in plan mode without read_contents: %+v", cmd, res)
		}
		if res := on.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision != Allow {
			t.Errorf("%q in plan mode with read_contents: %+v", cmd, res)
		}
	}
	for _, cmd := range remoteReadsRefused {
		if res := on.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision != Deny {
			t.Errorf("%q in plan mode: %+v", cmd, res)
		}
	}
}

// A host the operator did not declare, such as one ssh_connect added, keeps
// asking for every command; so does every host when the classifier is off.
func TestRemoteReadOnlyOnlyOnDeclaredHosts(t *testing.T) {
	e := withContents(ModeDefault)
	if res := e.Evaluate("ssh", true, remoteCall("vm-added", "ls -lah /var/log")); res.Decision != Ask {
		t.Errorf("undeclared host: %+v", res)
	}
	off := New(ModeDefault)
	if res := off.Evaluate("ssh", true, remoteCall("app1", "ls -lah /var/log")); res.Decision != Ask {
		t.Errorf("classifier off: %+v", res)
	}
	if res := New(ModePlan).Evaluate("ssh", true, remoteCall("app1", "ls")); res.Decision != Deny {
		t.Errorf("classifier off in plan mode: %+v", res)
	}
}

// Deny and ask rules, hooks, a stored secret on stdin and bypass come first,
// as for a declared read.
func TestRemoteReadOnlyRulesFirst(t *testing.T) {
	e := withContents(ModeDefault)
	if err := e.AddDeny("ssh(app1/cat /etc/os-release*)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAsk("ssh(*journalctl*)"); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate("ssh", true, remoteCall("app1", "cat /etc/os-release")); res.Decision != Deny {
		t.Errorf("deny rule: %+v", res)
	}
	if res := e.Evaluate("ssh", true, remoteCall("app1", "journalctl -n 5")); res.Decision != Ask {
		t.Errorf("ask rule: %+v", res)
	}
	secret, _ := json.Marshal(map[string]string{"host": "app1", "command": "ls", "stdin_secret": "PW"})
	if res := e.Evaluate("ssh", true, secret); res.Decision != Ask {
		t.Errorf("stdin secret: %+v", res)
	}
	if got := e.RemoteReadOf("ssh", secret); got != "" {
		t.Errorf("stdin secret read as %q", got)
	}
	if got := e.RemoteReadOf("ssh", remoteCall("app1", "ls -la")); got != ReadOnlyRule {
		t.Errorf("RemoteReadOf = %q", got)
	}
	// A hook's ask and its refusal both hold over the classifier, in plan mode too.
	for _, d := range []Decision{Ask, Deny} {
		for _, mode := range []Mode{ModeDefault, ModePlan} {
			h := declared(mode)
			h.Hooks = []Hook{func(string, json.RawMessage) *Result { return &Result{Decision: d, Reason: "hook says"} }}
			res := h.Evaluate("ssh", true, remoteCall("app1", "ls -la"))
			if res.Decision == Allow || mode == ModeDefault && res.Decision != d {
				t.Errorf("hook %s in %s: %+v", d, mode, res)
			}
		}
	}
}

// read_commands stays an extra allow list, and names its entry as the rule.
func TestRemoteReadCommandsStillExtra(t *testing.T) {
	e := declared(ModeDefault)
	if err := e.AllowRemoteReads("uptime -p"); err != nil {
		t.Fatal(err)
	}
	// On a host ssh_connect added, only the list applies.
	res := e.Evaluate("ssh", true, remoteCall("vm-added", "uptime -p"))
	if res.Decision != Allow || res.Rule != "ssh.read_commands: uptime -p" {
		t.Errorf("read_commands entry: %+v", res)
	}
	if res := e.Evaluate("ssh", true, remoteCall("vm-added", "uptime")); res.Decision != Ask {
		t.Errorf("undeclared host beyond the list: %+v", res)
	}
}

// A refusal says why, so the model and the person can see what would pass.
func TestRemoteReadOnlyExplains(t *testing.T) {
	for cmd, want := range map[string]string{
		"sort -o x y":                     "sort's read form takes",
		"ls > x":                          "redirection",
		"sort *.log":                      "./*.log",
		"find . -delete":                  "-delete is not a read",
		"ls | tee x":                      "tee is not on the read-only list",
		"systemctl stop x":                "its read verbs are",
		"getent hosts a.b.c":              "DNS",
		`grep "x" /etc/hosts`:             "rc",
		"cat /dev/watchdog":               "/dev",
		"tail -f /var/log/x":              "until it is stopped",
		"cat /etc/shado?":                 "credentials",
		"find / -exec md5sum {} +":        "-type f",
		"find / -type f -exec wc -c {} +": "-xdev",
		"find . -type f -exec wc -c {} +": "-xdev",
		"rg x /var/log":                   "--one-file-system",
		"grep -d recurse x /":             "from /",
		"grep -r x .":                     "login folder",
		"grep -r x /":                     "from /",
		"find / -type f -exec ls + {}":    "{} +",
		// One grep with + -o -exec ls {} as its arguments, as GNU find reads it.
		"find /tmp -exec grep + -o -exec ls {} +": "-exec of grep",
	} {
		if _, why := ClassifyRemoteRead(cmd, true); !strings.Contains(why, want) {
			t.Errorf("%q: %q, want %q in it", cmd, why, want)
		}
	}
}

// An entry refused at load names the forms that would be accepted.
func TestReadCommandsLoadErrorNamesForms(t *testing.T) {
	_, err := ParseRemoteRead("df -h*")
	if err == nil {
		t.Fatal("df -h* loaded")
	}
	for _, want := range []string{`"df -h"`, `"df -h *"`, `"df --output=*"`, `"-h*"`, "-h", "--human-readable", "declare the exact command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %s: %v", want, err)
		}
	}
	if _, err := ParseRemoteRead("sort -o /tmp/x"); err == nil || !strings.Contains(err.Error(), "--numeric-sort") {
		t.Errorf("sort -o: %v", err)
	}
}

// A content read refused for want of ssh.read_contents says so, and how the
// operator turns it on, instead of that it can make changes. What is allowed
// does not move: it still asks, and a write still names the change.
func TestRemoteContentAskNamesTheRule(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
		res := declared(mode).Evaluate("ssh", true, remoteCall("app1", "tail -n 100 /var/log/app/app.log"))
		if res.Decision != Ask {
			t.Fatalf("%s: a content read is no longer asked: %+v", mode, res)
		}
		for _, want := range []string{"tail prints the contents of files", "ssh.read_contents: true", "ssh.read_commands", string(mode) + " mode"} {
			if !strings.Contains(res.Reason, want) {
				t.Errorf("%s: reason %q lacks %q", mode, res.Reason, want)
			}
		}
		if strings.Contains(res.Reason, "can make changes") {
			t.Errorf("%s: a read is said to make changes: %q", mode, res.Reason)
		}
	}
	res := declared(ModeDefault).Evaluate("ssh", true, remoteCall("app1", "systemctl restart app"))
	if res.Decision != Ask || !strings.Contains(res.Reason, "can make changes") || !strings.Contains(res.Reason, "its read verbs are") {
		t.Errorf("a change is not explained as one: %+v", res)
	}
	// An undeclared host is not judged by the classifier, so its reason is unchanged.
	res = declared(ModeDefault).Evaluate("ssh", true, remoteCall("other", "tail -n 1 /x"))
	if res.Decision != Ask || strings.Contains(res.Reason, "read_contents") {
		t.Errorf("undeclared host: %+v", res)
	}
}

// A wildcard in ssh.read_commands stands for one argument: it never names a
// further file, and never a .. segment the entry does not write.
func TestReadCommandWildcardIsOneArgument(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AllowRemoteReads("tail -n * /var/log/app/app.log", "cat /var/log/*", "grep -i * /var/log/app/app.log"); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]Decision{
		"tail -n 100 /var/log/app/app.log":              Allow,
		"cat /var/log/app/x.log":                        Allow,
		"grep -i 'pool exhausted' /var/log/app/app.log": Allow,
		"tail -n 100 /etc/shadow /var/log/app/app.log":  Ask,
		"tail -n 100 -c 5 /var/log/app/app.log":         Ask,
		"cat /var/log/a /etc/shadow":                    Ask,
		"cat /var/log/../../etc/shadow":                 Ask,
		"grep -i x /etc/passwd /var/log/app/app.log":    Ask,
		"tail -n /var/log/app/app.log":                  Ask,
		// A word the remote shell expands is not the word the entry matched.
		"cat /var/log/.[.]/.[.]/etc/shadow":    Ask,
		"cat /var/log/.?/.?/etc/shadow":        Ask,
		"cat /var/log/{app,..}/x":              Ask,
		"cat /var/log/~root":                   Ask,
		"cat /var/log/$HOME":                   Ask,
		"grep -i /etc/s* /var/log/app/app.log": Ask,
		"grep -i 'a*' /var/log/app/app.log":    Allow,
		// An operand slot never takes an option; a value slot is as before.
		"grep -i --file=/etc/shadow /var/log/app/app.log": Ask,
		"grep -i -f/etc/shadow /var/log/app/app.log":      Ask,
		"grep -i -r /var/log/app/app.log":                 Ask,
		"grep -i -- -r /var/log/app/app.log":              Ask,
		"tail -n +5 /var/log/app/app.log":                 Allow,
		// The entry's own glob, as written, is the operator's.
		"cat /var/log/*": Allow,
	} {
		if got := e.Evaluate("ssh", true, remoteCall("app1", cmd)); got.Decision != want {
			t.Errorf("%q: %s (%s), want %s", cmd, got.Decision, got.Reason, want)
		}
	}
}

// A word that is ** alone stands for one or more whole arguments, each still
// judged as a read form; it loads only where * would.
func TestReadCommandDoubleStarIsWholeArguments(t *testing.T) {
	e := New(ModeDefault)
	e.ReadCommandsContents = true
	if err := e.AllowRemoteReads("systemctl status **", "cat **", "tail **"); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]Decision{
		"systemctl status nginx --no-pager -l": Allow,
		"systemctl status":                     Ask,
		"systemctl stop nginx":                 Ask,
		"cat /etc/os-release /etc/hostname":    Allow,
		"cat /var/log/../../etc/shadow":        Ask,
		"tail .[.]":                            Ask,
		"cat .?/.?":                            Ask,
		"cat /etc/os-release /etc/s*":          Ask,
	} {
		if got := e.Evaluate("ssh", true, remoteCall("app1", cmd)); got.Decision != want {
			t.Errorf("%q: %s (%s), want %s", cmd, got.Decision, got.Reason, want)
		}
	}
	for _, p := range []string{"systemctl **", "sudo **", "socat **"} {
		if _, err := ParseRemoteRead(p); err == nil {
			t.Errorf("%q was accepted", p)
		}
	}
}
