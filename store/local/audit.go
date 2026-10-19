package local

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store/chain"
)

// AuditFile is the control-plane audit record of a server without Postgres:
// <records>/<tenant>/audit/api.jsonl, one canonical JSON line an event, each
// carrying the hash of the line before it as a session's lines do. Several
// processes may append; each takes the file's lock for the write.
type AuditFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	lock *os.File
}

// auditLine is one event as the file holds it, fields in this order.
type auditLine struct {
	Seq        int64             `json:"seq"`
	At         string            `json:"at"`
	Principal  string            `json:"principal"`
	Action     string            `json:"action"`
	SandboxID  string            `json:"sandbox_id"`
	Generation int64             `json:"generation"`
	Outcome    string            `json:"outcome"`
	Code       string            `json:"code,omitempty"`
	Detail     map[string]string `json:"detail,omitempty"`
	Prev       string            `json:"prev"`
	Hash       string            `json:"hash,omitempty"`
}

func (l *auditLine) seal() ([]byte, error) {
	l.Hash = ""
	body, err := encode(l)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	l.Hash = hex.EncodeToString(sum[:])
	return encode(l)
}

// AuditPath is where the audit file of a tenant is under a records directory.
func AuditPath(dir, tenant string) string {
	return filepath.Join(dir, tenant, "audit", "api.jsonl")
}

// OpenAudit opens, creating when needed, the tenant's audit file under dir
// (the records directory; "" is ~/.abhed/records). Folders are 0700 and the
// file 0600; a folder that is a link is refused.
func OpenAudit(dir, tenant string) (*AuditFile, error) {
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	if tenant == "" {
		tenant = "default"
	}
	if err := checkID("tenant", tenant); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	// The marker puts the folder in the state class, as Open does.
	marker := filepath.Join(dir, tools.RecordMarker)
	if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(marker, []byte("Abhed's local record. Check it with: abhed record verify\n"), 0o600); err != nil {
			return nil, err
		}
	}
	path := AuditPath(dir, tenant)
	for _, d := range []string{filepath.Dir(filepath.Dir(path)), filepath.Dir(path)} {
		if info, err := os.Lstat(d); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a link; the record's folders must be its own", d)
		}
		if err := privateDir(d); err != nil {
			return nil, err
		}
	}
	f, err := openOwn(path, os.O_RDWR|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("open the audit record: %w", err)
	}
	lock, err := openOwn(path+".lock", os.O_RDWR|os.O_CREATE)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("open the audit record's lock: %w", err)
	}
	return &AuditFile{path: path, f: f, lock: lock}, nil
}

// Path is the file's path.
func (a *AuditFile) Path() string { return a.path }

// Append writes ev after the last line, linked to it. A torn last line, as
// a crash leaves, is cut off first so the chain goes on from a whole one.
func (a *AuditFile) Append(_ context.Context, ev controlplane.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return errors.New("the audit record is closed")
	}
	if err := waitLock(a.lock); err != nil {
		return fmt.Errorf("lock the audit record: %w", err)
	}
	defer func() { _ = unlock(a.lock) }()
	last, size, err := lastAuditLine(a.f)
	if err != nil {
		return err
	}
	if st, err := a.f.Stat(); err == nil && st.Size() != size {
		if err := a.f.Truncate(size); err != nil {
			return fmt.Errorf("cut the audit record's torn line: %w", err)
		}
	}
	prev := Genesis
	if last != nil {
		prev = last.Hash
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now()
	}
	l := auditLine{
		Seq: 1, At: at.UTC().Format(timeFormat), Principal: ev.Principal, Action: ev.Action,
		SandboxID: ev.SandboxID, Generation: ev.Generation, Outcome: ev.Outcome, Code: ev.Code,
		Detail: ev.Detail, Prev: prev,
	}
	if last != nil {
		l.Seq = last.Seq + 1
	}
	b, err := l.seal()
	if err != nil {
		return err
	}
	if _, err := writeLine(a.f, append(b, '\n')); err != nil {
		_ = a.f.Truncate(size)
		return fmt.Errorf("write the audit record: %w", err)
	}
	return nil
}

// Sync flushes the file to disk.
func (a *AuditFile) Sync() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	return a.f.Sync()
}

// Close syncs and closes the file.
func (a *AuditFile) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := errors.Join(a.f.Sync(), a.f.Close(), a.lock.Close())
	a.f, a.lock = nil, nil
	return err
}

// AuditFor lists the events of one sandbox, oldest first; "" lists the API's.
func (a *AuditFile) AuditFor(_ context.Context, id string) ([]controlplane.AuditEvent, error) {
	lines, _, err := readAudit(a.path)
	if err != nil {
		return nil, err
	}
	var out []controlplane.AuditEvent
	for _, l := range lines {
		if l.SandboxID != id {
			continue
		}
		at, _ := time.Parse(timeFormat, l.At)
		out = append(out, controlplane.AuditEvent{Seq: l.Seq, At: at, Principal: l.Principal, Action: l.Action,
			SandboxID: l.SandboxID, Generation: l.Generation, Outcome: l.Outcome, Code: l.Code, Detail: l.Detail})
	}
	return out, nil
}

// AuditReport is what VerifyAudit found.
type AuditReport struct {
	Lines int64
	Head  string
	// FirstBad is the first line whose link or hash does not hold; 0 when all do.
	FirstBad int64
	Why      string
	// Torn is set when the last line is incomplete, as a crash leaves it.
	Torn bool
}

// OK reports whether every line checks out.
func (r AuditReport) OK() bool { return r.FirstBad == 0 }

// VerifyAudit walks an audit file's chain: each line's hash over its own
// bytes, its link to the line before, and its sequence.
func VerifyAudit(path string) (AuditReport, error) {
	f, err := os.Open(path) // #nosec G304 -- a record path the caller names
	if err != nil {
		return AuditReport{}, err
	}
	defer f.Close()
	var r AuditReport
	prev, n := Genesis, int64(0)
	br := bufio.NewReader(f)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			r.Torn = true
			break
		}
		if len(raw) > 0 {
			n++
			if why := checkAuditLine(bytes.TrimSuffix(raw, []byte("\n")), prev, n); why != "" && r.FirstBad == 0 {
				r.FirstBad, r.Why = n, why
			}
			var l auditLine
			_ = json.Unmarshal(raw, &l)
			prev = l.Hash
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return r, err
		}
	}
	r.Lines, r.Head = n, prev
	if n == 0 {
		r.Head = ""
	}
	return r, nil
}

func checkAuditLine(raw []byte, prev string, n int64) string {
	var l auditLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return "not JSON: " + err.Error()
	}
	if l.Prev != prev {
		return "its prev is not the hash of the line before"
	}
	if l.Seq != n {
		return fmt.Sprintf("its seq is %d, not %d", l.Seq, n)
	}
	got := l.Hash
	b, err := l.seal()
	if err != nil {
		return err.Error()
	}
	if l.Hash != got || !bytes.Equal(b, raw) {
		return "its hash does not match its contents"
	}
	return ""
}

// lastAuditLine is the file's last whole line and the length up to its end.
func lastAuditLine(f *os.File) (*auditLine, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if size == 0 {
		return nil, 0, nil
	}
	// Read back far enough for the last line; an event is small.
	chunk := int64(64 << 10)
	for {
		start := max(size-chunk, 0)
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		end := int64(len(buf))
		if buf[end-1] != '\n' {
			// Torn: drop what follows the last newline.
			i := bytes.LastIndexByte(buf, '\n')
			if i < 0 && start > 0 {
				chunk *= 2
				continue
			}
			end = int64(i + 1)
		}
		whole := buf[:end]
		if len(whole) == 0 {
			return nil, start, nil
		}
		body := whole[:len(whole)-1]
		i := bytes.LastIndexByte(body, '\n')
		if i < 0 && start > 0 {
			chunk *= 2
			continue
		}
		var l auditLine
		if err := json.Unmarshal(body[i+1:], &l); err != nil {
			return nil, 0, fmt.Errorf("the audit record's last line is not JSON: %w", err)
		}
		return &l, start + end, nil
	}
}

func readAudit(path string) ([]auditLine, int64, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the record's own file
	if err != nil {
		return nil, 0, err
	}
	var out []auditLine
	for _, raw := range bytes.Split(b, []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var l auditLine
		if json.Unmarshal(raw, &l) == nil {
			out = append(out, l)
		}
	}
	return slices.Clip(out), int64(len(b)), nil
}

// AuditChain is the tenant's audit file under dir ("" is ~/.abhed/records)
// as a chain.Source; positions are its lines, and the scope is the tenant.
func AuditChain(dir string) chain.Source { return auditSource{dir} }

type auditSource struct{ dir string }

func (a auditSource) path(tenant string) (string, error) {
	dir := a.dir
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	if tenant == "" {
		tenant = "default"
	}
	if err := checkID("tenant", tenant); err != nil {
		return "", err
	}
	return AuditPath(dir, tenant), nil
}

func (a auditSource) Head(_ context.Context, tenant string) (chain.Head, error) {
	p, err := a.path(tenant)
	if err != nil {
		return chain.Head{}, err
	}
	h := chain.Head{Chain: "local/audit", Tenant: tenant, Scope: tenant}
	f, err := os.Open(p) // #nosec G304 -- the record's own file
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	defer f.Close()
	l, _, err := lastAuditLine(f)
	if err != nil || l == nil {
		return h, err
	}
	h.Pos, h.Hash = l.Seq, l.Hash
	return h, nil
}

// VerifyRange checks the whole file, as VerifyAudit does, and reports
// whether lines from to to (0 is the last) are among those that hold.
func (a auditSource) VerifyRange(_ context.Context, tenant string, from, to int64) (chain.Range, error) {
	p, err := a.path(tenant)
	if err != nil {
		return chain.Range{}, err
	}
	if from < 1 {
		from = 1
	}
	rep, err := VerifyAudit(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return chain.Range{}, err
	}
	if to <= 0 {
		to = rep.Lines
	}
	r := chain.Range{From: from, To: to, Before: Genesis}
	if !rep.OK() && rep.FirstBad <= to {
		r.FirstBad, r.Reason = max(rep.FirstBad, from), fmt.Sprintf("line %d: %s", rep.FirstBad, rep.Why)
		return r, nil
	}
	lines, _, err := readAudit(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return chain.Range{}, err
	}
	if from > 1 && from-1 <= int64(len(lines)) {
		r.Before = lines[from-2].Hash
	}
	switch {
	case to == 0:
	case to > int64(len(lines)):
		r.FirstBad = int64(len(lines)) + 1
		r.Reason = fmt.Sprintf("the file holds no line %d: lines were cut from its end", to)
	default:
		r.Hash = lines[to-1].Hash
	}
	return r, nil
}
