package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fixedAsker struct {
	answer string
	err    error
	asked  []Question
}

func (f *fixedAsker) AskPerson(_ context.Context, q Question) (string, error) {
	f.asked = append(f.asked, q)
	return f.answer, f.err
}

func askArgs(q string, labels ...string) json.RawMessage {
	var opts []Option
	for _, l := range labels {
		opts = append(opts, Option{Label: l})
	}
	b, _ := json.Marshal(Question{Question: q, Options: opts})
	return b
}

func TestAskPutsTheQuestionToThePerson(t *testing.T) {
	a := NewAsk()
	if a.Mutates() || a.Name() != "ask_user" {
		t.Fatal("ask_user changes nothing")
	}
	// With no one to ask, it says so; it never answers for the person.
	if r := a.Run(context.Background(), nil, askArgs("Which?", "A", "B")); r.IsError || !strings.Contains(r.Content, "No one is at the prompt") {
		t.Fatalf("no asker: %+v", r)
	}
	f := &fixedAsker{answer: "B"}
	a.SetAsker(f)
	if r := a.Run(context.Background(), nil, askArgs("Which?", "A", "B")); r.Content != "The person chose: B" || len(f.asked) != 1 {
		t.Fatalf("answered: %+v", r)
	}
	f.err = ErrNotAnswered
	if r := a.Run(context.Background(), nil, askArgs("Which?", "A", "B")); !strings.Contains(r.Content, "did not answer") {
		t.Fatalf("unanswered: %+v", r)
	}
	f.err = errors.New("broken")
	if r := a.Run(context.Background(), nil, askArgs("Which?", "A", "B")); !r.IsError {
		t.Fatalf("failure: %+v", r)
	}
}

func TestAskValidates(t *testing.T) {
	a := NewAsk()
	f := &fixedAsker{answer: "A"}
	a.SetAsker(f)
	for _, bad := range []json.RawMessage{
		askArgs("", "A", "B"),
		askArgs("Which?", "A"),
		askArgs("Which?", "A", "B", "C", "D", "E", "F", "G"),
		askArgs("Which?", "A", "a"),
		askArgs("Which?", "A", " "),
		askArgs(strings.Repeat("q", 501), "A", "B"),
		json.RawMessage(`{"question":"Which?","options":[{"label":"A","description":"` + strings.Repeat("d", 301) + `"},{"label":"B"}]}`),
	} {
		if r := a.Run(context.Background(), nil, bad); !r.IsError {
			t.Errorf("accepted %s", bad)
		}
	}
	if len(f.asked) != 0 {
		t.Fatal("an invalid question was asked")
	}
}
