// Package bgjob is the protocol of background jobs: agents a person starts on
// an Abhed server, which run on as that person after the command line or
// Studio that started them has gone. A job is an ordinary server session
// that the server tracks; its record, changes and approvals are the
// session's own routes.
//
// The server side is an edition feature (server.background). This package
// is only the wire types, the paths and a client, so the command line and
// Studio can speak to any server that serves them.
package bgjob

import (
	"encoding/json"
	"time"
)

// Paths the server serves.
const (
	// PathJobs is POST to start a job and GET for the caller's own.
	PathJobs = "/v1/bg/jobs"
	// PathApprovals is GET for every approval the caller's jobs wait on.
	PathApprovals = "/v1/bg/approvals"
	// PathAdminJobs is GET, for an administrator, for every job in the tenant.
	PathAdminJobs = "/v1/admin/bg/jobs"
)

// JobPath is one job's path; CancelPath stops it.
func JobPath(id string) string    { return PathJobs + "/" + id }
func CancelPath(id string) string { return JobPath(id) + "/cancel" }

// State is where a job is. Running and Waiting are live; the rest are final.
type State string

const (
	StateRunning State = "running"
	// StateWaiting is a running job whose agent waits on its owner's answer.
	StateWaiting State = "waiting"
	StateDone    State = "done"
	StateFailed  State = "failed"
	// StateCancelled is a job its owner or an administrator stopped.
	StateCancelled State = "cancelled"
	// StateTimedOut is a job stopped because an approval went unanswered
	// for the server's timeout: nothing it asked for was approved.
	StateTimedOut State = "timed_out"
)

// Final reports whether s is a state a job does not leave.
func (s State) Final() bool {
	switch s {
	case StateDone, StateFailed, StateCancelled, StateTimedOut:
		return true
	}
	return false
}

// Workspace kinds.
const (
	// WorkspaceSession is the session's own sandbox folder, as the server
	// gives every session; the default.
	WorkspaceSession = "session"
	// WorkspaceClone is a repository the server clones into that folder.
	WorkspaceClone = "clone"
	// WorkspaceSnapshot is files the caller uploads into it.
	WorkspaceSnapshot = "snapshot"
)

// Workspace is what a job starts with.
type Workspace struct {
	Kind string `json:"kind,omitempty"`
	// Repo and Ref name what a clone takes.
	Repo string `json:"repo,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// StartRequest starts a job. It names no identity, sandbox tier, secrets or
// network: those are the caller's sign-in and the server's policy.
type StartRequest struct {
	Prompt string `json:"prompt"`
	// Name is a label for lists; the server makes one from the prompt if empty.
	Name string `json:"name,omitempty"`
	// Mode is "plan", the server's own mode, or empty for the server's.
	Mode      string    `json:"mode,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Workspace Workspace `json:"workspace,omitzero"`
}

// Approval is one ask a job waits on. It is answered on the job's session:
// POST /v1/sessions/{session_id}/approve with request_id.
type Approval struct {
	JobID     string `json:"job_id"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id,omitempty"`
	Tool      string `json:"tool"`
	// Args are the call's arguments as the session's record keeps them,
	// secrets withheld: what the owner is asked to allow. Sent to the owner
	// only, never to an administrator or a notice.
	Args     json.RawMessage `json:"args,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Subagent string          `json:"subagent,omitempty"`
	Since    time.Time       `json:"since"`
	// Deadline is when the job is stopped if the ask is still unanswered.
	Deadline time.Time `json:"deadline"`
}

// Job is one background job as the server reports it.
type Job struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	// Prompt is shown to the owner only; an administrator's list omits it.
	Prompt    string    `json:"prompt,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Workspace Workspace `json:"workspace,omitzero"`
	State     State     `json:"state"`
	// SessionID is the job's session: its record, changes and approvals.
	SessionID string    `json:"session_id"`
	Created   time.Time `json:"created"`
	Ended     time.Time `json:"ended,omitzero"`
	// Reason is how the session ended, as its record says; Error says why a
	// job failed or was stopped.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// Waiting is the ask the job waits on, when it does.
	Waiting *Approval `json:"waiting,omitempty"`
}

// JobList is a list of jobs, newest first.
type JobList struct {
	Jobs []Job `json:"jobs"`
}

// ApprovalList is the asks a person's jobs wait on, oldest first.
type ApprovalList struct {
	Approvals []Approval `json:"approvals"`
}
