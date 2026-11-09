package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// AllowanceRequest is whose session an allowance is asked for.
type AllowanceRequest struct {
	Tenant  string
	User    string
	Session string
}

// Allowance is what an edition's limits leave a session's owner.
type Allowance struct {
	// Stop ends the run as budget.stopped; Reason says which limit is spent.
	Stop   bool
	Reason string
	// USD marks a dollar limit in force: a model with no price is refused.
	USD bool
	// Scope names the limit, as "user bob, monthly".
	Scope string
}

// allowanceWait bounds one ask, so a slow store cannot hold a turn.
var allowanceWait = 10 * time.Second

// allowance is the session's turn-boundary ask of Options.SessionAllowance,
// or nil when there is none. It fails closed: an error, a panic or no
// answer within allowanceWait stops the run. A run being interrupted is not
// waited for; the loop then ends it as stopped.
func (s *Server) allowance(spec StartSpec, sessionID string) func(context.Context) agent.Allowance {
	if s.opts.SessionAllowance == nil {
		return nil
	}
	req := AllowanceRequest{Tenant: spec.Tenant, User: spec.User, Session: sessionID}
	unavailable := agent.Allowance{Stop: true, Scope: "unavailable",
		Message: "stopped: the budgets that apply to this session could not be checked; try again shortly"}
	return func(ctx context.Context) agent.Allowance {
		ctx, cancel := context.WithTimeout(ctx, allowanceWait)
		defer cancel()
		type answer struct {
			a   Allowance
			err error
		}
		done := make(chan answer, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					done <- answer{err: fmt.Errorf("the allowance panicked: %v", p)}
				}
			}()
			a, err := s.opts.SessionAllowance(ctx, req)
			done <- answer{a, err}
		}()
		var got answer
		select {
		case got = <-done:
		case <-ctx.Done():
			got.err = ctx.Err()
		}
		if got.err != nil {
			if errors.Is(got.err, context.Canceled) {
				return agent.Allowance{} // interrupted: the loop ends the run as stopped
			}
			s.log.Error("session allowance unavailable", "session", sessionID, "err", got.err)
			return unavailable
		}
		a := got.a
		return agent.Allowance{Stop: a.Stop, USD: a.USD, Scope: a.Scope, Message: a.Reason}
	}
}
