package ui

import (
	"context"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// supervisorChip is the supervisor's verdict on the call being asked about,
// as one line for the approval card, or "" when it gave none.
func supervisorChip(ctx context.Context) (string, bool) {
	req, ok := agent.RequestedOf(ctx)
	if !ok || req.Supervisor == nil {
		return "", false
	}
	v := req.Supervisor
	if v.Code == "" || v.Code == "ok" {
		return "supervisor: ok", false
	}
	line := "supervisor: " + VisibleLine(v.Decision) + " · " + VisibleLine(v.Code)
	if v.Advice != "" {
		line += " — " + VisibleLine(v.Advice)
	}
	return line, true
}
