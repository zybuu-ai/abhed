package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/metrics"
)

// Metrics is the Prometheus metric set a server counts into: what the
// session record says, how the API is used and how the audit is written.
// Make one with NewMetrics, give it to Options.Metrics, and serve Handler
// where it may be read; a zero Metrics counts nothing.
type Metrics struct{ m *metrics.Abhed }

// NewMetrics makes an empty metric set.
func NewMetrics() *Metrics { return &Metrics{m: metrics.New()} }

// Handler serves the metrics in the Prometheus text format. A nil or zero
// Metrics, which counts nothing, serves an empty exposition.
func (m *Metrics) Handler() http.Handler {
	if a := m.abhed(); a != nil {
		return a.Registry.Handler()
	}
	return metrics.NewRegistry().Handler()
}

// AuditLog wraps a control-plane audit log so each write is timed and each
// sandbox operation's outcome counted; a log that can be read stays readable.
// A nil or zero Metrics returns a as it is.
func (m *Metrics) AuditLog(a controlplane.AuditLog) controlplane.AuditLog {
	if m.abhed() == nil || a == nil {
		return a
	}
	c := countedLog{AuditLog: a, m: m.m}
	if rd, ok := a.(controlplane.AuditReader); ok {
		return countedReader{countedLog: c, AuditReader: rd}
	}
	return c
}

type countedLog struct {
	controlplane.AuditLog
	m *metrics.Abhed
}

func (c countedLog) Append(ctx context.Context, ev controlplane.AuditEvent) error {
	start := time.Now()
	err := c.AuditLog.Append(ctx, ev)
	c.m.Audit(ev.Action, ev.Outcome, !strings.HasPrefix(ev.Action, APIAuditPrefix), time.Since(start), err)
	return err
}

type countedReader struct {
	countedLog
	controlplane.AuditReader
}

func (c countedReader) Append(ctx context.Context, ev controlplane.AuditEvent) error {
	return c.countedLog.Append(ctx, ev)
}

// abhed is the set inside, nil for none.
func (m *Metrics) abhed() *metrics.Abhed {
	if m == nil {
		return nil
	}
	return m.m
}
