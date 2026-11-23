package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// bashSeqModel answers with one bash call per command, in order, then a
// closing message.
func bashSeqModel(t *testing.T, commands ...string) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		i := int(n.Add(1)) - 1
		if i < len(commands) {
			args, _ := json.Marshal(map[string]string{"command": commands[i], "description": "probe"})
			call, _ := json.Marshal(string(args))
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+fmt.Sprint(i+1)+`","function":{"name":"bash","arguments":`+string(call)+`}}]}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// approvalOf returns the approval line in out, or fails.
func approvalOf(t *testing.T, out string) rpcApproval {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		var r struct {
			Type     string       `json:"type"`
			Approval *rpcApproval `json:"approval"`
		}
		if json.Unmarshal([]byte(l), &r) == nil && r.Type == "approval" && r.Approval != nil {
			return *r.Approval
		}
	}
	t.Fatalf("no approval line in:\n%s", out)
	return rpcApproval{}
}

// The caller answers an ask over the protocol, and the record says who did.
func TestRPCApprovalAllowedByTheCaller(t *testing.T) {
	srv := bashSeqModel(t, "touch made-by-agent")
	ws := rpcWorkspace(t, srv.URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start","approvals":true}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"make a file"}`)
	ask := approvalOf(t, s.waitFor(t, `"type":"approval"`))
	if ask.Tool != "bash" || ask.RequestID == "" || !strings.Contains(string(ask.Args), "touch made-by-agent") {
		t.Fatalf("approval does not name the call: %+v", ask)
	}
	s.send(`{"id":"3","method":"approve","request_id":"` + ask.RequestID + `","decision":"allow","approver":"olga@example.com"}`)
	out := s.waitFor(t, `"id":"2"`)
	if !strings.Contains(out, `"type":"answered"`) {
		t.Errorf("the approve request was not answered:\n%s", out)
	}
	if !strings.Contains(out, `"action.approved"`) || !strings.Contains(out, `olga@example.com`) {
		t.Errorf("the approval was not recorded with its approver:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "made-by-agent")); err != nil {
		t.Errorf("the approved command did not run: %v", err)
	}
}

func TestRPCApprovalDeniedByTheCaller(t *testing.T) {
	srv := bashSeqModel(t, "touch refused-file")
	ws := rpcWorkspace(t, srv.URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start","approvals":true}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"make a file"}`)
	ask := approvalOf(t, s.waitFor(t, `"type":"approval"`))
	s.send(`{"id":"3","method":"approve","request_id":"` + ask.RequestID + `","decision":"deny"}`)
	out := s.waitFor(t, `"id":"2"`)
	if !strings.Contains(out, `"action.denied"`) {
		t.Errorf("the refusal was not recorded:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "refused-file")); err == nil {
		t.Error("a refused command ran")
	}
}

// A remembered scope answers the next ask it covers without asking again.
func TestRPCApprovalRememberedScope(t *testing.T) {
	srv := bashSeqModel(t, "touch one", "touch two")
	ws := rpcWorkspace(t, srv.URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start","approvals":true}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"make files"}`)
	ask := approvalOf(t, s.waitFor(t, `"type":"approval"`))
	if ask.Offer == "" {
		t.Fatalf("policy offered no scope for %s", ask.Args)
	}
	s.send(`{"id":"3","method":"approve","request_id":"` + ask.RequestID + `","decision":"allow","remember":true}`)
	out := s.waitFor(t, `"id":"2"`)
	if n := strings.Count(out, `"type":"approval"`); n != 0 {
		t.Errorf("asked %d more times after the scope was remembered:\n%s", n, out)
	}
	if !strings.Contains(out, `"by":"session-scope"`) {
		t.Errorf("the second call was not recorded as allowed by the remembered scope:\n%s", out)
	}
	for _, f := range []string{"one", "two"} {
		if _, err := os.Stat(filepath.Join(ws, f)); err != nil {
			t.Errorf("%s was not made: %v", f, err)
		}
	}
}

// Input ending refuses an ask still open instead of leaving rpc waiting.
func TestRPCApprovalRefusedWhenInputEnds(t *testing.T) {
	srv := bashSeqModel(t, "touch never")
	ws := rpcWorkspace(t, srv.URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start","approvals":true}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"make a file"}`)
	s.waitFor(t, `"type":"approval"`)
	_ = s.in.Close()
	out := s.waitFor(t, `"id":"2"`)
	if !strings.Contains(out, `"action.denied"`) || !strings.Contains(out, "rpc input ended") {
		t.Errorf("the open ask was not refused for the input ending:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "never")); err == nil {
		t.Error("a command ran with no answer")
	}
}

// Without approvals on start, nothing is asked and the call is refused, as before.
func TestRPCWithoutApprovalsRefuses(t *testing.T) {
	srv := bashSeqModel(t, "touch nope")
	ws := rpcWorkspace(t, srv.URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start"}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"make a file"}`)
	out := s.waitFor(t, `"id":"2"`)
	if strings.Contains(out, `"type":"approval"`) || !strings.Contains(out, `"action.denied"`) {
		t.Errorf("a session without approvals asked, or did not refuse:\n%s", out)
	}
}

func TestRPCApproveNeedsAWaitingAsk(t *testing.T) {
	out := runRPC(t, t.TempDir(),
		`{"id":"1","method":"approve","request_id":"r-9","decision":"allow"}`,
		`{"id":"2","method":"approve","request_id":"r-9","decision":"maybe"}`,
		`{"id":"3","method":"approve","decision":"allow"}`,
		`{"id":"4","method":"quit"}`)
	for _, want := range []string{"no approval is waiting with request_id r-9", `"decision\": \"allow\" or \"deny\"`, `needs the ask's \"request_id\"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// An answer sent twice is taken once; the second is refused.
func TestRPCApprovalReplayIsRefused(t *testing.T) {
	srv := bashSeqModel(t, "touch once")
	ws := rpcWorkspace(t, srv.URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start","approvals":true}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"make a file"}`)
	ask := approvalOf(t, s.waitFor(t, `"type":"approval"`))
	s.send(`{"id":"3","method":"approve","request_id":"` + ask.RequestID + `","decision":"allow"}`)
	s.waitFor(t, `"id":"2"`)
	s.send(`{"id":"4","method":"approve","request_id":"` + ask.RequestID + `","decision":"allow"}`)
	if out := s.waitFor(t, `"id":"4"`); !strings.Contains(out, "no approval is waiting") {
		t.Errorf("a replayed answer was not refused:\n%s", out)
	}
}

// remember with nothing offered keeps nothing: a destructive command asks
// every time, and its arguments' hidden characters are flagged.
func TestRPCApprovalRememberWithoutOffer(t *testing.T) {
	srv := bashSeqModel(t, "rm -rf gone-one", "rm -rf gone-two\u202e")
	ws := rpcWorkspace(t, srv.URL, "")
	s := startRPC(t, ws)
	s.send(`{"id":"1","method":"start","approvals":true}`)
	s.waitFor(t, `"type":"ready"`)
	s.send(`{"id":"2","method":"prompt","prompt":"clean"}`)
	first := approvalOf(t, s.waitFor(t, `"type":"approval"`))
	if first.Offer != "" || !first.Destructive || first.ArgsHidden {
		t.Fatalf("first ask: %+v", first)
	}
	s.send(`{"id":"3","method":"approve","request_id":"` + first.RequestID + `","decision":"allow","remember":true}`)
	second := approvalOf(t, s.waitFor(t, `"type":"approval"`))
	if !second.ArgsHidden {
		t.Errorf("hidden characters in the arguments were not flagged: %s", second.Args)
	}
	s.send(`{"id":"4","method":"approve","request_id":"` + second.RequestID + `","decision":"deny"}`)
	if out := s.waitFor(t, `"id":"2"`); strings.Contains(out, "granted_scope") || strings.Contains(out, "session-scope") {
		t.Errorf("a scope was kept with none offered:\n%s", out)
	}
}

func TestRPCApprovalApproverIsBounded(t *testing.T) {
	long := strings.Repeat("a", rpcMaxApprover+1)
	out := runRPC(t, t.TempDir(),
		`{"id":"1","method":"approve","request_id":"r","decision":"allow","approver":"`+long+`"}`,
		`{"id":"2","method":"approve","request_id":"r","decision":"allow","approver":"olga\u202e"}`,
		`{"id":"3","method":"approve","request_id":"r","decision":"allow","approver":"a\u0007b"}`,
		`{"id":"4","method":"quit"}`)
	if strings.Count(out, "approver") < 3 || !strings.Contains(out, "longer than 256") {
		t.Errorf("an unbounded or hidden approver was taken:\n%s", out)
	}
}

// askDecision is an ask the default step makes for bash.
var askDecision = policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(touch *)", Reason: "needs approval"}

// A request whose record was withheld is refused without asking.
func TestRPCApproverRefusesAWithheldRequest(t *testing.T) {
	p := newRPCApprovals()
	asked := false
	approve := rpcApprover(p, func(any) { asked = true })
	ctx, ans := agent.ExpectAnswer(context.Background())
	ctx = agent.WithRequested(ctx, agent.Event{Payload: json.RawMessage(`{"withheld":"redaction failed"}`)})
	ok, err := approve(ctx, "bash", json.RawMessage(`{"command":"touch x"}`), askDecision)
	if ok || err != nil || asked || ans.By != agent.BySystem {
		t.Errorf("withheld request: ok=%v err=%v asked=%v answer=%+v", ok, err, asked, ans)
	}
}

// A stop while an ask waits ends it as interrupted; an answer that arrives
// with the stop is never applied.
func TestRPCApproverLosesToAStop(t *testing.T) {
	for i := 0; i < 200; i++ {
		p := newRPCApprovals()
		ctx, cancel := context.WithCancel(context.Background())
		var approve func(context.Context, string, json.RawMessage, policy.Result) (bool, error)
		approve = rpcApprover(p, func(v any) {
			// The answer and the stop are both ready before the approver waits.
			r := v.(rpcResponse)
			cancel()
			p.answer(r.Approval.RequestID, rpcAnswer{allow: true})
		})
		actx, ans := agent.ExpectAnswer(ctx)
		ok, err := approve(actx, "bash", json.RawMessage(`{"command":"touch x"}`), askDecision)
		if ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("round %d: an answer racing a stop was applied: ok=%v err=%v answer=%+v", i, ok, err, ans)
		}
	}
	p := newRPCApprovals()
	ctx, cancel := context.WithCancel(context.Background())
	approve := rpcApprover(p, func(any) { time.AfterFunc(10*time.Millisecond, cancel) })
	if ok, err := approve(ctx, "bash", json.RawMessage(`{}`), askDecision); ok || !errors.Is(err, context.Canceled) {
		t.Errorf("a stop while waiting: ok=%v err=%v", ok, err)
	}
}
