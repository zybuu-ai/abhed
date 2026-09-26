# Abhed — Tool Contracts

Status: Draft · 2026-09-02
**Evidence status: [E] engineering judgment**, informed by verified finding P2 (tool surface
discipline: 15 tools → 2 moved success 80% → 100%, vendor-reported, medium confidence).

This is the most important document in the repo. **Tools are where agents actually fail** —
not in the loop, not in the model. A tool that returns an unhelpful error teaches the model
nothing and burns a turn; a tool with ambiguous semantics produces silent corruption.

## 0. Rules that apply to every tool

1. **Errors are instructions.** An error message is read by the model, not a human. It must
   say what failed, why, and what to do instead. `"File not found"` is a wasted turn;
   `"File not found: src/auth.ts. Did you mean src/auth/index.ts? Use glob to list."` is a
   recovered one.
2. **No silent success.** A tool that "worked" but changed nothing must say so. The most
   expensive failure mode is an edit that no-ops while reporting success — the model
   proceeds on a false premise and the error surfaces many turns later.
3. **Deterministic output shape.** Same input → same output format, always. The model learns
   the shape; violating it costs accuracy.
4. **Bounded output.** Every tool caps its response and says when it truncated. Unbounded
   output is a context-budget attack on yourself (P2).
5. **Absolute paths in, absolute paths out.** Relative paths are ambiguous across turns after
   a `cd`. Normalize at the boundary; reject relative paths with a message showing the
   absolute form.
6. **Idempotent where possible.** Re-running a tool after an ambiguous failure must be safe.
7. **Every call is an event** (P6): `ActionEvent` in, `ObservationEvent` out, both persisted.

## 1. Tool surface

Nine native tools. Everything else arrives via the reviewed MCP gateway (§03-security).
This set is deliberately small — every tool is an attack surface and a decision the model
can get wrong.

| Tool | Purpose | Mutates | Approval |
|---|---|---|---|
| `read` | Read file content | no | none |
| `write` | Create/overwrite file | **yes** | ask (unless allowlisted) |
| `edit` | Exact-match replacement | **yes** | ask (unless allowlisted) |
| `glob` | Find files by pattern | no | none |
| `grep` | Search file contents | no | none |
| `bash` | Execute shell command | **yes** | ask, per-command scoped |
| `task` | Spawn subagent | no* | none (budget-capped) |
| `plan` | Write/update task plan | no | none |
| `todo` | Track multi-step progress | no | none |

\* `task` doesn't mutate directly, but its subagent can.

---

## 2. `read`

```json
{
  "name": "read",
  "description": "Read a file from the filesystem. Returns numbered lines. Prefer this over `cat` via bash — output is bounded, numbered, and the read is tracked for edit safety.",
  "input_schema": {
    "type": "object",
    "properties": {
      "path":   {"type":"string","description":"Absolute path to the file."},
      "offset": {"type":"integer","description":"1-indexed line to start from. Use with limit for large files."},
      "limit":  {"type":"integer","description":"Max lines to return. Default 2000."}
    },
    "required": ["path"]
  }
}
```

**Semantics**
- Output is `cat -n` style: `<line-number>\t<content>`. Line numbers let `edit` and the
  model refer to positions unambiguously.
- Default limit 2000 lines. On truncation, append:
  `[truncated: showing lines 1-2000 of 8431. Use offset/limit to read more.]`
- Lines longer than 2000 chars are truncated individually with `[...line truncated]`.
- **Records the read** in session state. `edit` requires a prior read of the same file
  (see §4) — this is the single most effective guard against destructive edits.
- Binary files: detect and refuse with the file type, don't dump bytes.
- Images/PDFs: return as structured content if the model supports it, else describe and refuse.
- Empty file: return `[file exists but is empty]`, **not** an empty string. An empty string
  reads as failure to the model.

**Errors**
| Condition | Message |
|---|---|
| Missing | `File not found: {path}. Use glob to locate it, or check the parent directory exists.` |
| Is a directory | `{path} is a directory, not a file. Use glob("{path}/**") to list contents.` |
| No permission | `Permission denied reading {path}.` |
| Outside workspace | `{path} is outside the session workspace ({root}). Access denied.` |
| Relative path | `Path must be absolute. Did you mean {cwd}/{path}?` |

---

## 3. `write`

```json
{
  "name": "write",
  "description": "Write content to a file, creating it or overwriting it entirely. For modifying part of an existing file, use `edit` instead — it is safer and cheaper.",
  "input_schema": {
    "type":"object",
    "properties":{
      "path":    {"type":"string","description":"Absolute path."},
      "content": {"type":"string","description":"Complete file content."}
    },
    "required":["path","content"]
  }
}
```

**Semantics**
- Creates parent directories only if the operator enables it; otherwise error (surprise
  directory creation is a common agent failure).
- **Overwriting an existing file requires a prior `read`** of it in this session. Without
  it: error, don't ask. This prevents the model from destroying content it never saw.
- Writes atomically: temp file in the same directory, then rename. A partial write on
  crash is worse than no write.
- Preserves existing file mode; new files get 0644 (or operator default).
- Returns bytes written and whether the file was created or overwritten — never a bare "ok".

**Errors**
| Condition | Message |
|---|---|
| Overwrite without read | `Refusing to overwrite {path} — it exists but has not been read this session. Call read({path}) first to see what you would replace.` |
| Parent missing | `Parent directory does not exist: {dir}. Create it with bash mkdir -p first.` |
| Read-only FS | `Filesystem is read-only at {path}.` |
| Disk full | `Write failed: no space left on device.` |

---

## 4. `edit` — the highest-stakes tool

```json
{
  "name":"edit",
  "description":"Replace an exact string in a file. The old_string must match the file content exactly, including whitespace and indentation. Include enough surrounding context to make the match unique.",
  "input_schema":{
    "type":"object",
    "properties":{
      "path":       {"type":"string","description":"Absolute path."},
      "old_string": {"type":"string","description":"Exact text to replace, including indentation. Must be unique in the file unless replace_all is true."},
      "new_string": {"type":"string","description":"Replacement text. Must differ from old_string."},
      "replace_all":{"type":"boolean","description":"Replace every occurrence. Default false."}
    },
    "required":["path","old_string","new_string"]
  }
}
```

**Why exact-match and not line numbers or diffs:** line numbers drift the moment anything
above changes, and models produce malformed unified diffs at a meaningful rate. Exact string
matching fails *loudly and safely* — a non-match changes nothing, and the error tells the
model precisely what to fix. This is the design decision that makes reliable editing possible.

**Semantics**
- `old_string` must match **byte-exactly**, including leading whitespace. Do not normalize,
  trim, or fuzzy-match. Fuzzy matching produces silent wrong edits — the worst outcome.
- **Requires a prior `read` of the file this session.** No exceptions.
- If `old_string` appears more than once and `replace_all` is false: **error with the count
  and the line numbers of each occurrence**, so the model can add context and retry. Never
  pick one arbitrarily.
- If `old_string == new_string`: error. This is always a model mistake.
- Empty `old_string` is only valid for a new file (equivalent to `write`); on an existing
  file, error.
- Atomic write, same as `write`.
- **Returns a snippet of the changed region with line numbers**, so the model sees the result
  without re-reading — saves a turn and confirms the edit landed where intended.

**Errors — these matter more than the happy path**

| Condition | Message |
|---|---|
| No match | `old_string not found in {path}.\nThe file may have changed, or whitespace/indentation may differ.\nNearest partial match at line {n}:\n{context}\nRe-read the file and copy the exact text.` |
| Multiple matches | `old_string appears {n} times in {path} (lines {list}).\nAdd surrounding context to make it unique, or set replace_all: true.` |
| No prior read | `Refusing to edit {path} — not read this session. Call read({path}) first.` |
| Identical strings | `old_string and new_string are identical — this edit would do nothing.` |
| File changed since read | `{path} changed on disk since you read it. Re-read before editing.` |

The "nearest partial match" in the no-match error is worth the implementation cost: it turns
a dead-end into a recoverable turn. Compute it with a similarity pass over candidate windows.

---

## 5. `glob`

```json
{
  "name":"glob",
  "description":"Find files matching a glob pattern. Returns paths sorted by modification time, newest first.",
  "input_schema":{
    "type":"object",
    "properties":{
      "pattern":{"type":"string","description":"Glob, e.g. **/*.ts or src/**/*.test.js"},
      "path":   {"type":"string","description":"Directory to search from. Defaults to workspace root."}
    },
    "required":["pattern"]
  }
}
```

**Semantics**
- Sorted by mtime descending — recently-touched files are usually the relevant ones.
- Respects `.gitignore` and an operator ignore list by default. Honors `node_modules`,
  `.git`, `target`, `dist`, `__pycache__` exclusions.
- Caps at 1000 results with an explicit truncation note and a suggestion to narrow.
- Returns `[no files matched: {pattern}]` on empty — never a bare empty list.

---

## 6. `grep`

```json
{
  "name":"grep",
  "description":"Search file contents with a regular expression. This is the primary code-navigation tool — prefer it over reading files speculatively.",
  "input_schema":{
    "type":"object",
    "properties":{
      "pattern":     {"type":"string","description":"Regular expression (RE2 syntax)."},
      "path":        {"type":"string","description":"File or directory to search. Defaults to workspace root."},
      "glob":        {"type":"string","description":"Filter files by glob, e.g. *.go"},
      "output_mode": {"type":"string","enum":["content","files_with_matches","count"],"description":"Default files_with_matches."},
      "context":     {"type":"integer","description":"Lines of context around each match (content mode only)."},
      "case_insensitive":{"type":"boolean"},
      "multiline":   {"type":"boolean","description":"Allow . to match newlines."}
    },
    "required":["pattern"]
  }
}
```

**Semantics**
- **RE2, not PCRE** — no backtracking, so no catastrophic-regex DoS from model output.
  Document this: models will try lookaheads. Reject with a message naming the unsupported
  construct and suggesting an alternative.
- Default `files_with_matches` keeps output small; the model escalates to `content` when it
  needs detail. This ordering is a context-budget decision (P2).
- Content mode caps at 100 matches and 50 lines per file, with truncation notes.
- Backed by ripgrep where available, with a pure-Go fallback for the air-gapped bundle.

**Errors**
| Condition | Message |
|---|---|
| Invalid regex | `Invalid pattern: {err}. Abhed uses RE2 syntax — lookahead/backreference are unsupported. Rewrite without {construct}.` |
| No matches | `No matches for {pattern}{in path}. Try a broader pattern or check the path.` |

---

## 7. `bash`

```json
{
  "name":"bash",
  "description":"Run a shell command in the session sandbox. Use for builds, tests, git, and package managers. Prefer read/glob/grep for file inspection — they are cheaper and safer.",
  "input_schema":{
    "type":"object",
    "properties":{
      "command":    {"type":"string"},
      "description":{"type":"string","description":"Short human-readable description shown in the approval prompt."},
      "timeout_ms": {"type":"integer","description":"Default 120000, max 600000."},
      "background": {"type":"boolean","description":"Run detached; returns a handle. Use for servers and long builds."}
    },
    "required":["command","description"]
  }
}
```

**Semantics**
- Runs inside the session microVM (§03-security I3). Never on the host.
- **Only a call that is just `cd <folder>` carries its directory to the next call**;
  a cd inside a longer command, and shell state (env vars, functions), do **not** — each
  call is a fresh shell. Document this explicitly: models assume otherwise and it causes
  confusing failures.
- Combined stdout+stderr, capped at 30k chars with head+tail retained on truncation (the
  middle is usually least useful; the error is at the end).
- Exit code always reported. Non-zero is **not** a tool error — it's a valid observation the
  model must reason about. Never convert a failing test run into a tool failure.
- `description` is required because it's what the human sees in the approval prompt. A tool
  that asks for approval without saying what it does is unusable.
- Background mode returns a handle; output retrievable and the process killable.
- The command runs in a session of its own, with no controlling terminal, so a read of
  `/dev/tty` fails at once. A cancelled call (an interrupt, Send now, a shutdown, the
  timeout) kills that session's process group, in every sandbox tier, and a container is
  removed. A job started with `&` that still holds the output when the command exits
  does not hold the call, whatever the exit status: the result says it is still running,
  and its later output is not shown.
- **Interactive commands are rejected** with guidance (`git rebase -i`, `vim`, anything
  needing a TTY) — they hang forever otherwise.

**Approval** — per-command scoped, not per-tool (P7). `bash(npm test)` allowlisted does not
allowlist `bash(rm -rf /)`. Destructive patterns (`rm -rf`, `dd`, `mkfs`, `:(){ :|:& };:`,
force-push) require confirmation **in every mode**, including the most permissive.

---

## 8. `task` — subagent spawn

```json
{
  "name":"task",
  "description":"Spawn a subagent with a fresh context to handle a self-contained subtask. Use when a task requires extensive exploration whose intermediate detail you do not need. The subagent returns only a summary.",
  "input_schema":{
    "type":"object",
    "properties":{
      "prompt":     {"type":"string","description":"Complete, self-contained task. The subagent sees none of this conversation."},
      "description":{"type":"string","description":"3-5 word label."},
      "agent_type": {"type":"string","description":"Which subagent profile: explore | test | review | general."},
      "max_turns":  {"type":"integer"}
    },
    "required":["prompt","description"]
  }
}
```

**Semantics** (per P3, arch §3)
- Fresh context: own system prompt + ABHED.md, **no parent turns**.
- Returns a bounded summary (target 1–2k tokens). Truncate and note if exceeded.
- **Budget is hierarchical** — subagent spend counts against the parent cap. On exhaustion,
  spawn fails cleanly with `Budget limit reached` and running subagents are stopped.
- **Nested spawning disabled by default**; concurrency capped (default 20).
- The prompt must be self-contained. Enforce this in the description — the single most common
  failure is a prompt referencing "the file we discussed."

---

## 9. `plan` and `todo`

Lightweight, but they carry real weight: they are the model's externalized working memory,
and they survive compaction when the conversation doesn't (P4).

- **`plan`** — write or update a structured plan for the current task. Persisted to the
  session and re-injected after compaction. Used by plan mode (§07-ux).
- **`todo`** — a checklist with states (`pending` / `in_progress` / `done`). Exactly one item
  `in_progress` at a time; enforce it. Rendered live in the CLI so the human can see progress
  without reading the transcript.

Both are cheap to call and should be called often on multi-step work. The system prompt
should say so explicitly.

---

## 10. Conformance suite

Every tool ships with tests that a new model must pass before serving traffic (arch §5,
capability probe). These are the tests that catch adapter bugs before users do:

| Test | Asserts |
|---|---|
| Schema round trip | Model emits valid args for every tool |
| Exact-match edit | Whitespace-sensitive replacement succeeds |
| Multi-match rejection | Ambiguous edit errors rather than guessing |
| Read-before-edit | Unread-file edit is refused |
| Error recovery | Model recovers from each documented error within 2 turns |
| Truncation handling | Model uses offset/limit after a truncation notice |
| Non-zero exit | Model reasons about a failing test rather than retrying blindly |
| Path discipline | Relative paths corrected, escapes refused |
| Budget exhaustion | Subagent cap produces clean failure, not a hang |

**The error-recovery test is the important one.** It measures whether your error messages
actually teach — which is the difference between a tool set that works and one that
frustrates the model into loops.
