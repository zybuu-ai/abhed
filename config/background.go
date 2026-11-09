package config

import (
	"fmt"
	"net/url"
	"time"
)

// ServerBackgroundConfig is server.background: agents a person starts on the
// server from the command line or Studio, which run on after they leave. An
// edition feature; a binary without it refuses enabled.
type ServerBackgroundConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// ApprovalTimeoutSeconds is how long a job waits for its owner's answer
	// before it is stopped with nothing approved. Zero is an hour.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds,omitempty"`
	// MaxRunning bounds the jobs one person has running at once. Zero is 2.
	MaxRunning int `json:"max_running,omitempty"`
	// NotifyURL is sent a POST when a job waits on its owner and when it
	// ends: the job, the tool and the reason, never the call's arguments.
	NotifyURL string `json:"notify_url,omitempty"`
}

// ApprovalTimeout is approval_timeout_seconds with its default.
func (b ServerBackgroundConfig) ApprovalTimeout() time.Duration {
	if b.ApprovalTimeoutSeconds <= 0 {
		return time.Hour
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
	if b.ApprovalTimeoutSeconds < 0 {
		return fmt.Errorf("server.background.approval_timeout_seconds %d is negative", b.ApprovalTimeoutSeconds)
	}
	if b.MaxRunning < 0 {
		return fmt.Errorf("server.background.max_running %d is negative", b.MaxRunning)
	}
	if b.NotifyURL != "" {
		u, err := url.Parse(b.NotifyURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return fmt.Errorf("server.background.notify_url %q: use an http or https URL with no credentials in it", b.NotifyURL)
		}
	}
	return nil
}
