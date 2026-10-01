# Structured output

A program embedding Abhed usually does not want prose. It wants a value: a
verdict, a list of findings, a record it can store. `RunJSON` runs a prompt
whose answer must match a JSON Schema, and decodes it into a Go value.

```go
type Review struct {
    Verdict    string   `json:"verdict"`
    Confidence float64  `json:"confidence"`
    Notes      []string `json:"notes"`
}

schema := json.RawMessage(`{
  "type": "object",
  "required": ["verdict", "confidence"],
  "additionalProperties": false,
  "properties": {
    "verdict":    {"type": "string", "enum": ["safe", "unsafe"]},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "notes":      {"type": "array", "items": {"type": "string"}}
  }
}`)

var out Review
err := a.RunJSON(ctx, "Review pkg/auth for injection risks.", schema, &out)
```

`RunStructured` is the same call returning the validated JSON as sent, for a
caller that would rather decode it themselves.

## How it works, and why it works on every provider

Some providers have a native "answer in this shape" mode. Most do not, and the
ones that do disagree with each other. What every one of the twenty providers
can do is call a tool.

So the answer is delivered by calling a tool named `result` whose input schema
**is** your schema. The harness validates the call. If it matches, the run ends
— completed — and you get the value. If it does not, the model is told what is
wrong, path by path:

```
The result does not match the schema:
- $.verdict: must be one of "safe", "unsafe"
- $.confidence: above maximum 1

Fix the fields named above and call result again with the complete object.
```

and it fixes its own output on the next turn. Five rejected attempts end the
run; a model that cannot satisfy the schema in five is not going to on the
sixth.

Validation happens **inside** the loop, not after it. That is the whole
difference from parsing the model's prose: the model gets to correct the
answer, and the caller never sees one that failed validation. Stored secrets
are then redacted from the answer, so an answer holding one may no longer match
the schema, or come back as `{"withheld": ...}`; see [the SDK](09-sdk.md).

## What the schema can say

Types (`object`, `array`, `string`, `number`, `integer`, `boolean`, `null`,
and lists of them), `required`, `additionalProperties`, `items`, `enum`,
`const`, `minimum`/`maximum` and their exclusive forms, `minLength`/`maxLength`,
`pattern`, `minItems`/`maxItems`/`uniqueItems`, `anyOf`/`oneOf`/`allOf`/`not`,
`nullable`, and `$ref` into `definitions` or `$defs`.

A keyword the validator would not enforce is **refused when the schema is
compiled**, before a turn is spent. A constraint that is silently skipped is a
contract that is not, and this is the one place a quiet default would be a
lie to the caller.

## Failure modes, named

| What happened | What you get |
|---|---|
| The model answered in prose and never called `result` | `ErrNoResult`, carrying its last message |
| Five attempts all failed validation | `ErrNoResult` with reason `completed` — the run ended on the last rejection |
| The run hit its turn or token limit first | an error naming the terminal reason |
| Your schema uses a keyword the validator does not support | an error before anything runs |
| The JSON matched the schema but not your Go type | an error saying so — the schema and the struct disagree, which is a bug in the caller |

The transcript is recorded as always: every rejected attempt is an observation
in the event log, so "why did it take four tries" is answerable afterwards.
