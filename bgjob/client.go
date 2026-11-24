package bgjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/profiles"
)

// Client speaks to one server as one signed-in profile.
type Client struct {
	Profile *profiles.Profile
	HTTP    *http.Client
	// Saved keeps a refreshed token; nil keeps it in memory only.
	Saved func(*profiles.Profile) error
}

// Error is a call the server refused.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the server answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s (%d)", e.Message, e.Status)
}

// StatusOf is the HTTP status a refusal carried, 0 for any other error.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// do sends one call and decodes a JSON answer into out.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	ct := ""
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		ct = "application/json"
	}
	b, err := c.send(ctx, method, path, ct, body, time.Minute)
	if err != nil || out == nil || len(b) == 0 {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("the server's answer could not be read: %w", err)
	}
	return nil
}

// send sends one call with body as contentType and returns the answer.
func (c *Client) send(ctx context.Context, method, path, contentType string, body []byte, timeout time.Duration) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.Profile.Do(cctx, c.HTTP, func() (*http.Request, error) {
		req, err := c.Profile.NewRequest(method, path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return req, nil
	}, c.Saved)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("could not reach %s: %w", c.Profile.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, refused(path, resp.StatusCode, b)
	}
	return b, nil
}

// UploadSnapshot sends a snapshot, a gzip-compressed tar as PathSnapshots
// describes, for a job to start from.
func (c *Client) UploadSnapshot(ctx context.Context, tgz []byte) (Snapshot, error) {
	var snap Snapshot
	if len(tgz) > MaxSnapshotUpload {
		return snap, fmt.Errorf("the snapshot is %d MiB compressed, over the %d MiB a server takes", len(tgz)>>20, MaxSnapshotUpload>>20)
	}
	b, err := c.send(ctx, http.MethodPost, PathSnapshots, "application/gzip", tgz, 10*time.Minute)
	if err != nil {
		return snap, err
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return snap, fmt.Errorf("the server's answer could not be read: %w", err)
	}
	return snap, nil
}

// JobPatch is the patch a job kept when it ended (see Job.Result).
func (c *Client) JobPatch(ctx context.Context, id string) (string, error) {
	b, err := c.send(ctx, http.MethodGet, PatchPath(url.PathEscape(id)), "", nil, time.Minute)
	return string(b), err
}

// refused reads a refusal. A server without background jobs answers their
// paths with the router's bare 404, which says nothing a person can act on.
func refused(path string, status int, b []byte) error {
	text := strings.TrimSpace(string(b))
	if status == http.StatusNotFound && text == "404 page not found" && strings.HasPrefix(path, "/v1/bg/") {
		return &Error{Status: status, Message: "this server runs no background jobs: it is not an edition " +
			"that has them, or server.background.enabled is off"}
	}
	var e struct {
		Error any `json:"error"`
	}
	out := &Error{Status: status}
	if json.Unmarshal(b, &e) == nil {
		switch v := e.Error.(type) {
		case string:
			out.Message = v
		case map[string]any:
			out.Message, _ = v["message"].(string)
		}
	}
	return out
}

// Start starts a job.
func (c *Client) Start(ctx context.Context, req StartRequest) (Job, error) {
	var j Job
	return j, c.do(ctx, http.MethodPost, PathJobs, req, &j)
}

// List is the caller's jobs, newest first.
func (c *Client) List(ctx context.Context) ([]Job, error) {
	var l JobList
	err := c.do(ctx, http.MethodGet, PathJobs, nil, &l)
	return l.Jobs, err
}

// Get is one of the caller's jobs.
func (c *Client) Get(ctx context.Context, id string) (Job, error) {
	var j Job
	return j, c.do(ctx, http.MethodGet, JobPath(url.PathEscape(id)), nil, &j)
}

// Cancel stops one of the caller's jobs and returns it as it now is.
func (c *Client) Cancel(ctx context.Context, id string) (Job, error) {
	var j Job
	return j, c.do(ctx, http.MethodPost, CancelPath(url.PathEscape(id)), struct{}{}, &j)
}

// Approvals is every ask the caller's jobs wait on.
func (c *Client) Approvals(ctx context.Context) ([]Approval, error) {
	var l ApprovalList
	err := c.do(ctx, http.MethodGet, PathApprovals, nil, &l)
	return l.Approvals, err
}

// Answer answers an ask on its job's session, as the console does.
func (c *Client) Answer(ctx context.Context, a Approval, approved bool) error {
	body := map[string]any{"approved": approved, "request_id": a.RequestID}
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(a.SessionID)+"/approve", body, nil)
}

// AnswerRefused begins the reason a server's approval gate gives when it
// refuses an answer as a decision (server.ErrAnswerRefused), such as the
// owner's own approval where a workspace needs other approvers.
const AnswerRefused = "this answer is refused"

// IsAnswerRefused reports whether err is the approval gate refusing an
// answer, which leaves the ask waiting, rather than any other 403, such as
// a revoked owner.
func IsAnswerRefused(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusForbidden && strings.HasPrefix(e.Message, AnswerRefused)
}

// AnswerAlways allows an ask and, for the rest of its job, every later call
// the ask's scope covers: the scope is remembered on the job's session only.
func (c *Client) AnswerAlways(ctx context.Context, a Approval) error {
	if a.Scope == "" {
		return errors.New("this ask offers no rule to always allow; answer it once")
	}
	body := map[string]any{"approved": true, "request_id": a.RequestID, "scope": a.Scope}
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(a.SessionID)+"/approve", body, nil)
}

// Event is one event of a job's record.
type Event struct {
	ID        string          `json:"id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// Events is a job session's record so far.
func (c *Client) Events(ctx context.Context, sessionID string) ([]Event, error) {
	var evs []Event
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/replay", nil, &evs)
	if StatusOf(err) == http.StatusNotFound {
		return nil, nil // nothing recorded yet
	}
	return evs, err
}

// Patch is what a job's session changed, as one unified diff; ok is false
// when the server that ran it no longer holds the originals to diff against.
func (c *Client) Patch(ctx context.Context, sessionID string) (patch string, ok bool, err error) {
	var ch struct {
		Available bool `json:"available"`
		Files     []struct {
			Path string `json:"path"`
			Diff string `json:"diff"`
			Note string `json:"note"`
		} `json:"files"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/changes", nil, &ch); err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, f := range ch.Files {
		if f.Diff == "" {
			fmt.Fprintf(&b, "# %s: no diff (%s)\n", f.Path, f.Note)
			continue
		}
		b.WriteString(f.Diff)
		if !strings.HasSuffix(f.Diff, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), ch.Available, nil
}
