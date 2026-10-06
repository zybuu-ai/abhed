package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// adminLogName is the append-only log kept beside the managed file.
const adminLogName = "admin.jsonl"

// adminEntry is one line of the admin log: who tried to change the managed
// configuration, from what to what, and what came of it. It never holds a key.
type adminEntry struct {
	Time    string `json:"time"`
	Command string `json:"command"`
	// Action is the verb alone, web-search on or off, as the system log gives it.
	Action   string         `json:"action,omitempty"`
	File     string         `json:"file"`
	UID      string         `json:"uid"`
	User     string         `json:"user,omitempty"`
	SudoUser string         `json:"sudo_user,omitempty"`
	SudoUID  string         `json:"sudo_uid,omitempty"`
	Agent    string         `json:"agent_command,omitempty"`
	From     map[string]any `json:"from,omitempty"`
	To       map[string]any `json:"to,omitempty"`
	Result   string         `json:"result"` // changed, unchanged or refused
	Reason   string         `json:"reason,omitempty"`
}

// adminCmd is `abhed admin`, which changes only the managed configuration.
func adminCmd(args []string, out, errw io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) || args[0] != "web-search" {
		fmt.Fprintln(errw, "usage: abhed admin web-search on|off [--provider name] [--base-url url] [--api-key-env VAR] [--max-results n]\n"+
			"  turns web search on or off in the managed configuration, which only an administrator can write (sudo on your own machine)")
		if len(args) > 0 && isHelpArg(args[0]) {
			return 0
		}
		return 2
	}
	return adminWebSearch(args[1:], out, errw)
}

// adminWebSearch turns web search on or off in the managed file, and logs
// the attempt, allowed or refused, beside it.
func adminWebSearch(args []string, out, errw io.Writer) int {
	if len(args) == 0 || (args[0] != "on" && args[0] != "off") {
		fmt.Fprintln(errw, "abhed admin web-search: say on or off")
		return 2
	}
	state := args[0]
	f, err := parseSearchFlags(args[1:], errw)
	if err != nil {
		return 2
	}
	provider, baseURL, keyEnv, maxResults := &f.provider, &f.baseURL, &f.keyEnv, &f.maxResults

	file := managed.ConfigFile
	e := newAdminEntry(args)
	refuse := func(why string) int {
		e.Result, e.Reason = "refused", why
		fmt.Fprintf(errw, "abhed admin web-search: refused: %s\n", why)
		logAdmin(e, errw)
		return 1
	}
	if why := inAgentCommand(); why != "" {
		e.Agent = why
		return refuse("it runs inside an agent's command (" + why + "); an agent must not administer the harness that confines it")
	}
	if state == "off" && (*provider != "" || *baseURL != "" || *keyEnv != "" || *maxResults != 0) {
		return refuse("--provider, --base-url, --api-key-env and --max-results go with on")
	}
	if err := checkSearchFlags(*provider, *baseURL, *keyEnv, *maxResults); err != nil {
		return refuse(err.Error())
	}

	doc, mode, err := readManagedDoc(file)
	if err != nil {
		return refuse(err.Error())
	}
	section, _ := doc["web_search"].(map[string]any)
	e.From = shownSection(section)
	next := maps.Clone(section)
	if next == nil {
		next = map[string]any{}
	}
	next["enabled"] = state == "on"
	for k, v := range map[string]string{"provider": *provider, "base_url": *baseURL, "api_key_env": *keyEnv} {
		if v != "" {
			next[k] = v
		}
	}
	if *maxResults > 0 {
		next["max_results"] = *maxResults
	}
	if p, _ := next["provider"].(string); strings.EqualFold(p, "searxng") && next["enabled"] == true {
		if b, _ := next["base_url"].(string); b == "" {
			return refuse("searxng needs --base-url, the address of your instance")
		}
	}
	e.To = shownSection(next)
	if sameJSONValue(section, next) {
		e.Result = "unchanged"
		logAdmin(e, errw)
		fmt.Fprintf(out, "web search is already %s in %s\n", state, config.Printable(file))
		return 0
	}
	doc["web_search"] = next
	if err := writeManagedDoc(file, doc, mode); err != nil {
		return refuse(err.Error())
	}
	e.Result = "changed"
	logAdmin(e, errw)
	fmt.Fprintf(out, "web search is %s in %s (%s)\nNew sessions take it; restart `abhed serve` for the server.\n",
		state, config.Printable(file), describeSection(e.To))
	return 0
}

// searchFlags are the flags of `abhed admin web-search on|off`.
type searchFlags struct {
	provider, baseURL, keyEnv string
	maxResults                int
}

// parseSearchFlags parses the flags after on or off; errw takes what is
// wrong with them, io.Discard when only the values are wanted.
func parseSearchFlags(args []string, errw io.Writer) (searchFlags, error) {
	var f searchFlags
	fs := flag.NewFlagSet("admin web-search", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.StringVar(&f.provider, "provider", "", "duckduckgo, brave, tavily, serper or searxng")
	fs.StringVar(&f.baseURL, "base-url", "", "the provider's endpoint: a self-hosted searxng, or an egress broker")
	fs.StringVar(&f.keyEnv, "api-key-env", "", "the environment variable holding the provider's key; the key itself is never taken here")
	fs.IntVar(&f.maxResults, "max-results", 0, "results per search")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errw, "abhed admin web-search: unexpected %q\n", fs.Arg(0))
		return f, fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	return f, nil
}

// adminCommand is the command as the log keeps it: the verb and each flag
// whose value passed its check, a URL without credentials; a value that did
// not is not written, since it may be a key typed in the wrong place.
func adminCommand(args []string) string {
	if len(args) == 0 {
		return "web-search"
	}
	parts := []string{"web-search"}
	if args[0] == "on" || args[0] == "off" {
		parts = append(parts, args[0])
	} else {
		parts = append(parts, "(refused)")
	}
	f, err := parseSearchFlags(args[1:], io.Discard)
	if err != nil {
		return strings.Join(append(parts, "(arguments not understood)"), " ")
	}
	shown := func(flag, v string, ok bool) {
		switch {
		case v == "":
		case ok:
			parts = append(parts, flag, v)
		default:
			parts = append(parts, flag, "(refused)")
		}
	}
	shown("--provider", f.provider, checkSearchFlags(f.provider, "", "", 0) == nil)
	if checkSearchFlags("", f.baseURL, "", 0) == nil {
		shown("--base-url", config.PrintableURL(f.baseURL), true)
	} else {
		shown("--base-url", f.baseURL, false)
	}
	shown("--api-key-env", f.keyEnv, checkSearchFlags("", "", f.keyEnv, 0) == nil)
	if f.maxResults != 0 {
		shown("--max-results", strconv.Itoa(f.maxResults), f.maxResults > 0)
	}
	return strings.Join(parts, " ")
}

// newAdminEntry starts the log entry for `abhed admin web-search args`.
func newAdminEntry(args []string) adminEntry {
	e := adminEntry{Time: time.Now().UTC().Format(time.RFC3339), Command: adminCommand(args),
		File: managed.ConfigFile, UID: strconv.Itoa(os.Getuid())}
	if len(args) > 0 && (args[0] == "on" || args[0] == "off") {
		e.Action = "web-search " + args[0]
	}
	if u, err := user.Current(); err == nil {
		e.User = u.Username
	}
	// sudo names the person who asked; only root's word for it is taken.
	if os.Getuid() == 0 {
		e.SudoUser, e.SudoUID = os.Getenv("SUDO_USER"), os.Getenv("SUDO_UID")
	}
	return e
}

// logAdminInAgent logs `abhed admin` refused before it ran, inside an
// agent's command (why), as the command itself would have.
func logAdminInAgent(args []string, why string, errw io.Writer) {
	if len(args) == 0 || args[0] != "web-search" {
		return
	}
	e := newAdminEntry(args[1:])
	e.Agent, e.Result = why, "refused"
	e.Reason = "it runs inside an agent's command (" + why + "); an agent must not administer the harness that confines it"
	logAdmin(e, errw)
}

// checkSearchFlags refuses a provider or endpoint web search cannot use.
func checkSearchFlags(provider, baseURL, keyEnv string, max int) error {
	if provider != "" && !slices.Contains([]string{"duckduckgo", "ddg", "brave", "tavily", "serper", "searxng"}, strings.ToLower(provider)) {
		return fmt.Errorf("unknown provider (want duckduckgo, brave, tavily, serper or searxng)")
	}
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("--base-url must be an http or https URL")
		}
		if u.User != nil {
			return fmt.Errorf("--base-url must not carry a password; name the key's variable with --api-key-env")
		}
	}
	if keyEnv != "" && (!keyEnvName.MatchString(keyEnv) || looksLikeKey(keyEnv)) {
		return fmt.Errorf("--api-key-env takes the name of the variable that holds the key, such as SEARCH_API_KEY " +
			"(capitals, digits and _, not starting with a digit), never the key itself")
	}
	if max < 0 {
		return fmt.Errorf("--max-results must be positive")
	}
	return nil
}

// keyEnvName is the variable names --api-key-env takes.
var keyEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// readManagedDoc reads the managed file as a document to edit, with the mode
// to keep. No file is an empty document; a link is refused, since writing
// would replace it with a file.
func readManagedDoc(file string) (map[string]any, os.FileMode, error) {
	doc := map[string]any{}
	fi, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return doc, 0o644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file; edit it by hand", config.Printable(file))
	}
	data, err := os.ReadFile(file) // #nosec G304 -- the managed configuration, which this command edits
	if err != nil {
		return nil, 0, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, fmt.Errorf("%s is not valid JSON, so it was not changed: %w", config.Printable(file), err)
	}
	return doc, fi.Mode().Perm(), nil
}

// Hooks for the tests: the steps of writing the managed file.
var (
	chownManaged  = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
	syncManaged   = func(f *os.File) error { return f.Sync() }
	syncManagedIn = syncDir
)

// writeManagedDoc replaces the managed file with doc, through a new file in
// the same directory, so a reader sees the old file or the new one whole.
// The new file keeps the old one's owner and group (root:abhed 0640 must stay
// readable by the server) and is on disk before it replaces the old one.
func writeManagedDoc(file string, doc map[string]any, mode os.FileMode) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- every user reads the managed configuration
		return noRights(dir, err)
	}
	uid, gid, owned := -1, -1, false
	if fi, err := os.Lstat(file); err == nil {
		uid, gid, owned = fileOwner(fi)
	}
	tmp, err := os.CreateTemp(dir, ".config.json.*")
	if err != nil {
		return noRights(dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	fail := func(err error) error {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fail(err)
	}
	if owned && runningAsRoot() {
		if err := chownManaged(tmp, uid, gid); err != nil {
			return fail(fmt.Errorf("keep the owner of %s: %w", config.Printable(file), err))
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := syncManaged(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return noRights(dir, err)
	}
	return syncManagedIn(dir)
}

func noRights(dir string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("cannot write %s: changing the managed configuration needs root or the administrator's rights; "+
			"run it with sudo, or ask your administrator", config.Printable(dir))
	}
	return err
}

// shownSection is a web_search section as the log and the output show it:
// a key is never shown, only whether one is set.
func shownSection(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range s {
		switch k {
		case "api_key":
			out["api_key_set"] = v != nil && v != ""
		case "base_url":
			if str, ok := v.(string); ok {
				out[k] = config.PrintableURL(str)
				continue
			}
			out[k] = v
		default:
			out[k] = v
		}
	}
	return out
}

func describeSection(s map[string]any) string {
	var parts []string
	for _, k := range []string{"provider", "base_url", "api_key_env", "max_results"} {
		if v, ok := s[k]; ok && v != "" {
			parts = append(parts, fmt.Sprintf("%s %v", k, v))
		}
	}
	if len(parts) == 0 {
		return "provider duckduckgo, the default"
	}
	return strings.Join(parts, ", ")
}

func sameJSONValue(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// logAdmin appends e to the admin log beside the managed file. One who may
// not write there, which a refused attempt usually means, is logged in their
// own ~/.abhed/admin.jsonl instead, and told so.
func logAdmin(e adminEntry, errw io.Writer) {
	// The system log first: the file below is one its writer can erase.
	if err := sysLogWrite(sysLogMessage(e)); err != nil {
		fmt.Fprintf(errw, "abhed admin: the attempt is not in the system log: %v\n", err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	primary := filepath.Join(filepath.Dir(managed.ConfigFile), adminLogName)
	if appendLine(primary, line) == nil {
		return
	}
	home, err := os.UserHomeDir()
	if err == nil {
		own := filepath.Join(home, ".abhed", adminLogName)
		if err = os.MkdirAll(filepath.Dir(own), 0o700); err == nil {
			if err = appendLine(own, line); err == nil {
				fmt.Fprintf(errw, "abhed admin: the attempt is logged in %s; %s is not writable\n", config.Printable(own), config.Printable(primary))
				return
			}
		}
	}
	fmt.Fprintf(errw, "abhed admin: the attempt could not be logged: %v\n", err)
}

// appendLine appends one line to path, which must be a plain file if it exists.
func appendLine(path string, line []byte) error {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640) // #nosec G302 G304 -- the admin log beside the managed file
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
