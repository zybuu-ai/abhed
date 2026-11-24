package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"time"
)

// ServerBackgroundConfig is server.background: agents a person starts on the
// server from the command line or Studio, which run on after they leave. An
// edition feature; a binary without it refuses enabled.
type ServerBackgroundConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// ApprovalTimeoutSeconds is how long a job waits for its owner's answer
	// before it is denied and the job stopped. Zero is 1740, which is also
	// the most: a minute short of the 30 minutes after which the server
	// denies any ask, so the job's own timeout is the one that fires.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds,omitempty"`
	// MaxRunning bounds the jobs one person has running at once. Zero is 2.
	MaxRunning int `json:"max_running,omitempty"`
	// NotifyURL is sent a POST when a job waits on its owner and when it
	// ends: the job, its owner and the tool, never a prompt or arguments.
	// https, or http to this machine only. Set only by the operator: a
	// workspace's file never sets server.background, trusted or not.
	NotifyURL string `json:"notify_url,omitempty"`
	// SnapshotsWaiting bounds the snapshots the server keeps waiting for
	// their jobs, everyone's together. Zero is 64.
	SnapshotsWaiting int `json:"snapshots_waiting,omitempty"`
	// SnapshotsMB bounds the disk they take together, in MiB. Zero is 2048.
	SnapshotsMB int `json:"snapshots_mb,omitempty"`
}

// SnapshotsLimits are snapshots_waiting and snapshots_mb with their defaults.
func (b ServerBackgroundConfig) SnapshotsLimits() (count int, bytes int64) {
	count, mb := b.SnapshotsWaiting, b.SnapshotsMB
	if count <= 0 {
		count = 64
	}
	if mb <= 0 {
		mb = 2048
	}
	return count, int64(mb) << 20
}

// BackgroundSnapshotDir is where serve keeps snapshots waiting for their
// jobs: beside the control plane's state folder, outside every workspace and
// off a temporary file system.
func BackgroundSnapshotDir() (string, error) {
	state, err := ControlPlaneStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(state), "background", "snapshots"), nil
}

// MaxApprovalTimeout is the longest a job's ask may wait: a minute short of
// the 30 minutes after which a session's ask is denied whoever waits on it.
const MaxApprovalTimeout = 29 * time.Minute

// ApprovalTimeout is approval_timeout_seconds with its default.
func (b ServerBackgroundConfig) ApprovalTimeout() time.Duration {
	if b.ApprovalTimeoutSeconds <= 0 {
		return MaxApprovalTimeout
	}
	return time.Duration(b.ApprovalTimeoutSeconds) * time.Second
}

// Running is max_running with its default.
func (b ServerBackgroundConfig) Running() int {
	if b.MaxRunning <= 0 {
		return 2
	}
	return b.MaxRunning
}

func (b ServerBackgroundConfig) validate() error {
	if b.ApprovalTimeoutSeconds < 0 || time.Duration(b.ApprovalTimeoutSeconds)*time.Second > MaxApprovalTimeout {
		return fmt.Errorf("server.background.approval_timeout_seconds %d: from 0 to %d", b.ApprovalTimeoutSeconds, int(MaxApprovalTimeout.Seconds()))
	}
	if b.SnapshotsWaiting < 0 || b.SnapshotsMB < 0 {
		return fmt.Errorf("server.background.snapshots_waiting and snapshots_mb may not be negative")
	}
	if b.MaxRunning < 0 {
		return fmt.Errorf("server.background.max_running %d is negative", b.MaxRunning)
	}
	if b.NotifyURL != "" {
		u, err := url.Parse(b.NotifyURL)
		if err != nil || u.Host == "" || u.User != nil ||
			(u.Scheme != "https" && (u.Scheme != "http" || !loopbackHost(u.Hostname()))) {
			return fmt.Errorf("server.background.notify_url %q: use https, or http to this machine only, with no credentials in the URL", b.NotifyURL)
		}
	}
	return nil
}
