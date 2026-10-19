package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ssh_put and ssh_get move files to and from a host over SFTP. What is sent
// comes from a workspace file or a stored secret named by reference, so a
// certificate's key reaches the host without passing through the model; what
// is fetched lands in the workspace. The approval and the record keep the
// paths, the size and the sha256, never the content.

// DefaultTransferMax is how many bytes one transfer may carry unless the
// operator sets ssh.transfer_max_mb.
const DefaultTransferMax = 16 << 20

// transferTimeout bounds one transfer.
const transferTimeout = 5 * time.Minute

func init() {
	tools.RegisterSubjector("ssh_put", PutTool{})
	tools.RegisterSubjector("ssh_get", GetTool{})
}

// transferSubject is host:remote_path, as scp writes it, so a rule on one
// host never covers another.
func transferSubject(args json.RawMessage) (string, bool) {
	m, err := tools.DecodeArgs(args)
	if err != nil {
		return "", false
	}
	h, _ := tools.Lookup(m, "host")
	p, _ := tools.Lookup(m, "remote_path")
	host, _ := h.(string)
	rp, _ := p.(string)
	if host == "" {
		return "", false
	}
	return host + ":" + rp, true
}

// checkRemotePath refuses a remote path that is not absolute and clean, so
// a rule on it reads the file the transfer reaches.
func checkRemotePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("remote_path is required: the file's absolute path on the host")
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("remote_path %q must be absolute", p)
	case path.Clean(p) != p:
		return fmt.Errorf("remote_path %q must be clean: no '.', '..', '//' or trailing '/' (did you mean %s?)", p, path.Clean(p))
	case strings.ContainsAny(p, "\x00\r\n") || p == "/":
		return fmt.Errorf("remote_path %q cannot name a file", p)
	}
	return nil
}

// host finds the named host, the session's own first, as the ssh tool does.
func findHost(r *Registry, sess *tools.Session, name string) (*Host, string, error) {
	mine := sessionHostsOf(sess, r, false)
	if h, ok := mine.get(name); ok {
		return h, "added in this session", nil
	}
	if r != nil {
		if h, ok := r.get(name); ok {
			return h, "declared by the operator", nil
		}
	}
	names := knownNames(r, mine)
	if len(names) == 0 {
		return nil, "", fmt.Errorf("no SSH hosts are configured. An operator declares them in " +
			"ssh.hosts, or the user gives one for ssh_connect")
	}
	return nil, "", fmt.Errorf("no host named %q. Configured hosts: %s", name, strings.Join(names, ", "))
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func limitOf(n int64) int64 {
	if n <= 0 {
		return DefaultTransferMax
	}
	return n
}

// ---------------------------------------------------------------- put

// PutTool sends a workspace file or a stored secret to a host.
type PutTool struct {
	R *Registry
	// Secret returns a stored secret's value; nil means no store.
	Secret func(name string) (string, error)
	// Max is the most one transfer carries; 0 is DefaultTransferMax.
	Max int64
}

func (PutTool) Name() string { return "ssh_put" }

// Mutates is true: it changes a file on a host, with no undo.
func (PutTool) Mutates() bool { return true }

// FixedArgs: content sent as an unknown argument is dropped, not recorded.
func (PutTool) FixedArgs() {}

// SecretArgs puts secret to a secret(NAME) rule.
func (PutTool) SecretArgs() []string { return []string{"secret"} }

func (PutTool) PolicySubject(args json.RawMessage) (string, bool) { return transferSubject(args) }

func (PutTool) FilesRead(raw json.RawMessage) []string {
	var a putArgs
	if json.Unmarshal(raw, &a) != nil || a.File == "" {
		return nil
	}
	return []string{a.File}
}

func (PutTool) FilesWritten(json.RawMessage) []string { return nil }

func (PutTool) Description() string {
	return "Send a file to a remote machine over SFTP: a workspace file (file, an absolute path) " +
		"or a stored secret by NAME (secret), such as a TLS key the user stored with " +
		"`abhed secret set NAME`, so the content never passes through you. Writes remote_path " +
		"whole, replacing it. Every call requires approval. Never put file content in a command."
}

func (PutTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "host":{"type":"string","description":"Name of a configured host."},
    "remote_path":{"type":"string","description":"Absolute path of the file on the host."},
    "file":{"type":"string","description":"Absolute path of the workspace file to send."},
    "secret":{"type":"string","description":"NAME of the stored secret to send instead of a file. Never the value."},
    "mode":{"type":"string","description":"Octal permissions for the file, e.g. 0644. Default 0644 for a file, 0600 for a secret."}
  },
  "required":["host","remote_path"]
}`)
}

type putArgs struct {
	Host       string `json:"host"`
	RemotePath string `json:"remote_path"`
	File       string `json:"file"`
	Secret     string `json:"secret"`
	Mode       string `json:"mode"`
}

func (a putArgs) check() (uint32, error) {
	if err := checkRemotePath(a.RemotePath); err != nil {
		return 0, err
	}
	switch {
	case (a.File == "") == (a.Secret == ""):
		return 0, fmt.Errorf("give exactly one of file (a workspace path) or secret (a stored secret's NAME)")
	case a.Secret != "" && !secrets.ValidName(a.Secret):
		return 0, fmt.Errorf("secret is the NAME of a stored secret, such as TLS_KEY, never its value. " +
			"Ask the user to store it with `abhed secret set NAME`")
	}
	mode := uint64(0o644)
	if a.Secret != "" {
		mode = 0o600
	}
	if a.Mode != "" {
		m, err := strconv.ParseUint(a.Mode, 8, 32)
		if err != nil || m > 0o777 {
			return 0, fmt.Errorf("mode %q is not octal permissions such as 0644", a.Mode)
		}
		mode = m
	}
	return uint32(mode), nil
}

// content reads what a call sends, bounded by the limit.
func (t PutTool) content(sess *tools.Session, a putArgs) ([]byte, string, error) {
	limit := limitOf(t.Max)
	if a.Secret != "" {
		if t.Secret == nil {
			return nil, "", fmt.Errorf("no secrets store is available here, so ssh_put cannot read %s", a.Secret)
		}
		v, err := t.Secret(a.Secret)
		if err != nil {
			return nil, "", err
		}
		if int64(len(v)) > limit {
			return nil, "", fmt.Errorf("stored secret %s is more than the %d bytes a transfer may carry", a.Secret, limit)
		}
		return []byte(v), "stored secret " + a.Secret, nil
	}
	if sess == nil {
		return nil, "", fmt.Errorf("ssh_put reads a file only in a session")
	}
	p, err := sess.Resolve(a.File)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(p)
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("cannot read %s: %w", a.File, err)
	case info.IsDir():
		return nil, "", fmt.Errorf("%s is a directory; ssh_put sends one file", a.File)
	case info.Size() > limit:
		return nil, "", fmt.Errorf("%s is %d bytes, more than the %d a transfer may carry", a.File, info.Size(), limit)
	}
	data, err := sess.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read %s: %w", a.File, err)
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("%s is more than the %d bytes a transfer may carry", a.File, limit)
	}
	return data, "file " + a.File, nil
}

// Precheck refuses, before anyone is asked, a call that cannot succeed: an
// unknown host, a bad path, or a source that cannot be read.
func (t PutTool) Precheck(sess *tools.Session, raw json.RawMessage) error {
	var a putArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	if a.Host == "" {
		return nil // nothing names a host yet; Run says what is missing
	}
	if _, err := a.check(); err != nil {
		return err
	}
	if _, _, err := findHost(t.R, sess, a.Host); err != nil {
		return err
	}
	_, _, err := t.content(sess, a)
	return err
}

// Target names the account and address and where the content comes from.
// It reads nothing: it is shown and recorded for a denied call too, and
// the result gives the size and sha256 of what was sent.
func (t PutTool) Target(sess *tools.Session, raw json.RawMessage) string {
	var a putArgs
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	mode, err := a.check()
	if err != nil {
		return ""
	}
	h, origin, err := findHost(t.R, sess, a.Host)
	if err != nil {
		return ""
	}
	what := "file " + a.File
	if a.Secret != "" {
		what = "stored secret " + a.Secret
	}
	return fmt.Sprintf("writes %s on %s@%s, %s, from %s, mode %04o", a.RemotePath, h.User(), h.Addr(), origin, what, mode)
}

func (t PutTool) Run(ctx context.Context, sess *tools.Session, raw json.RawMessage) tools.Result {
	var a putArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for ssh_put: %v", err)
	}
	mode, err := a.check()
	if err != nil {
		return errf("%v", err)
	}
	h, _, err := findHost(t.R, sess, a.Host)
	if err != nil {
		return errf("%v", err)
	}
	data, what, err := t.content(sess, a)
	if err != nil {
		return errf("%v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	if err := h.Put(ctx, a.RemotePath, data, mode); err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: fmt.Sprintf("Wrote %s on %s@%s from %s: %d bytes, sha256 %s, mode %04o.",
		a.RemotePath, h.User(), h.Name(), what, len(data), digest(data), mode)}
}

// ---------------------------------------------------------------- get

// GetTool fetches a file from a host into the workspace.
type GetTool struct {
	R *Registry
	// Max is the most one transfer carries; 0 is DefaultTransferMax.
	Max int64
}

func (GetTool) Name() string { return "ssh_get" }

// Mutates is true: it writes a workspace file, and reads a host the
// operator may not want read without asking.
func (GetTool) Mutates() bool { return true }

func (GetTool) FixedArgs() {}

func (GetTool) PolicySubject(args json.RawMessage) (string, bool) { return transferSubject(args) }

func (GetTool) FilesRead(json.RawMessage) []string { return nil }

func (GetTool) FilesWritten(raw json.RawMessage) []string {
	var a getArgs
	if json.Unmarshal(raw, &a) != nil || a.File == "" {
		return nil
	}
	return []string{a.File}
}

func (GetTool) Description() string {
	return "Fetch a file from a remote machine over SFTP into the workspace (file, an absolute " +
		"path), to read or compare it there. An existing workspace file is replaced only if you " +
		"have read it this session. Every call requires approval unless a rule allows it."
}

func (GetTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "host":{"type":"string","description":"Name of a configured host."},
    "remote_path":{"type":"string","description":"Absolute path of the file on the host."},
    "file":{"type":"string","description":"Absolute workspace path to write it to."}
  },
  "required":["host","remote_path","file"]
}`)
}

type getArgs struct {
	Host       string `json:"host"`
	RemotePath string `json:"remote_path"`
	File       string `json:"file"`
}

func (a getArgs) check(sess *tools.Session) (string, error) {
	if err := checkRemotePath(a.RemotePath); err != nil {
		return "", err
	}
	if sess == nil {
		return "", fmt.Errorf("ssh_get writes a file only in a session")
	}
	if a.File == "" {
		return "", fmt.Errorf("file is required: the absolute workspace path to write to")
	}
	return sess.Resolve(a.File)
}

// Precheck refuses an unknown host, a bad path, or a destination the
// session cannot write, before anyone is asked.
func (t GetTool) Precheck(sess *tools.Session, raw json.RawMessage) error {
	var a getArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	if a.Host == "" {
		return nil // nothing names a host yet; Run says what is missing
	}
	p, err := a.check(sess)
	if err != nil {
		return err
	}
	if _, _, err := findHost(t.R, sess, a.Host); err != nil {
		return err
	}
	return sess.CanWriteFetched(p)
}

func (t GetTool) Target(sess *tools.Session, raw json.RawMessage) string {
	var a getArgs
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	h, origin, err := findHost(t.R, sess, a.Host)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("reads %s on %s@%s, %s, into %s (at most %d bytes)",
		a.RemotePath, h.User(), h.Addr(), origin, a.File, limitOf(t.Max))
}

func (t GetTool) Run(ctx context.Context, sess *tools.Session, raw json.RawMessage) tools.Result {
	var a getArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for ssh_get: %v", err)
	}
	p, err := a.check(sess)
	if err != nil {
		return errf("%v", err)
	}
	h, _, err := findHost(t.R, sess, a.Host)
	if err != nil {
		return errf("%v", err)
	}
	if err := sess.CanWriteFetched(p); err != nil {
		return errf("%v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	data, err := h.Get(ctx, a.RemotePath, limitOf(t.Max))
	if err != nil {
		if isNoSuchFile(err) {
			return errf("%s does not exist on %s.", a.RemotePath, h.Name())
		}
		return errf("%v", err)
	}
	verb, err := sess.WriteFetched(p, data)
	if err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: fmt.Sprintf("%s %s from %s@%s:%s: %d bytes, sha256 %s. Read it with the read tool.",
		verb, sess.Rel(p), h.User(), h.Name(), a.RemotePath, len(data), digest(data))}
}
