package bgcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/bgjob"
	"github.com/zybuu-ai/abhed/profiles"
)

const token = "tok-bg-3c1f9a"

// fakeServer serves the background-job protocol for one job.
type fakeServer struct {
	mu       sync.Mutex
	job      bgjob.Job
	started  []bgjob.StartRequest
	answers  []map[string]any
	canceled int
	requests int
	// noBG answers every /v1/bg path as a server without jobs does.
	noBG bool
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, `{"error":"sign in"}`, http.StatusUnauthorized)
		return
	}
	if f.noBG && strings.HasPrefix(r.URL.Path, "/v1/bg/") {
		http.NotFound(w, r)
		return
	}
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "POST" && r.URL.Path == bgjob.PathJobs:
		var req bgjob.StartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.started = append(f.started, req)
		f.job.Prompt = req.Prompt
		w.WriteHeader(http.StatusAccepted)
		reply(f.job)
	case r.Method == "GET" && r.URL.Path == bgjob.PathJobs:
		reply(bgjob.JobList{Jobs: []bgjob.Job{f.job}})
	case r.Method == "GET" && r.URL.Path == bgjob.JobPath(f.job.ID):
		reply(f.job)
	case r.Method == "GET" && r.URL.Path == bgjob.JobPath("nope"):
		http.Error(w, `{"error":"no such job"}`, http.StatusNotFound)
	case r.Method == "POST" && r.URL.Path == bgjob.CancelPath(f.job.ID):
		f.canceled++
		f.job.State = bgjob.StateCancelled
		reply(f.job)
	case r.Method == "POST" && r.URL.Path == "/v1/sessions/"+f.job.SessionID+"/approve":
		var a map[string]any
		_ = json.NewDecoder(r.Body).Decode(&a)
		f.answers = append(f.answers, a)
		f.job.Waiting = nil
		f.job.State = bgjob.StateDone
		reply(map[string]bool{"ok": true})
	case r.Method == "GET" && r.URL.Path == "/v1/sessions/"+f.job.SessionID+"/replay":
		reply([]bgjob.Event{{Seq: 1, Type: "user.message", Payload: json.RawMessage(`{"text":"hi \u001b[31mred"}`), CreatedAt: time.Now()}})
	case r.Method == "GET" && r.URL.Path == "/v1/sessions/"+f.job.SessionID+"/changes":
		reply(map[string]any{"available": true, "files": []map[string]any{{"path": "a.txt", "diff": "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-x\n+y\n"}}})
	default:
		http.NotFound(w, r)
	}
}

type rig struct {
	srv      *fakeServer
	profiles string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := &fakeServer{job: bgjob.Job{ID: "bg_1", Name: "fix \x1b]0;pwned\x07 it", Owner: "alice", State: bgjob.StateRunning,
		SessionID: "sess_1", Created: time.Now()}}
	hs := httptest.NewServer(f)
	t.Cleanup(hs.Close)
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := profiles.Save(path, &profiles.File{Version: 1, Current: "alice", Profiles: map[string]*profiles.Profile{
		"alice": {URL: hs.URL, Token: token, TokenType: "Bearer"}}}); err != nil {
		t.Fatal(err)
	}
	return &rig{srv: f, profiles: path}
}

type result struct {
	code     int
	out, err string
}

func (r *rig) run(in string, interactive bool, inAgent string, args ...string) result {
	var out, errOut bytes.Buffer
	env := Env{Ctx: context.Background(), ProfilesPath: r.profiles, In: strings.NewReader(in), Out: &out, Err: &errOut,
		Interactive: interactive, Poll: 5 * time.Millisecond, InAgent: func() string { return inAgent }}
	code := Main(env, args)
	return result{code, out.String(), errOut.String()}
}

func TestStartSendsThePromptAndNoIdentity(t *testing.T) {
	r := newRig(t)
	res := r.run("", false, "", "start", "-mode", "plan", "-name", "n", "fix", "the", "tests")
	if res.code != ExitOK || !strings.Contains(res.out, "started bg_1") || !strings.Contains(res.out, "abhed bg attach bg_1") {
		t.Fatalf("start: %+v", res)
	}
	got := r.srv.started[0]
	if got.Prompt != "fix the tests" || got.Mode != "plan" || got.Name != "n" {
		t.Fatalf("sent %+v", got)
	}
	if res := r.run("", false, "", "start"); res.code != ExitUsage {
		t.Fatalf("a start without a prompt: %+v", res)
	}
}

// An agent's command must not start a job, which runs as the person with
// their secrets, nor answer one's asks: nothing reaches the server.
func TestRefusedInsideAnAgentsCommand(t *testing.T) {
	r := newRig(t)
	for _, args := range [][]string{{"start", "x"}, {"attach", "bg_1"}, {"list"}, {"cancel", "bg_1"}} {
		if res := r.run("y\n", true, "ABHED_SANDBOX is set", args...); res.code != ExitAuth || !strings.Contains(res.err, "refused inside an agent's command") {
			t.Fatalf("%v inside an agent: %+v", args, res)
		}
	}
	if r.srv.requests != 0 {
		t.Fatalf("%d requests reached the server", r.srv.requests)
	}
}

// What the server sends is shown with its hidden characters escaped, so a
// job's name or record cannot drive the person's terminal.
func TestListAndLogsEscapeWhatTheServerSends(t *testing.T) {
	r := newRig(t)
	r.srv.job.Waiting = &bgjob.Approval{JobID: "bg_1", SessionID: "sess_1", RequestID: "req_1", Tool: "bash"}
	res := r.run("", false, "", "list")
	if res.code != ExitOK || !strings.Contains(res.out, "running (bash)") || strings.ContainsAny(res.out, "\x1b\x07") {
		t.Fatalf("list: %q", res.out)
	}
	res = r.run("", false, "", "logs", "bg_1")
	if res.code != ExitOK || !strings.Contains(res.out, "user.message") || strings.Contains(res.out, "\x1b") {
		t.Fatalf("logs: %q", res.out)
	}
	res = r.run("", false, "", "logs", "-patch", "bg_1")
	if res.code != ExitOK || !strings.Contains(res.out, "+++ b/a.txt\n@@ -1 +1 @@\n-x\n+y\n") {
		t.Fatalf("logs -patch: %q", res.out)
	}
	if res := r.run("", false, "", "logs", "nope"); res.code != ExitNotFound {
		t.Fatalf("an unknown job: %+v", res)
	}
}

// attach puts the ask to the person and answers it on the job's session by
// its request id; only y allows.
func TestAttachAnswersTheAskOnTheSession(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{{"y\n", true}, {"\n", false}, {"yes please\n", false}} {
		r := newRig(t)
		r.srv.job.Waiting = &bgjob.Approval{JobID: "bg_1", SessionID: "sess_1", RequestID: "req_9", Tool: "bash",
			Reason: "runs a command", Deadline: time.Now().Add(time.Hour)}
		res := r.run(tc.in, true, "", "attach", "bg_1")
		if res.code != ExitOK || !strings.Contains(res.err, "The job asks: bash") {
			t.Fatalf("%q: attach: %+v", tc.in, res)
		}
		if len(r.srv.answers) != 1 || r.srv.answers[0]["request_id"] != "req_9" || r.srv.answers[0]["approved"] != tc.want {
			t.Fatalf("%q: answered %v", tc.in, r.srv.answers)
		}
	}
}

// Without a terminal attach answers nothing: it says what waits, and the
// server's timeout decides.
func TestAttachWithoutATerminalAnswersNothing(t *testing.T) {
	r := newRig(t)
	r.srv.job.Waiting = &bgjob.Approval{JobID: "bg_1", SessionID: "sess_1", RequestID: "req_9", Tool: "bash", Deadline: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	code := Main(Env{Ctx: ctx, ProfilesPath: r.profiles, In: strings.NewReader("y\n"), Out: &out, Err: &errOut, Poll: 5 * time.Millisecond}, []string{"attach", "bg_1"})
	if code != ExitOK || !strings.Contains(errOut.String(), "the job waits on you: bash") || !strings.Contains(errOut.String(), "runs on") {
		t.Fatalf("attach: %d %q", code, errOut.String())
	}
	if len(r.srv.answers) != 0 {
		t.Fatalf("answered without a person: %v", r.srv.answers)
	}
}

func TestCancelAndAJobThatDidNotFinishDone(t *testing.T) {
	r := newRig(t)
	if res := r.run("", false, "", "cancel", "bg_1"); res.code != ExitOK || !strings.Contains(res.out, "cancelled") || r.srv.canceled != 1 {
		t.Fatalf("cancel: %+v", res)
	}
	if res := r.run("", false, "", "attach", "bg_1"); res.code != ExitFailed || !strings.Contains(res.err, "cancelled") {
		t.Fatalf("attach to a cancelled job: %+v", res)
	}
}

func TestAServerWithoutJobsSaysSo(t *testing.T) {
	r := newRig(t)
	r.srv.noBG = true
	if res := r.run("", false, "", "list"); res.code != ExitNotFound || !strings.Contains(res.err, "runs no background jobs") {
		t.Fatalf("list: %+v", res)
	}
}
