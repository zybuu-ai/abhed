package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func TestSurfaceAskerIsAQuestionNotAnApproval(t *testing.T) {
	q := tools.Question{Question: "Which database?", Options: []tools.Option{{Label: "sqlite"}, {Label: "postgres", Description: "the server"}}}
	for _, tc := range []struct {
		answer, want string
		err          bool
	}{{"o1", "postgres", false}, {"none", "none of the options", false}, {"", "", true}} {
		sf := &scriptSurface{answers: []string{tc.answer}}
		got, err := surfaceAsker{st: &cliState{surface: sf}}.AskPerson(context.Background(), q)
		if (err != nil) != tc.err || !strings.HasPrefix(got, tc.want) {
			t.Fatalf("answer %q: %q %v", tc.answer, got, err)
		}
		d := sf.asked[0]
		if d.Kind != ui.DialogChoice || d.Default != "" || !strings.Contains(d.Why, "not an approval") {
			t.Fatalf("dialog %+v", d)
		}
	}
}

// Only an interactive main conversation has the tool; subagents' registry
// and a headless run do not.
func TestAskOnlyAtATerminal(t *testing.T) {
	reg := tools.NewRegistry(tools.Read{})
	if _, ok := withAsk(reg, false).Get("ask_user"); ok {
		t.Fatal("a headless run can ask")
	}
	if _, ok := withAsk(reg, true).Get("ask_user"); !ok {
		t.Fatal("an interactive session cannot ask")
	}
	if _, ok := reg.Get("ask_user"); ok {
		t.Fatal("the subagents' registry gained ask_user")
	}
}

// End to end: with no one able to answer (piped input), the question is
// refused as unanswered, never answered for the person.
func TestCLIAskUnansweredIsNeverAnswered(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"question": "Which?", "options": []map[string]string{{"label": "A"}, {"label": "B"}}})
	call, _ := json.Marshal(string(args))
	c := startCLIWith(t, func(w io.Writer, n int, _ string) {
		if n == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"ask_user","arguments":`+string(call)+`}}]}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"ok"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	conv := c.task("pick one")
	if !strings.Contains(conv, "did not answer") || strings.Contains(conv, "The person chose") {
		t.Fatalf("the question was answered for the person: %s", conv)
	}
}
