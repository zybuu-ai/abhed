package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

func costHarness(t *testing.T, name string, turns []scriptedTurn) (*Loop, *MemStore) {
	t.Helper()
	l, store, _ := harness(t, turns, policy.ModeBypass, true)
	l.Adapter = namedAdapter{scriptedAdapter: l.Adapter.(*scriptedAdapter), name: name, window: 100000}
	l.Budget = NewBudget(0, 0, false)
	return l, store
}

func modelCalls(t *testing.T, store *MemStore) []ModelCall {
	t.Helper()
	evs, _ := store.Events("sess1")
	var out []ModelCall
	for _, e := range evs {
		if e.Type == EvModelCall {
			var c ModelCall
			_ = json.Unmarshal(e.Payload, &c)
			out = append(out, c)
		}
	}
	return out
}

func budgetStops(t *testing.T, store *MemStore) []BudgetStopped {
	t.Helper()
	evs, _ := store.Events("sess1")
	var out []BudgetStopped
	for _, e := range evs {
		if e.Type == EvBudgetStopped {
			var b BudgetStopped
			_ = json.Unmarshal(e.Payload, &b)
			out = append(out, b)
		}
	}
	return out
}

var turnUsage = &model.Usage{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 200}

// Each call to a priced model records its cost, and the session's end and
// usage carry the total.
func TestCostIsRecordedPerCallAndSession(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: turnUsage},
		{text: "done", usage: turnUsage},
	})
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	per := (600*2.50 + 400*1.25 + 200*10.00) / 1e6
	calls := modelCalls(t, store)
	if len(calls) != 2 {
		t.Fatalf("%d model calls", len(calls))
	}
	for _, c := range calls {
		if c.CostUSD == nil || !near(*c.CostUSD, per) {
			t.Fatalf("call cost %v, want %v", c.CostUSD, per)
		}
	}
	if u := l.Usage(); !near(u.CostUSD, 2*per) || u.Unpriced {
		t.Fatalf("usage %+v", u)
	}
	end, ok := LastEnd(sessEvents(t, store))
	if !ok || !near(end.CostUSD, 2*per) || end.Unpriced {
		t.Fatalf("end %+v", end)
	}
	if !near(l.Budget.SpentUSD(), 2*per) {
		t.Fatalf("budget spent %v", l.Budget.SpentUSD())
	}
}

// A model with no price records no cost and marks the session unpriced.
func TestUnpricedModelIsNotGuessed(t *testing.T) {
	l, store := costHarness(t, "gemma4:26b", []scriptedTurn{{text: "done", usage: turnUsage}})
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if c := modelCalls(t, store); len(c) != 1 || c[0].CostUSD != nil {
		t.Fatalf("calls %+v", c)
	}
	if u := l.Usage(); u.CostUSD != 0 || !u.Unpriced {
		t.Fatalf("usage %+v", u)
	}
}

// A configured price prices a model the list does not have.
func TestConfiguredPrice(t *testing.T) {
	l, store := costHarness(t, "gemma4:26b", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.Budget.Prices = map[string]model.Price{"gemma4:26b": {Input: 1, Output: 1}}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if c := modelCalls(t, store); len(c) != 1 || c[0].CostUSD == nil || !near(*c[0].CostUSD, 1200.0/1e6) {
		t.Fatalf("calls %+v", c)
	}
}

// Past the dollar cap the run stops cleanly at the next turn, with exit 3,
// a recorded reason and a message.
func TestDollarBudgetStopsTheRun(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: &model.Usage{InputTokens: 100000, OutputTokens: 1000}},
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: turnUsage},
		{text: "done", usage: turnUsage},
	})
	l.Budget.MaxUSD = 0.10 // the first call costs $0.26
	reason, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if reason != TermMaxBudget || reason.ExitCode() != 3 {
		t.Fatalf("reason %s", reason)
	}
	if n := len(modelCalls(t, store)); n != 1 {
		t.Fatalf("%d calls made past the cap", n)
	}
	stops := budgetStops(t, store)
	if len(stops) != 1 || stops[0].Reason != "usd" || stops[0].MaxUSD != 0.10 || stops[0].Message == "" {
		t.Fatalf("stops %+v", stops)
	}
}

// A dollar cap with a model that has no price fails closed: no call is made.
func TestDollarBudgetRefusesAnUnpricedModel(t *testing.T) {
	l, store := costHarness(t, "gemma4:26b", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.Budget.MaxUSD = 5
	reason, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if reason != TermMaxBudget {
		t.Fatalf("reason %s", reason)
	}
	if n := len(modelCalls(t, store)); n != 0 {
		t.Fatalf("%d calls made to an unpriced model under a dollar cap", n)
	}
	if stops := budgetStops(t, store); len(stops) != 1 || stops[0].Reason != "unpriced" || stops[0].Model != "gemma4:26b" {
		t.Fatalf("stops %+v", stops)
	}
}

// A subagent's spawn is refused once the dollar cap is reached, and a
// continued session starts from the dollars its record says it spent.
func TestDollarBudgetSharedAndCarried(t *testing.T) {
	b := NewBudget(0, 0, false)
	b.MaxUSD = 1
	b.SpendUSD(1.5)
	if err := b.TryReserveSubagent(); err == nil {
		t.Fatal("a spawn past the dollar cap must be refused")
	}
	c := NewBudget(0, 0, false)
	c.Carry(0, 0, 0.75)
	if !near(c.SpentUSD(), 0.75) {
		t.Fatalf("carried %v", c.SpentUSD())
	}
	st := NewMemStore()
	p := NewRecorder(st, "p", "")
	_, _ = p.Record(EvSubagentReturn, ActorAgent, Trusted, map[string]any{"session": "a", "tokens_in": 1, "cost_usd": 0.25})
	_, _ = p.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted, CostUSD: 0.5})
	evs, _ := st.Events("p")
	if _, _, usd := CarriedSpend(evs); !near(usd, 0.75) {
		t.Fatalf("carried spend %v", usd)
	}
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

func sessEvents(t *testing.T, s *MemStore) []Event {
	t.Helper()
	evs, err := s.Events("sess1")
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A resumed session goes on from the dollars its record says it spent, so
// each resume carries more, never less, and an unpriced call stays known.
func TestCostCarriedAcrossResumes(t *testing.T) {
	per := (600*2.50 + 400*1.25 + 200*10.00) / 1e6
	var carried SessionEnded
	var spent float64
	for i := 1; i <= 3; i++ {
		l, store := costHarness(t, "gpt-4o", []scriptedTurn{{text: "done", usage: turnUsage}})
		if i > 1 {
			l.CarryUsage(carried)
			l.Budget.Carry(0, 0, carried.CostUSD)
		}
		if _, err := l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		end, _ := LastEnd(sessEvents(t, store))
		if !near(end.CostUSD, float64(i)*per) || end.CostUSD < spent {
			t.Fatalf("resume %d: session cost %v, want %v", i, end.CostUSD, float64(i)*per)
		}
		if _, _, usd := CarriedSpend(sessEvents(t, store)); !near(usd, float64(i)*per) {
			t.Fatalf("resume %d: carried %v", i, usd)
		}
		spent, carried = end.CostUSD, end
	}
	l, store := costHarness(t, "gpt-4o", []scriptedTurn{{text: "done", usage: turnUsage}})
	l.CarryUsage(SessionEnded{CostUSD: 1, Unpriced: true})
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if end, _ := LastEnd(sessEvents(t, store)); !end.Unpriced || !near(end.CostUSD, 1+per) {
		t.Fatalf("an unpriced earlier run was forgotten: %+v", end)
	}
}

// callsIn is every model.call recorded in session sid.
func callsIn(t *testing.T, store *MemStore, sid string) []ModelCall {
	t.Helper()
	evs, _ := store.Events(sid)
	var out []ModelCall
	for _, e := range evs {
		if e.Type == EvModelCall {
			var c ModelCall
			_ = json.Unmarshal(e.Payload, &c)
			out = append(out, c)
		}
	}
	return out
}

// renamed is an adapter answering to another model name.
type renamed struct {
	model.Adapter
	name string
}

func (r renamed) Profile() model.Profile {
	p := r.Adapter.Profile()
	p.Name = r.name
	return p
}

func scripted(name string, turns ...scriptedTurn) namedAdapter {
	return namedAdapter{scriptedAdapter: &scriptedAdapter{turns: turns}, name: name, window: 100000}
}

func capped(usd float64) *Budget {
	b := NewBudget(0, 0, false)
	b.MaxUSD = usd
	return b
}

func criticClaims(t *testing.T) ([]ClaimVerdict, *recordIndex) {
	t.Helper()
	b := newRec(t)
	b.call("c1", "read", map[string]string{"path": "/w/app.yaml"}, "replicas: 3\n", nil)
	return checkClaims(checkEnv{events: b.events()}, textClaims("There are 3 replicas."))
}

// The critic's call is charged to the budget and priced, so a session that
// used it is not shown as unpriced; under a cap an unpriced critic is not asked.
func TestCriticIsChargedAndGated(t *testing.T) {
	l, store, _, _ := verifyHarness(t, VerifyLabel, false, nil)
	l.Budget = NewBudget(0, 0, false)
	critic := scripted("gpt-4o-mini", scriptedTurn{text: `{"verdicts":[{"n":1,"verdict":"supported"}]}`, usage: turnUsage})
	verdicts, ix := criticClaims(t)
	l.critique(context.Background(), &Critic{Adapter: critic}, verdicts, ix)
	calls := callsIn(t, store, "sess1")
	if len(calls) != 1 || calls[0].Purpose != PurposeCritic || calls[0].CostUSD == nil || *calls[0].CostUSD <= 0 {
		t.Fatalf("critic call %+v", calls)
	}
	if u := l.Usage(); u.Unpriced || !near(u.CostUSD, *calls[0].CostUSD) || !near(l.Budget.SpentUSD(), u.CostUSD) {
		t.Fatalf("usage %+v spent %v", u, l.Budget.SpentUSD())
	}

	l2, _, _, _ := verifyHarness(t, VerifyLabel, false, nil)
	l2.Budget = capped(1)
	unpriced := scripted("no-price-model", scriptedTurn{text: `{"verdicts":[{"n":1,"verdict":"supported"}]}`})
	verdicts, ix = criticClaims(t)
	if got := l2.critique(context.Background(), &Critic{Adapter: unpriced}, verdicts, ix); got != CriticUnavailable {
		t.Fatalf("critic %q", got)
	}
	if len(unpriced.gotRequests) != 0 || verdicts[0].Verdict != ClaimUnverified {
		t.Fatalf("asked %d times, verdict %s", len(unpriced.gotRequests), verdicts[0].Verdict)
	}
}

// A suggestion model with no price is not asked under a cap; a priced one is
// charged.
func TestSuggestionIsChargedAndGated(t *testing.T) {
	for _, priced := range []bool{false, true} {
		stub := &suggestStub{replies: []stubReply{{text: "done"}, {text: "Add a README"}}}
		l, store := suggestLoop(t, stub)
		l.Budget = capped(1)
		l.Budget.Prices = map[string]model.Price{"stub-model": {Input: 1, Output: 1}}
		name := "no-price-model"
		if priced {
			name = "gpt-4o-mini"
		}
		l.Suggest.Adapter = renamed{Adapter: stub, name: name}
		if reason, err := l.Run(context.Background(), "go"); err != nil || reason != TermCompleted {
			t.Fatalf("run %s %v", reason, err)
		}
		l.WaitSuggestion(context.Background())
		_, calls := suggestions(t, store)
		if !priced {
			if len(stub.requests()) != 1 || len(calls) != 0 {
				t.Fatalf("an unpriced suggestion model was asked: %d requests, %+v", len(stub.requests()), calls)
			}
			continue
		}
		if len(calls) != 1 || calls[0].CostUSD == nil || l.Usage().Unpriced {
			t.Fatalf("suggestion call %+v usage %+v", calls, l.Usage())
		}
	}
}

func compactLoop(t *testing.T, summarizer model.Adapter, b *Budget) (*Loop, *MemStore) {
	t.Helper()
	l, store, _ := pairLoop(t)
	l.Compactor.Summarizer = nil
	l.Compactor.Adapter = summarizer
	l.Budget = b
	l.messages = append([]model.Message{{Role: model.RoleUser, Content: "first"},
		{Role: model.RoleAssistant, Content: "reply"}}, summaryThenFourExchanges()[1:]...)
	return l, store
}

// A compaction's summary is charged; under a cap an unpriced summarizer is not
// asked and the history is left alone.
func TestCompactionIsChargedAndGated(t *testing.T) {
	sum := scripted("gpt-4o", scriptedTurn{text: "summary", usage: turnUsage})
	l, store := compactLoop(t, sum, NewBudget(0, 0, false))
	if _, err := l.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := callsIn(t, store, "pair")
	if len(calls) != 1 || calls[0].Purpose != PurposeCompaction || calls[0].CostUSD == nil || calls[0].TokensIn != 1000 {
		t.Fatalf("compaction call %+v", calls)
	}
	if !near(l.Budget.SpentUSD(), *calls[0].CostUSD) || l.Budget.Spent() != 1200 {
		t.Fatalf("budget $%v %d tokens", l.Budget.SpentUSD(), l.Budget.Spent())
	}

	none := scripted("no-price-model", scriptedTurn{text: "summary"})
	l2, _ := compactLoop(t, none, capped(1))
	before := len(l2.messages)
	if _, err := l2.Compact(context.Background()); err == nil || !strings.Contains(err.Error(), "has no price") {
		t.Fatalf("err %v", err)
	}
	if len(none.gotRequests) != 0 || len(l2.messages) != before {
		t.Fatalf("asked %d times, history %d→%d", len(none.gotRequests), before, len(l2.messages))
	}
}

// A pipeline's model step is charged to the calling loop; under a cap an
// unpriced step model is not asked.
func TestPipelineStepIsChargedAndGated(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", nil)
	s := &Steps{loop: l}
	req := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "classify"}}}
	if _, err := s.Complete(context.Background(), scripted("gpt-4o-mini", scriptedTurn{text: "x", usage: turnUsage}), req); err != nil {
		t.Fatal(err)
	}
	calls := modelCalls(t, store)
	if len(calls) != 1 || calls[0].Purpose != PurposePipeline || calls[0].CostUSD == nil || !near(l.Budget.SpentUSD(), *calls[0].CostUSD) {
		t.Fatalf("pipeline call %+v", calls)
	}
	l.Budget.MaxUSD = 1
	none := scripted("no-price-model", scriptedTurn{text: "x"})
	if _, err := s.Complete(context.Background(), none, req); err == nil || len(none.gotRequests) != 0 {
		t.Fatalf("an unpriced step model was asked: %v, %d", err, len(none.gotRequests))
	}
}

// unmetered is a context for compacting with no loop, which only tests do.
func unmetered() context.Context {
	return context.WithValue(context.Background(), unmeteredKey{}, true)
}

// A compaction outside a loop has no budget to charge, so its summary call is refused.
func TestCompactionWithNoLoopIsRefused(t *testing.T) {
	sum := scripted("gpt-4o", scriptedTurn{text: "summary"})
	c := NewCompactor(sum, 0.01)
	if _, _, err := c.Compact(context.Background(), "manual", "", longMessages(20, 10), 0); !errors.Is(err, errUnmetered) {
		t.Fatalf("err %v", err)
	}
	if len(sum.gotRequests) != 0 {
		t.Fatalf("asked %d times", len(sum.gotRequests))
	}
}

// cutAdapter streams some text, then holds until the call is cancelled, and
// never reports usage, as a stream cut mid-way does.
type cutAdapter struct {
	name    string
	started chan struct{}
}

func (c *cutAdapter) Name() string { return c.name }
func (c *cutAdapter) Profile() model.Profile {
	return model.Profile{Name: c.name, ContextWindow: 100000}
}
func (c *cutAdapter) CountTokens(model.Request) (int, error) { return 500, nil }
func (c *cutAdapter) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	go func() {
		defer close(ch)
		ch <- model.Chunk{Type: model.ChunkText, Text: strings.Repeat("x", 400)}
		close(c.started)
		<-ctx.Done()
		ch <- model.Chunk{Type: model.ChunkError, Err: ctx.Err()}
	}()
	return ch, nil
}

// A side call cancelled mid-stream reports no usage, yet cost tokens: they
// are estimated as a turn's are, marked so, and charged.
func TestCancelledSideCallIsEstimatedAndCharged(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", nil)
	s := &Steps{loop: l}
	cut := &cutAdapter{name: "gpt-4o-mini", started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-cut.started; cancel() }()
	if _, err := s.Complete(ctx, cut, model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}}); err == nil {
		t.Fatal("a cancelled call returned no error")
	}
	calls := modelCalls(t, store)
	if len(calls) != 1 || !calls[0].Estimated || calls[0].TokensIn != 500 || calls[0].TokensOut != 100 || calls[0].CostUSD == nil {
		t.Fatalf("cancelled call %+v", calls)
	}
	want := (500*0.15 + 100*0.60) / 1e6
	if !near(*calls[0].CostUSD, want) || !near(l.Budget.SpentUSD(), want) || l.Budget.Spent() != 600 {
		t.Fatalf("charged $%v of $%v, %d tokens", l.Budget.SpentUSD(), want, l.Budget.Spent())
	}
}

// A suggestion cancelled by the next message is estimated and charged too.
func TestCancelledSuggestionIsCharged(t *testing.T) {
	stub := &suggestStub{replies: []stubReply{{text: "done"}, {text: "again"}}}
	l, store := suggestLoop(t, stub)
	l.Budget = NewBudget(0, 0, false)
	l.Budget.Prices = map[string]model.Price{"stub-model": {Input: 0, Output: 0}}
	cut := &cutAdapter{name: "gpt-4o-mini", started: make(chan struct{})}
	l.Suggest.Adapter = cut
	l.Suggest.Timeout = time.Minute
	if _, err := l.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	<-cut.started
	l.Suggest = nil
	if _, err := l.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	_, calls := suggestions(t, store)
	if len(calls) != 1 || !calls[0].Estimated || calls[0].CostUSD == nil || *calls[0].CostUSD <= 0 {
		t.Fatalf("cancelled suggestion %+v", calls)
	}
	if !near(l.Budget.SpentUSD(), *calls[0].CostUSD) {
		t.Fatalf("budget $%v", l.Budget.SpentUSD())
	}
}

// counted is a named adapter whose requests count tokens, as a real one's do.
type counted struct {
	namedAdapter
	n int
}

func (c counted) CountTokens(model.Request) (int, error) { return c.n, nil }

// A provider that reports no usage does not make calls free: a call with no
// prompt tokens reported is estimated and charged, so a cap still stops the run.
func TestUnreportedUsageIsEstimatedUnderACap(t *testing.T) {
	none := &model.Usage{}
	turns := []scriptedTurn{}
	for range 4 {
		turns = append(turns, scriptedTurn{calls: []model.ToolCall{call("bash", map[string]string{"command": "true"})}, usage: none})
	}
	l, store := costHarness(t, "gpt-4o", turns)
	l.Adapter = counted{namedAdapter: l.Adapter.(namedAdapter), n: 2000}
	l.Budget.MaxUSD = 0.000001
	reason, err := l.Run(context.Background(), "go")
	if err != nil || reason != TermMaxBudget {
		t.Fatalf("run %s %v", reason, err)
	}
	calls := modelCalls(t, store)
	if len(calls) != 1 || !calls[0].Estimated || calls[0].TokensIn != 2000 || calls[0].CostUSD == nil || *calls[0].CostUSD <= 0 {
		t.Fatalf("calls %+v", calls)
	}
}

// Under a dollar cap, a call cut before usage came back is charged its whole
// output bound: cutting a call does not stop its bill.
func TestCutCallUnderACapIsChargedAtItsBound(t *testing.T) {
	l, store := costHarness(t, "gpt-4o", nil)
	l.Budget.MaxUSD = 10
	s := &Steps{loop: l}
	cut := &cutAdapter{name: "gpt-4o-mini", started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-cut.started; cancel() }()
	_, _ = s.Complete(ctx, cut, model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}, MaxTokens: 2048})
	calls := modelCalls(t, store)
	if len(calls) != 1 || !calls[0].Estimated || calls[0].TokensOut != 2048 {
		t.Fatalf("cut call %+v", calls)
	}
}
