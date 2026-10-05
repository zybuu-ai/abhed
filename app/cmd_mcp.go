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
			"       abhed mcp remove NAME\n"+
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
	case "add":
		s, err := parseMCPAdd(args[1:], cio.err)
		if errors.Is(err, errUsage) {
			return usage()
		}
		if err != nil {
			return fail(err)
		}
		return fail0(mcpAdd(s, cio), fail)
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

// parseMCPAdd reads add's flags and words into a server entry, enabled.
func parseMCPAdd(args []string, errw io.Writer) (config.MCPServerConfig, error) {
	var s config.MCPServerConfig
	var envs, headers multiFlag
	var allowTools string
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Var(&envs, "env", "KEY=VALUE for a command server's environment, or KEY to pass your own value (repeatable)")
	fs.Var(&headers, "header-env", "HEADER=VAR: send header HEADER with the value of environment variable VAR (repeatable)")
	fs.StringVar(&allowTools, "allow-tools", "", "comma-separated tools to offer; empty offers all")
	if err := fs.Parse(args); err != nil {
		return s, errUsage
	}
	words := fs.Args()
	if len(words) < 2 {
		return s, errUsage
	}
	s.Name, s.Enabled = words[0], true
	if !mcpServerName.MatchString(s.Name) {
		return s, fmt.Errorf("server name %s: letters, digits, _ and - only, up to 64", config.Printable(s.Name))
	}
	if u, err := url.Parse(words[1]); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		if len(words) > 2 || len(envs) > 0 {
			return s, errors.New("a URL server takes no arguments and no -env")
		}
		s.URL = words[1]
	} else {
		s.Command, s.Args = words[1], words[2:]
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
	s.AllowTools = splitRules(allowTools)
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
	if cfg.ManagedSets("mcp") {
		return &config.ManagedError{Key: "mcp.servers", Value: "", File: managed.ConfigFile,
			Reason: "the managed configuration sets the MCP servers, so they are changed only there"}
	}
	return nil
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
		words := []string{config.Printable(s.Command)}
		for _, a := range s.Args {
			words = append(words, config.Printable(a))
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

// mcpAdd confirms and writes a server into the person's own file.
func mcpAdd(s config.MCPServerConfig, cio mcpIO) error {
	if err := mcpManaged(); err != nil {
		return err
	}
	unlock, err := lockUserConfig()
	if err != nil {
		return err
	}
	defer unlock()
	doc, file, err := readUserDoc()
	if err != nil {
		return err
	}
	servers := userServers(doc)
	if slices.ContainsFunc(servers, func(v any) bool { return serverName(v) == s.Name }) {
		return fmt.Errorf("%s already has an MCP server named %s; abhed mcp remove %s first", config.Printable(file), s.Name, s.Name)
	}
	// A terminal can be faked from inside a command; the sandbox's deny on
	// ~/.abhed is what keeps the agent out, and this refuses earlier.
	if cio.inSandbox {
		return errors.New("adding an MCP server is refused inside an agent's command (ABHED_SANDBOX is set); run it in your own terminal")
	}
	// Adding a server lets the agent start a process or reach an endpoint, so
	// only a person at a terminal can agree to it.
	if !cio.terminal {
		return errors.New("adding an MCP server needs a confirmation at a terminal; run it in one, or edit ~/.abhed/config.json")
	}
	fmt.Fprintf(cio.out, "Add this MCP server to %s, enabled for every session?\n%s\n"+
		"The agent may then call its tools; each call is put to the policy and recorded. [y/N] ",
		config.Printable(file), describeServer(s))
	line, _ := bufio.NewReader(cio.in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		fmt.Fprintln(cio.out, "not added")
		return nil
	}
	entry, err := toDoc(s)
	if err != nil {
		return err
	}
	if _, err := writeUserSetting("mcp.servers", append(servers, entry)); err != nil {
		return err
	}
	if err := logConfigChange("mcp add", "mcp.servers", s, file); err != nil {
		return fmt.Errorf("added, but the change log was not written: %w", err)
	}
	fmt.Fprintf(cio.out, "added %s to %s; it starts with the next session\n", s.Name, config.Printable(file))
	return nil
}

// mcpRemove takes a server out of the person's own file. It narrows, so it
// does not ask.
func mcpRemove(name string, out io.Writer) error {
	if err := mcpManaged(); err != nil {
		return err
	}
	unlock, err := lockUserConfig()
	if err != nil {
		return err
	}
	defer unlock()
	doc, file, err := readUserDoc()
	if err != nil {
		return err
	}
	servers := userServers(doc)
	i := slices.IndexFunc(servers, func(v any) bool { return serverName(v) == name })
	if i < 0 {
		return fmt.Errorf("%s has no MCP server named %s", config.Printable(file), config.Printable(name))
	}
	var removed config.MCPServerConfig
	if b, err := json.Marshal(servers[i]); err == nil {
		_ = json.Unmarshal(b, &removed)
	}
	if _, err := writeUserSetting("mcp.servers", slices.Delete(servers, i, i+1)); err != nil {
		return err
	}
	if err := logConfigChange("mcp remove", "mcp.servers", removed, file); err != nil {
		return fmt.Errorf("removed, but the change log was not written: %w", err)
	}
	fmt.Fprintf(out, "removed %s from %s\n", name, config.Printable(file))
	return nil
}

// mcpList shows the servers in effect in this workspace and where each came from.
func mcpList(workspace string, trust config.TrustChoice, out io.Writer) error {
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: true})
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
		case cfg.ManagedSets("mcp"):
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
		}
		fmt.Fprintf(out, "%-20s %-9s %-8s %s\n", config.Printable(s.Name), source, state, where)
	}
	return nil
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
}

// logConfigChange appends a change to the person's config change log.
func logConfigChange(by, key string, s config.MCPServerConfig, file string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	kind := "stdio"
	if s.URL != "" {
		kind = "http"
	}
	line, err := json.Marshal(configChange{Time: time.Now().UTC(), By: by, Key: key, Name: s.Name,
		Kind: kind, SHA256: hex.EncodeToString(sum[:]), File: file})
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(file), "config-changes.jsonl")
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
