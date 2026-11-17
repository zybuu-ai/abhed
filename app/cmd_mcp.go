package app

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcpauth"
	"golang.org/x/term"
)

// mcpIO is where `abhed mcp` reads its answer and writes; a variable so tests
// can answer the confirmation.
type mcpIO struct {
	in       io.Reader
	out, err io.Writer
	// terminal is whether in is a person at a terminal: only one can confirm.
	terminal bool
	// inSandbox is whether this runs in an agent's command, which sets
	// ABHED_SANDBOX; such a command never adds a server.
	inSandbox bool
	// byEditor is a yes read from -confirm-fd, which the change log says apart.
	byEditor bool
}

func stdMCPIO() mcpIO {
	return mcpIO{in: os.Stdin, out: os.Stdout, err: os.Stderr, terminal: term.IsTerminal(int(os.Stdin.Fd())), inSandbox: inAgentCommand() != ""}
}

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// mcpCmd is `abhed mcp add|list|remove`: MCP servers in the person's own
// ~/.abhed/config.json. Adding one confirms at a terminal, since it lets the
// agent do more; every change is appended to ~/.abhed/config-changes.jsonl.
func mcpCmd(workspace string, args []string, trust config.TrustChoice, cio mcpIO) int {
	usage := func() int {
		fmt.Fprintln(cio.err, "usage: abhed mcp list\n"+
			"       abhed mcp add [-env KEY[=VALUE]]... [-header-env HEADER=VAR]... [-allow-tools a,b] NAME COMMAND [ARGS...]\n"+
			"       abhed mcp add [-header-env HEADER=VAR]... [-allow-tools a,b] NAME https://URL\n"+
			"         either add also takes -sha256 HEX (only that entry) and -confirm-fd N (the yes from an inherited descriptor, for an editor)\n"+
			"       abhed mcp remove NAME\n"+
			"       abhed mcp login [-confirm-fd N] NAME | logout NAME\n"+
			"         login -confirm-fd N reads the yes from an inherited descriptor and leaves the browser to the editor that asked\n"+
			"  Servers are added to ~/.abhed/config.json, enabled, after a confirmation at a terminal.")
		return 2
	}
	fail := func(err error) int { fmt.Fprintf(cio.err, "abhed: %v\n", err); return 1 }
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage()
		}
		return fail0(mcpList(workspace, trust, cio.out), fail)
	case "remove", "rm":
		if len(args) != 2 {
			return usage()
		}
		return fail0(mcpRemove(args[1], cio.out), fail)
	case "login":
		name, fd, ok := parseMCPLogin(args[1:])
		if !ok {
			return usage()
		}
		if fd != 0 {
			f := os.NewFile(uintptr(fd), "confirm") // #nosec G115 -- checked to be 3 or above
			if _, err := f.Stat(); err != nil {
				return fail(fmt.Errorf("-confirm-fd %d is not open", fd))
			}
			defer func() { _ = f.Close() }()
			cio.in, cio.terminal, cio.byEditor = f, true, true
		}
		return fail0(mcpLogin(workspace, trust, name, cio), fail)
	case "logout":
		if len(args) != 2 {
			return usage()
		}
		return fail0(mcpLogout(workspace, trust, args[1], cio), fail)
	case "add":
		s, o, err := parseMCPAddOpts(args[1:], cio.err)
		if errors.Is(err, errUsage) {
			return usage()
		}
		if err != nil {
			return fail(err)
		}
		if o.confirmFD != 0 {
			f := os.NewFile(uintptr(o.confirmFD), "confirm") // #nosec G115 -- checked to be 3 or above
			if _, err := f.Stat(); err != nil {
				return fail(fmt.Errorf("-confirm-fd %d is not open", o.confirmFD))
			}
			defer func() { _ = f.Close() }()
			cio.in, cio.terminal, cio.byEditor = f, true, true
		}
		return fail0(mcpAdd(s, o.sha256, cio), fail)
	}
	return usage()
}

func fail0(err error, fail func(error) int) int {
	if err != nil {
		return fail(err)
	}
	return 0
}

var errUsage = errors.New("usage")

// parseMCPLogin reads login's words: NAME, or -confirm-fd N NAME for an
// editor that asked the person and opens the browser itself.
func parseMCPLogin(args []string) (name string, fd int, ok bool) {
	fs := flag.NewFlagSet("mcp login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&fd, "confirm-fd", 0, "")
	if fs.Parse(args) != nil || fs.NArg() != 1 || (fd != 0 && fd < 3) {
		return "", 0, false
	}
	return fs.Arg(0), fd, true
}

// mcpAddOpts are add's flags for an editor's main process: where the yes is
// read from instead of a terminal, and the entry the person was shown.
type mcpAddOpts struct {
	confirmFD int
	sha256    string
}

// parseMCPAdd reads add's flags and words into a server entry, enabled.
func parseMCPAdd(args []string, errw io.Writer) (config.MCPServerConfig, error) {
	s, _, err := parseMCPAddOpts(args, errw)
	return s, err
}

func parseMCPAddOpts(args []string, errw io.Writer) (config.MCPServerConfig, mcpAddOpts, error) {
	var s config.MCPServerConfig
	var o mcpAddOpts
	var envs, headers, allowTools multiFlag
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.IntVar(&o.confirmFD, "confirm-fd", 0, "read the yes from this inherited descriptor (3 or above) instead of a terminal, for an editor that asked the person")
	fs.StringVar(&o.sha256, "sha256", "", "add only if the entry's SHA-256 is this, the one the person was shown")
	fs.Var(&envs, "env", "KEY=VALUE for a command server's environment, or KEY to pass your own value (repeatable)")
	fs.Var(&headers, "header-env", "HEADER=VAR: send header HEADER with the value of environment variable VAR (repeatable)")
	fs.Var(&allowTools, "allow-tools", "comma-separated tools to offer (repeatable); none offers all")
	if err := fs.Parse(args); err != nil {
		return s, o, errUsage
	}
	words := fs.Args()
	// A yes from a descriptor is always held to the entry the person was shown.
	if len(words) < 2 || (o.confirmFD != 0 && (o.confirmFD < 3 || o.sha256 == "")) {
		return s, o, errUsage
	}
	s, err := newMCPServer(words[0], words[1], words[2:], envs, headers, splitLists(allowTools))
	return s, o, err
}

// newMCPServer is one server entry, enabled, as `abhed mcp add` and Studio
// describe it: a command with its arguments, or an http(s) URL.
func newMCPServer(name, target string, args, envs, headers, allowTools []string) (config.MCPServerConfig, error) {
	var s config.MCPServerConfig
	s.Name, s.Enabled = name, true
	if !mcpServerName.MatchString(s.Name) {
		return s, fmt.Errorf("server name %s: letters, digits, _ and - only, up to 64", config.Printable(s.Name))
	}
	if target == "" {
		return s, errors.New("a server needs a command or an http(s) URL")
	}
	if u, err := url.Parse(target); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		if len(args) > 0 || len(envs) > 0 {
			return s, errors.New("a URL server takes no arguments and no -env")
		}
		s.URL = target
	} else {
		s.Command, s.Args = target, args
	}
	for _, e := range envs {
		// A bare KEY passes your own value of it.
		if k, _, _ := strings.Cut(e, "="); !envName.MatchString(k) {
			return s, fmt.Errorf("-env %s: want KEY=VALUE, or KEY to pass your own value", config.Printable(e))
		}
		s.Env = append(s.Env, e)
	}
	for _, h := range headers {
		k, v, ok := strings.Cut(h, "=")
		if !ok || k == "" || strings.ContainsAny(k, " :\r\n") || !envName.MatchString(v) {
			return s, fmt.Errorf("-header-env %s: want HEADER=VAR", config.Printable(h))
		}
		if s.HeadersEnv == nil {
			s.HeadersEnv = map[string]string{}
		}
		s.HeadersEnv[k] = v
	}
	if len(s.HeadersEnv) > 0 && s.URL == "" {
		return s, errors.New("-header-env is for a URL server")
	}
	s.AllowTools = allowTools
	return s, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// mcpManaged refuses a change when the managed configuration sets the MCP
// servers: a user entry would be replaced by its list anyway, and it is the
// organisation's to change.
func mcpManaged() error {
	cfg, err := config.LoadManaged()
	if err != nil {
		return err
	}
	if cfg.ManagedSets("mcp.servers") {
		return &config.ManagedError{Key: "mcp.servers", Value: "", File: managed.ConfigFile,
			Reason: "the managed configuration sets the MCP servers, so they are changed only there"}
	}
	return nil
}

// plainWord is a word that reads the same quoted or not.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// quotedWord is one word of a command as a person reviews it: escaped, and
// quoted when it holds a space or shell syntax, so word boundaries show.
func quotedWord(w string) string {
	p := config.Printable(w)
	if plainWord.MatchString(p) {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// describeServer is one server as a person reviews it, every value escaped.
func describeServer(s config.MCPServerConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  name:    %s\n", config.Printable(s.Name))
	if s.URL != "" {
		fmt.Fprintf(&b, "  url:     %s\n", config.PrintableURL(s.URL))
		for k, v := range s.HeadersEnv {
			fmt.Fprintf(&b, "  header:  %s from $%s\n", config.Printable(k), config.Printable(v))
		}
	} else {
		words := []string{quotedWord(s.Command)}
		for _, a := range s.Args {
			words = append(words, quotedWord(a))
		}
		fmt.Fprintf(&b, "  command: %s\n", strings.Join(words, " "))
		for _, e := range s.Env {
			if k, _, ok := strings.Cut(e, "="); ok {
				fmt.Fprintf(&b, "  env:     %s=…\n", config.Printable(k))
			} else {
				fmt.Fprintf(&b, "  env:     %s (your own value)\n", config.Printable(k))
			}
		}
	}
	tools := "all it offers"
	if len(s.AllowTools) > 0 {
		tools = config.Printable(strings.Join(s.AllowTools, ", "))
	}
	fmt.Fprintf(&b, "  tools:   %s", tools)
	return b.String()
}

// mcpAddable is why s cannot be added, if it cannot: the managed
// configuration sets the servers, its MCP lists refuse s (it would never
// start), or the person's file already has a server of that name.
func mcpAddable(s config.MCPServerConfig) (string, error) {
	if err := mcpManaged(); err != nil {
		return "", err
	}
	if cfg, err := config.LoadManaged(); err != nil {
		return "", err
	} else if err := cfg.MCPAdmitted(config.MCPServerOf(s)); err != nil {
		return "", err
	}
	doc, file, err := readUserDoc()
	if err != nil {
		return file, err
	}
	if slices.ContainsFunc(userServers(doc), func(v any) bool { return serverName(v) == s.Name }) {
		return file, fmt.Errorf("%s already has an MCP server named %s; remove it first", config.Printable(file), s.Name)
	}
	return file, nil
}

// writeMCPServer adds s, enabled, to the person's own file once they have
// agreed, checking again under the file's lock, and logs the change as by.
func writeMCPServer(s config.MCPServerConfig, by string) (string, error) {
	unlock, err := lockUserConfig()
	if err != nil {
		return "", err
	}
	defer unlock()
	file, err := mcpAddable(s)
	if err != nil {
		return file, err
	}
	doc, _, err := readUserDoc()
	if err != nil {
		return file, err
	}
	entry, err := toDoc(s)
	if err != nil {
		return file, err
	}
	if _, err := writeUserSetting("mcp.servers", append(userServers(doc), entry)); err != nil {
		return file, err
	}
	if err := logConfigChange(by, "mcp.servers", s, file); err != nil {
		return file, fmt.Errorf("added, but the change log was not written: %w", err)
	}
	return file, nil
}

// mcpAdd confirms and writes a server into the person's own file. A refusal
// by policy or inside an agent's command is logged as an attempt.
func mcpAdd(s config.MCPServerConfig, want string, cio mcpIO) error {
	file, err := mcpAddable(s)
	var me *config.ManagedError
	if errors.As(err, &me) {
		_ = logConfigRefusal("mcp add", s, file, err.Error())
	}
	if err != nil {
		return err
	}
	if _, sum := mcpEntrySum(s); want != "" && !strings.EqualFold(want, sum) {
		return fmt.Errorf("the server differs from the one shown (sha256 %s, not %s); nothing was added", sum, config.Printable(want))
	}
	// A terminal can be faked from inside a command; the sandbox's deny on
	// ~/.abhed is what keeps the agent out, and this refuses earlier.
	if cio.inSandbox {
		_ = logConfigRefusal("mcp add", s, file, "inside an agent's command")
		return errors.New("adding an MCP server is refused inside an agent's command (ABHED_SANDBOX is set); run it in your own terminal")
	}
	// Adding a server lets the agent start a process or reach an endpoint, so
	// only a person can agree to it: at a terminal, or in an editor that asked.
	if !cio.terminal {
		return errors.New("adding an MCP server needs a confirmation at a terminal; run it in one, or edit ~/.abhed/config.json")
	}
	fmt.Fprintf(cio.out, "Add this MCP server to %s, enabled for every session?\n%s\n"+
		"The agent may then call its tools; each call is put to the policy and recorded. [y/N] ",
		config.Printable(file), describeServer(s))
	line, _ := bufio.NewReader(cio.in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		fmt.Fprintln(cio.out, "not added")
		if cio.byEditor {
			// The editor asked already: no yes on the descriptor is a failure, not a no.
			return errors.New("no yes was read from -confirm-fd; nothing was added")
		}
		return nil
	}
	by := "mcp add"
	if cio.byEditor {
		by = "mcp add -confirm-fd"
	}
	if _, err := writeMCPServer(s, by); err != nil {
		return err
	}
	fmt.Fprintf(cio.out, "added %s to %s; it starts with the next session\n", s.Name, config.Printable(file))
	return nil
}

// mcpRemove takes a server out of the person's own file. It narrows, so it
// does not ask.
func mcpRemove(name string, out io.Writer) error {
	file, err := removeMCPServer(name, "mcp remove")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "removed %s from %s\n", name, config.Printable(file))
	return nil
}

// removeMCPServer takes the server named name out of the person's own file
// and logs the change as by.
func removeMCPServer(name, by string) (string, error) {
	file, _, err := removeMCPServerEntry(name, by)
	return file, err
}

// removeMCPServerEntry is removeMCPServer, also returning the entry removed.
func removeMCPServerEntry(name, by string) (string, config.MCPServerConfig, error) {
	var removed config.MCPServerConfig
	if err := mcpManaged(); err != nil {
		return "", removed, err
	}
	unlock, err := lockUserConfig()
	if err != nil {
		return "", removed, err
	}
	defer unlock()
	doc, file, err := readUserDoc()
	if err != nil {
		return file, removed, err
	}
	servers := userServers(doc)
	i := slices.IndexFunc(servers, func(v any) bool { return serverName(v) == name })
	if i < 0 {
		return file, removed, fmt.Errorf("%s has no MCP server named %s", config.Printable(file), config.Printable(name))
	}
	if b, err := json.Marshal(servers[i]); err == nil {
		_ = json.Unmarshal(b, &removed)
	}
	if _, err := writeUserSetting("mcp.servers", slices.Delete(servers, i, i+1)); err != nil {
		return file, removed, err
	}
	if err := logConfigChange(by, "mcp.servers", removed, file); err != nil {
		return file, removed, fmt.Errorf("removed, but the change log was not written: %w", err)
	}
	return file, removed, nil
}

// mcpList shows the servers in effect in this workspace and where each came from.
func mcpList(workspace string, trust config.TrustChoice, out io.Writer) error {
	cfg, err := loadNetworked(workspace, config.LoadOptions{Trust: trust, Quiet: true})
	if err != nil {
		return err
	}
	doc, _, err := readUserDoc()
	if err != nil {
		return err
	}
	user := map[string]bool{}
	for _, v := range userServers(doc) {
		user[serverName(v)] = true
	}
	if len(cfg.MCP.Servers) == 0 {
		fmt.Fprintln(out, "no MCP servers are configured; abhed mcp add NAME COMMAND adds one")
		return nil
	}
	for _, s := range cfg.MCP.Servers {
		source := "workspace"
		switch {
		case cfg.ManagedSets("mcp.servers"):
			source = "managed"
		case user[s.Name]:
			source = "user"
		}
		state := "enabled"
		if !s.Enabled {
			state = "disabled"
		}
		where := config.Printable(s.Command)
		if s.URL != "" {
			where = config.PrintableURL(s.URL)
			where += mcpLoginNote(s.Name, s.URL, time.Now())
		}
		fmt.Fprintf(out, "%-20s %-9s %-8s %s\n", config.Printable(s.Name), source, state, where)
	}
	return nil
}

// mcpLoginNote is how abhed mcp list shows a URL server's login: "signed
// in" only while the login can still give a token.
func mcpLoginNote(name, serverURL string, now time.Time) string {
	st, err := mcpauth.State(openVault(), name, serverURL)
	again := "; abhed mcp login " + config.Printable(name) + ")"
	switch {
	case err != nil:
		return " (the stored login does not read" + again
	case !st.Held:
		return ""
	case st.OtherURL:
		return " (login for another URL" + again
	case !st.RefreshRefused.IsZero():
		return " (not signed in: the login was refused at refresh " + st.RefreshRefused.Local().Format("2006-01-02 15:04") + again
	case !st.Usable(now):
		return " (not signed in: the login ended" + again
	case !st.Expiry.IsZero() && !st.Refreshable:
		return " (signed in until " + st.Expiry.Local().Format("2006-01-02 15:04") + ")"
	}
	return " (signed in)"
}

// readUserDoc is ~/.abhed/config.json as a generic document, so a write
// keeps every key it does not touch.
func readUserDoc() (map[string]any, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	file := filepath.Join(home, ".abhed", "config.json")
	doc := map[string]any{}
	data, err := os.ReadFile(file) // #nosec G304 -- the person's own ~/.abhed/config.json
	if errors.Is(err, os.ErrNotExist) {
		return doc, file, nil
	}
	if err != nil {
		return nil, file, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, file, fmt.Errorf("%s is not valid JSON, so it was not changed: %w", file, err)
	}
	return doc, file, nil
}

func userServers(doc map[string]any) []any {
	m, _ := doc["mcp"].(map[string]any)
	list, _ := m["servers"].([]any)
	return list
}

func serverName(v any) string {
	m, _ := v.(map[string]any)
	n, _ := m["name"].(string)
	return n
}

func toDoc(s config.MCPServerConfig) (any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(b, &v)
}

// configChange is one line of ~/.abhed/config-changes.jsonl. It names the
// entry and its hash, never its values, which may hold credentials.
type configChange struct {
	Time   time.Time `json:"time"`
	By     string    `json:"by"`
	Key    string    `json:"key"`
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	SHA256 string    `json:"sha256"`
	File   string    `json:"file"`
	// Refused says why an attempt was not written.
	Refused string `json:"refused,omitempty"`
}

// mcpEntrySum is a server's transport and the SHA-256 of its entry, as the
// change log and the record name it.
func mcpEntrySum(s config.MCPServerConfig) (kind, sum string) {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	kind = "stdio"
	if s.URL != "" {
		kind = "http"
	}
	return kind, hex.EncodeToString(h[:])
}

// logConfigRefusal logs an add that was refused, so an attempt to widen is
// on file with the changes.
func logConfigRefusal(by string, s config.MCPServerConfig, file, why string) error {
	if file == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		file = filepath.Join(home, ".abhed", "config.json")
	}
	return appendConfigChange(configChange{By: by, Key: "mcp.servers", Name: s.Name, File: file, Refused: why}, s)
}

// logConfigChange appends a change to the person's config change log.
func logConfigChange(by, key string, s config.MCPServerConfig, file string) error {
	return appendConfigChange(configChange{By: by, Key: key, Name: s.Name, File: file}, s)
}

// maxConfigChangeLog bounds the change log against attempts: past it a refused
// attempt is no longer logged, while a change still is.
const maxConfigChangeLog = 4 << 20

func appendConfigChange(c configChange, s config.MCPServerConfig) error {
	c.Time = time.Now().UTC()
	c.Kind, c.SHA256 = mcpEntrySum(s)
	line, err := json.Marshal(c)
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(c.File), "config-changes.jsonl")
	if st, err := os.Lstat(path); err == nil && c.Refused != "" && st.Size() >= maxConfigChangeLog {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|oNoFollow, 0o600) // #nosec G304 -- beside the person's own config
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
