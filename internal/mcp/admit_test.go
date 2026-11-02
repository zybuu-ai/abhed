package mcp

import (
	"context"
	"errors"
	"testing"
)

// A server Admit refuses is never started, is listed with why, and a restart
// does not start it; one it admits goes on to start as before.
func TestGatewayAdmit(t *testing.T) {
	g := NewGateway()
	defer g.Close()
	asked := map[string]int{}
	g.Admit = func(c ServerConfig) error {
		asked[c.Name]++
		if c.Name == "refused" {
			return errors.New("the lists leave it out")
		}
		return nil
	}
	errs := g.Connect(context.Background(), []ServerConfig{
		{Name: "refused", Command: "/nonexistent/should-not-run", Enabled: true},
		{Name: "admitted", Command: "/nonexistent/admitted-server", Enabled: true},
		{Name: "off", Command: "/nonexistent/off", Enabled: false},
	})
	if len(errs) != 2 {
		t.Fatalf("errors: %v", errs)
	}
	var refusedErr, admittedErr error
	for _, err := range errs {
		if errors.Is(err, ErrNotAdmitted) {
			refusedErr = err
		} else {
			admittedErr = err
		}
	}
	if refusedErr == nil || admittedErr == nil {
		t.Fatalf("want one refusal and one start failure: %v", errs)
	}
	if asked["off"] != 0 {
		t.Error("a disabled server was judged")
	}
	var st ServerStatus
	for _, s := range g.Servers() {
		if s.Name == "refused" {
			st = s
		}
	}
	if !st.NotAdmitted || st.Connected || !errors.Is(st.Err, ErrNotAdmitted) {
		t.Fatalf("status: %+v", st)
	}
	if err := g.Restart(context.Background(), "refused"); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("restart: %v", err)
	}
	if asked["refused"] < 2 {
		t.Error("the restart was not judged again")
	}
}
