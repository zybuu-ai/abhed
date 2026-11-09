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

// The commands the parity run on an operator's host was refused, and the
// everyday reads beside them, run unasked on a host the operator declared.
var remoteReadsAllowed = []string{
	"ls -lah /var/log/app",
	"ls /var/log/app",
	"ls -lh /var/log/app/core*",
	"du -ah /var/log/app | sort -rh | head -n 20",
	"du -sh /var/log/app/* | sort -hr",
	"find /var/log/app -name 'core*' -exec ls -lh {} +",
	"find /var/log/app -name core.1 -exec ls -l {} \\;",
	"find /var/log -type f -size +10M -mtime -2 -print",
	"find /var/log -maxdepth 1 -newermt 2026-11-01 -ls",
	"grep -i error /var/log/app/app.log | tail -n 20",
	"grep -c 'db pool' /var/log/app/app.log",
	"grep -rn \"pool exhausted\" /var/log/app",
	"tail -n 50 /var/log/app/app.log",
	"df -h",
	"df -hT /var",
	"free -m",
	"uptime",
	"ps aux | grep -i java | head",
	"ps -eo pid,rss,comm --sort=-rss | head -n 5",
	"systemctl status nginx --no-pager",
	"journalctl -u nginx -n 100 --no-pager | grep -i error",
	"cat /etc/os-release",
	"wc -l /var/log/app/app.log",
	"sort -k2 -n /tmp/x | head",
	"ls|sort",
	"stat -c '%s %n' /var/log/app/app.log",
	"docker ps -aq",
}

// Every evasion of the read-only form is not a read: it asks in default
// mode and is refused in plan mode, as before the classifier.
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
	"ls -la /root/.ssh",
	"head /srv/app/.env",
	"grep -r password /srv/app/.env.production",
	"cat /etc/nginx/tls/site.key",
	"tail /home/ops/.aws/credentials",
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
	e := declared(ModeDefault)
	for _, cmd := range remoteReadsAllowed {
		res := e.Evaluate("ssh", true, remoteCall("app1", cmd))
		if res.Decision != Allow || res.Rule != ReadOnlyRule || !strings.Contains(res.Reason, ReadOnlyRule) {
			t.Errorf("%q on a declared host: %+v", cmd, res)
		}
	}
	for _, cmd := range remoteReadsRefused {
		res := e.Evaluate("ssh", true, remoteCall("app1", cmd))
		if res.Decision == Allow {
			t.Errorf("%q was allowed: %+v", cmd, res)
		}
		if _, why := ClassifyRemoteRead(cmd); why == "" {
			t.Errorf("%q classified as a read", cmd)
		}
	}
}

// Plan mode allows a classified read and still refuses the rest.
func TestRemoteReadOnlyInPlanMode(t *testing.T) {
	e := declared(ModePlan)
	for _, cmd := range remoteReadsAllowed {
		if res := e.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision != Allow || res.Step != "mode" {
			t.Errorf("%q in plan mode: %+v", cmd, res)
		}
	}
	for _, cmd := range remoteReadsRefused {
		if res := e.Evaluate("ssh", true, remoteCall("app1", cmd)); res.Decision != Deny {
			t.Errorf("%q in plan mode: %+v", cmd, res)
		}
	}
}

// A host the operator did not declare, such as one ssh_connect added, keeps
// asking for every command; so does every host when the classifier is off.
func TestRemoteReadOnlyOnlyOnDeclaredHosts(t *testing.T) {
	e := declared(ModeDefault)
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

// Deny and ask rules, a stored secret on stdin and bypass come first, as
// for a declared read.
func TestRemoteReadOnlyRulesFirst(t *testing.T) {
	e := declared(ModeDefault)
	if err := e.AddDeny("ssh(app1/cat /etc/shadow*)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAsk("ssh(*journalctl*)"); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate("ssh", true, remoteCall("app1", "cat /etc/shadow")); res.Decision != Deny {
		t.Errorf("deny rule: %+v", res)
	}
	if res := e.Evaluate("ssh", true, remoteCall("app1", "journalctl -n 5")); res.Decision != Ask {
		t.Errorf("ask rule: %+v", res)
	}
	secret, _ := json.Marshal(map[string]string{"host": "app1", "command": "cat", "stdin_secret": "PW"})
	if res := e.Evaluate("ssh", true, secret); res.Decision != Ask {
		t.Errorf("stdin secret: %+v", res)
	}
	if got := e.RemoteReadOf("ssh", secret); got != "" {
		t.Errorf("stdin secret read as %q", got)
	}
	if got := e.RemoteReadOf("ssh", remoteCall("app1", "ls -la")); got != ReadOnlyRule {
		t.Errorf("RemoteReadOf = %q", got)
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
		"sort -o x y":        "sort's read form takes",
		"ls > x":             "redirection",
		"sort *.log":         "./*.log",
		"find . -delete":     "-delete is not a read",
		"ls | tee x":         "tee is not on the read-only list",
		"systemctl stop x":   "its read verbs are",
		"getent hosts a.b.c": "DNS",
	} {
		if _, why := ClassifyRemoteRead(cmd); !strings.Contains(why, want) {
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
