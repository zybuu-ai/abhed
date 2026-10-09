package sandboxcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/controlplane"
)

// sshInfo is POST :ssh's answer.
type sshInfo struct {
	HostAlias   string `json:"host_alias"`
	User        string `json:"user"`
	HostKey     string `json:"host_key"`
	Fingerprint string `json:"fingerprint"`
	KnownHosts  string `json:"known_hosts"`
	Relay       string `json:"relay"`
	Subprotocol string `json:"subprotocol"`
	Generation  int64  `json:"generation"`
}

// sshInfo asks the server how to reach a sandbox over SSH, and checks the
// answer is one known_hosts line for the sandbox's alias.
func (r *runner) sshInfo(id string) (sshInfo, error) {
	var info sshInfo
	if _, err := r.cl.json(r.Ctx, call{method: http.MethodPost, path: sandboxPath(id, "ssh")}, &info); err != nil {
		return info, err
	}
	if info.HostAlias != "abhed-"+id || info.User == "" || strings.ContainsAny(info.User, " \t\r\n%'\"") {
		return info, fmt.Errorf("the server's SSH details for %s are not for that sandbox", id)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(info.HostKey)); err != nil {
		return info, fmt.Errorf("the server's SSH host key could not be read: %w", err)
	}
	if strings.ContainsAny(info.KnownHosts, "\r\n") || !strings.HasPrefix(info.KnownHosts, info.HostAlias+" ") {
		return info, errors.New("the server's known_hosts line is not one line for the sandbox")
	}
	return info, nil
}

// trustHostKeys writes each sandbox's host key line to the known_hosts
// file, replacing what it held for the same alias, so ssh checks the key
// the server gave over its authenticated connection.
func (r *runner) trustHostKeys(infos []sshInfo) error {
	if r.KnownHosts == "" {
		return errors.New("no known_hosts file to keep the sandbox's host key in")
	}
	dir := filepath.Dir(r.KnownHosts)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var kept []string
	if fi, err := os.Lstat(r.KnownHosts); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular file, not a link", r.KnownHosts)
		}
		b, err := os.ReadFile(r.KnownHosts)
		if err != nil {
			return err
		}
		replaced := map[string]bool{}
		for _, in := range infos {
			replaced[in.HostAlias] = true
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := sc.Text()
			host, _, _ := strings.Cut(strings.TrimSpace(line), " ")
			if !replaced[host] {
				kept = append(kept, line)
			}
		}
	}
	for _, in := range infos {
		kept = append(kept, in.KnownHosts)
	}
	tmp, err := os.CreateTemp(dir, ".known_hosts.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.WriteString(strings.Join(kept, "\n") + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, r.KnownHosts)
}

// shellQuote quotes s for the POSIX shell ssh runs a ProxyCommand with.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:,@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// proxyCommand is the ProxyCommand that reaches id as this profile. ssh
// expands % in it, so a literal one is doubled.
func (r *runner) proxyCommand(id string) (string, error) {
	exe := r.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", err
		}
	}
	cmd := shellQuote(exe) + " sandbox connect --stdio -profile " + shellQuote(r.cl.name) + " " + id
	return strings.ReplaceAll(cmd, "%", "%%"), nil
}

// sshOptions are the options every ssh to a sandbox runs with: the relay
// as its ProxyCommand and the host key the server gave, checked strictly.
func (r *runner) sshOptions(info sshInfo, id string) ([]string, error) {
	proxy, err := r.proxyCommand(id)
	if err != nil {
		return nil, err
	}
	return []string{
		"-o", "ProxyCommand=" + proxy,
		"-o", "UserKnownHostsFile=" + strings.ReplaceAll(r.KnownHosts, "%", "%%"),
		"-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlias=" + info.HostAlias,
		"-o", "CheckHostIP=no",
		"-l", info.User,
	}, nil
}

func (r *runner) ssh(args []string) error {
	fs := r.flags("ssh", "ID [-- SSH ARGS]")
	pos, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageError{errors.New("ssh takes a sandbox id")}
	}
	id, err := oneID(pos[:1], "ssh")
	if err != nil {
		return err
	}
	if r.SSH == nil {
		return errors.New("no ssh program to run")
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	info, err := r.sshInfo(id)
	if err != nil {
		return err
	}
	if err := r.trustHostKeys([]sshInfo{info}); err != nil {
		return err
	}
	argv, err := r.sshOptions(info, id)
	if err != nil {
		return err
	}
	argv = append(append(argv, info.HostAlias), pos[1:]...)
	code, err := r.SSH(r.Ctx, argv)
	if err != nil {
		return err
	}
	return exitStatus(code)
}

// sshHost is one Host block of ssh-config.
type sshHost struct {
	Host           string `json:"host"`
	SandboxID      string `json:"sandbox_id"`
	User           string `json:"user"`
	ProxyCommand   string `json:"proxy_command"`
	KnownHostsFile string `json:"known_hosts_file"`
	Fingerprint    string `json:"fingerprint"`
}

func (r *runner) sshConfig(args []string) error {
	fs := r.flags("ssh-config", "[ID...] [-l SELECTOR]")
	sel := fs.String("l", "", "every sandbox matching this label selector (default with no ids: all Ready ones)")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	for _, id := range pos {
		if !controlplane.ValidID(id) {
			return usageError{fmt.Errorf("%q is not a sandbox id", id)}
		}
	}
	if len(pos) > 0 && *sel != "" {
		return usageError{errors.New("ssh-config takes ids or -l, not both")}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	ids := pos
	if len(ids) == 0 {
		q := url.Values{"state": {string(controlplane.StateReady)}}
		if *sel != "" {
			q.Set("selector", *sel)
		}
		for after := ""; ; {
			if after != "" {
				q.Set("after", after)
			}
			var page sandboxList
			if _, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: "/v1/sandboxes?" + q.Encode()}, &page); err != nil {
				return err
			}
			for _, sb := range page.Items {
				ids = append(ids, sb.ID)
			}
			if page.Next == "" {
				break
			}
			after = page.Next
		}
	}
	var infos []sshInfo
	var hosts []sshHost
	for _, id := range ids {
		info, err := r.sshInfo(id)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		proxy, err := r.proxyCommand(id)
		if err != nil {
			return err
		}
		infos = append(infos, info)
		hosts = append(hosts, sshHost{Host: info.HostAlias, SandboxID: id, User: info.User, ProxyCommand: proxy,
			KnownHostsFile: r.KnownHosts, Fingerprint: info.Fingerprint})
	}
	if len(infos) > 0 {
		if err := r.trustHostKeys(infos); err != nil {
			return err
		}
	}
	if r.out == "json" {
		if hosts == nil {
			hosts = []sshHost{}
		}
		enc := json.NewEncoder(r.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(hosts)
	}
	tw := tabwriter.NewWriter(r.Out, 0, 0, 1, ' ', 0)
	for i, h := range hosts {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		fmt.Fprintf(tw, "# abhed sandbox %s (%s)\n", h.SandboxID, h.Fingerprint)
		fmt.Fprintf(tw, "Host %s\n", h.Host)
		fmt.Fprintf(tw, "  HostKeyAlias %s\n", h.Host)
		fmt.Fprintf(tw, "  User %s\n", h.User)
		fmt.Fprintf(tw, "  ProxyCommand %s\n", h.ProxyCommand)
		fmt.Fprintf(tw, "  UserKnownHostsFile \"%s\"\n", strings.ReplaceAll(h.KnownHostsFile, "%", "%%"))
		fmt.Fprintf(tw, "  StrictHostKeyChecking yes\n")
		fmt.Fprintf(tw, "  CheckHostIP no\n")
	}
	return tw.Flush()
}
