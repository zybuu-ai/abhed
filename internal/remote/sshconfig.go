package remote

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// An ~/.ssh/config the person already has is read, in Abhed's own process,
// for the hosts they named in it: Host, HostName, User, Port, IdentityFile,
// ProxyJump, UserKnownHostsFile and Include, as OpenSSH reads them (the
// first value obtained for a keyword wins; IdentityFile adds). A Match block
// is not evaluated and applies to nothing. A host that needs ProxyCommand or
// a keyword Abhed cannot honour safely is listed with the reason it is not
// usable, never connected to some other way.

// SSHConfig is a parsed ssh_config.
type SSHConfig struct {
	Path   string
	blocks []sshBlock
	// Unsupported notes, per file and line, what was not read.
	Unsupported []string
}

type sshBlock struct {
	patterns []string // Host patterns; nil for a Match block, which matches nothing
	match    bool
	opts     []sshOpt
}

type sshOpt struct{ key, value string }

// MachineHost is one Host from ~/.ssh/config, resolved.
type MachineHost struct {
	Alias, HostName, User string
	Port                  int
	IdentityFiles         []string
	ProxyJump             string
	KnownHostsFiles       []string
	// Refused is why Abhed will not connect to it, or "".
	Refused string
}

// Addr is HostName:Port.
func (h MachineHost) Addr() string { return joinHostPort(h.HostName, h.Port) }

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

// DefaultSSHConfigPath is ~/.ssh/config, or "" with no home.
func DefaultSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

// LoadSSHConfig reads path and the files it includes. A missing file is an
// empty config.
func LoadSSHConfig(path string) (*SSHConfig, error) {
	c := &SSHConfig{Path: path}
	if path == "" {
		return c, nil
	}
	if err := c.read(path, 0, -1); err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	return c, nil
}

// read adds file's blocks. Lines before a file's first Host or Match go to
// the block the file was included from, as OpenSSH reads an Include inside
// a Host block; at the top they apply to every host. Blocks are named by
// index, since the slice grows as files are read.
func (c *SSHConfig) read(file string, depth, cur int) error {
	if depth > 8 {
		return fmt.Errorf("%s: Include nests too deeply", file)
	}
	f, err := os.Open(file) // #nosec G304 -- the person's own ssh_config, read in Abhed's process
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if cur < 0 {
		c.blocks = append(c.blocks, sshBlock{patterns: []string{"*"}})
		cur = len(c.blocks) - 1
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		key, value, ok := splitSSHLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "host":
			c.blocks = append(c.blocks, sshBlock{patterns: strings.Fields(value)})
			cur = len(c.blocks) - 1
		case "match":
			c.blocks = append(c.blocks, sshBlock{match: true})
			cur = len(c.blocks) - 1
			c.Unsupported = append(c.Unsupported, fmt.Sprintf("%s:%d: Match blocks are not evaluated", file, n))
		case "include":
			for _, pat := range strings.Fields(value) {
				pat = expandTilde(pat)
				if !filepath.IsAbs(pat) {
					pat = filepath.Join(filepath.Dir(c.Path), pat)
				}
				matches, _ := filepath.Glob(pat)
				slices.Sort(matches)
				for _, m := range matches {
					if err := c.read(m, depth+1, cur); err != nil && !os.IsNotExist(err) {
						return err
					}
					// A Host in the included file ends the including block.
					cur = len(c.blocks) - 1
				}
			}
		default:
			c.blocks[cur].opts = append(c.blocks[cur].opts, sshOpt{key, value})
		}
	}
	return sc.Err()
}

// splitSSHLine reads "Key value", "Key=value" or "Key = value", lowering the key.
func splitSSHLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), "", true
	}
	key = strings.ToLower(line[:i])
	value = strings.TrimLeft(line[i:], " \t")
	value = strings.TrimPrefix(value, "=")
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return key, value, true
}

// matches reports whether alias matches a Host line's patterns: one
// positive pattern matches and no negated one does.
func (b sshBlock) matches(alias string) bool {
	if b.match {
		return false
	}
	hit := false
	for _, p := range b.patterns {
		neg := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		ok, err := path.Match(strings.ToLower(p), strings.ToLower(alias))
		if err != nil || !ok {
			continue
		}
		if neg {
			return false
		}
		hit = true
	}
	return hit
}

// Aliases are the concrete names the Host lines give, sorted: no wildcard
// or negated pattern, which names no one host.
func (c *SSHConfig) Aliases() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, b := range c.blocks {
		for _, p := range b.patterns {
			if p == "" || strings.ContainsAny(p, "*?!") || slices.Contains(out, p) {
				continue
			}
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// Resolve reads alias as OpenSSH would; ok is false when no Host line names it.
func (c *SSHConfig) Resolve(alias string) (MachineHost, bool) {
	if c == nil || alias == "" || !slices.Contains(c.Aliases(), alias) {
		return MachineHost{}, false
	}
	return c.resolve(alias), true
}

// resolve applies every block matching name, first value winning.
func (c *SSHConfig) resolve(name string) MachineHost {
	h := MachineHost{Alias: name}
	seen := map[string]bool{}
	var refused []string
	for _, b := range c.blocks {
		if !b.matches(name) {
			continue
		}
		for _, o := range b.opts {
			if o.key == "identityfile" {
				if strings.EqualFold(o.value, "none") {
					continue
				}
				h.IdentityFiles = append(h.IdentityFiles, o.value)
				continue
			}
			if seen[o.key] {
				continue
			}
			seen[o.key] = true
			switch o.key {
			case "hostname":
				h.HostName = o.value
			case "user":
				h.User = o.value
			case "port":
				if p, err := strconv.Atoi(o.value); err == nil && p > 0 && p < 65536 {
					h.Port = p
				}
			case "proxyjump":
				if !strings.EqualFold(o.value, "none") {
					h.ProxyJump = o.value
				}
			case "userknownhostsfile":
				if !strings.EqualFold(o.value, "none") {
					h.KnownHostsFiles = strings.Fields(o.value)
				}
			case "proxycommand":
				if !strings.EqualFold(o.value, "none") {
					refused = append(refused, "it reaches the host through ProxyCommand, a program Abhed does not run")
				}
			case "stricthostkeychecking":
				// A host's key is checked against known_hosts whatever this says;
				// Abhed never accepts or records one silently.
			}
		}
	}
	if h.HostName == "" {
		h.HostName = name
	}
	h.HostName = strings.ReplaceAll(h.HostName, "%h", name)
	if h.Port == 0 {
		h.Port = 22
	}
	if h.User == "" {
		h.User = localUser()
	}
	for i, f := range h.IdentityFiles {
		h.IdentityFiles[i] = expandTokens(f, h)
	}
	for i, f := range h.KnownHostsFiles {
		h.KnownHostsFiles[i] = expandTokens(f, h)
	}
	h.Refused = strings.Join(refused, "; ")
	return h
}

func localUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// expandTokens expands ~ and the tokens IdentityFile takes: %d home, %u
// local user, %h host name, %r remote user, %p port, %% a percent.
func expandTokens(s string, h MachineHost) string {
	home, _ := os.UserHomeDir()
	r := strings.NewReplacer("%%", "%", "%d", home, "%u", localUser(), "%h", h.HostName,
		"%r", h.User, "%p", strconv.Itoa(h.Port), "%n", h.Alias)
	return expandTilde(r.Replace(s))
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// UseSSHConfig offers the hosts c names to the ssh tools, beside the
// operator's: a declared host keeps its name.
// allowed, when set, are the only addresses they may be (ssh.connect_hosts).
func (r *Registry) UseSSHConfig(c *SSHConfig, allowed []string) {
	r.mu.Lock()
	r.machine, r.machineAllowed = c, allowed
	r.mu.Unlock()
}

// machineConfig is the ssh_config the registry offers, or nil.
func (r *Registry) machineConfig() *SSHConfig {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.machine
}

// MachineHost resolves alias from the person's ~/.ssh/config, unless the
// operator declared that name.
func (r *Registry) MachineHost(alias string) (MachineHost, bool) {
	c := r.machineConfig()
	if c == nil {
		return MachineHost{}, false
	}
	if _, declared := r.get(alias); declared {
		return MachineHost{}, false
	}
	mh, ok := c.Resolve(alias)
	r.mu.RLock()
	allowed := r.machineAllowed
	r.mu.RUnlock()
	if ok && mh.Refused == "" && !addrAllowed(allowed, mh.Addr()) {
		mh.Refused = "ssh.connect_hosts does not list " + mh.Addr()
	}
	return mh, ok
}

// machineAliases are the ~/.ssh/config hosts offered, sorted.
func (r *Registry) machineAliases() []string {
	c := r.machineConfig()
	if c == nil {
		return nil
	}
	var out []string
	for _, a := range c.Aliases() {
		if _, declared := r.get(a); !declared {
			out = append(out, a)
		}
	}
	return out
}

// maxJumps bounds a ProxyJump chain.
const maxJumps = 4

// newMachineHost builds the connection for mh, its jump hosts first.
func newMachineHost(c *SSHConfig, mh MachineHost) (*Host, error) {
	if mh.Refused != "" {
		return nil, fmt.Errorf("host %s in your ~/.ssh/config is not usable from Abhed: %s", mh.Alias, mh.Refused)
	}
	h, err := NewHost(machineHostConfig(mh))
	if err != nil {
		return nil, err
	}
	if mh.ProxyJump == "" {
		return h, nil
	}
	hops := strings.Split(mh.ProxyJump, ",")
	if len(hops) > maxJumps {
		return nil, fmt.Errorf("host %s: ProxyJump names %d hops; Abhed follows at most %d", mh.Alias, len(hops), maxJumps)
	}
	var prev *Host
	for _, hop := range hops {
		jh, err := jumpHost(c, strings.TrimSpace(hop))
		if err != nil {
			return nil, fmt.Errorf("host %s: ProxyJump %s: %w", mh.Alias, hop, err)
		}
		jh.cfg.jump = prev
		prev = jh
	}
	h.cfg.jump = prev
	return h, nil
}

// jumpHost reads one ProxyJump hop, [user@]host[:port] or an alias, with
// what the config says of that host.
func jumpHost(c *SSHConfig, spec string) (*Host, error) {
	if spec == "" || strings.HasPrefix(spec, "ssh://") || strings.ContainsAny(spec, " \t") {
		return nil, fmt.Errorf("not a host Abhed can read")
	}
	userPart, hostPart := "", spec
	if i := strings.LastIndex(spec, "@"); i >= 0 {
		userPart, hostPart = spec[:i], spec[i+1:]
	}
	port := 0
	if h, p, err := net.SplitHostPort(hostPart); err == nil {
		hostPart = h
		if port, err = strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("bad port")
		}
	}
	mh := c.resolve(hostPart)
	if mh.Refused != "" {
		return nil, fmt.Errorf("%s", mh.Refused)
	}
	if userPart != "" {
		mh.User = userPart
	}
	if port != 0 {
		mh.Port = port
	}
	mh.ProxyJump = "" // a hop's own ProxyJump is not followed
	return NewHost(machineHostConfig(mh))
}

func machineHostConfig(mh MachineHost) HostConfig {
	known := mh.KnownHostsFiles
	if len(known) == 0 {
		known = []string{"~/.ssh/known_hosts", "~/.ssh/known_hosts2"}
	}
	return HostConfig{Name: mh.Alias, Addr: mh.Addr(), User: mh.User,
		machine: true, identityFiles: mh.IdentityFiles, knownHostFiles: known}
}
