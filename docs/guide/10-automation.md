# Automation

Three ways to run Abhed without a person at the prompt.

## Headless

```bash
abhed -p "fix the failing tests" -mode auto -allow 'bash(go test*)'
abhed -p "explain what pkg/auth does" -mode plan
abhed -p "add a test for Valid" -output-format json > events.jsonl
```

Exit codes: `0` completed · `2` turn limit · `3` budget · `4` policy denied ·
`5` retries exhausted · `130` interrupted. A CI job can branch on those.

There is no one to approve, so anything needing approval is refused. Name what
may run with `-allow`, and keep the list narrow.

## RPC

For a caller that is not Go, `abhed rpc` speaks line-delimited JSON on stdin and
stdout — no server, no port, no auth for what is one process talking to its own
child.

```python
p = subprocess.Popen(["abhed", "rpc"], stdin=PIPE, stdout=PIPE, text=True, bufsize=1)

def send(**kw):
    p.stdin.write(json.dumps(kw) + "\n"); p.stdin.flush()

send(method="start", mode="auto")
send(method="prompt", prompt="fix the failing tests")
# every event arrives as {"type":"event", ...} while it works
```

| Method | |
|---|---|
| `start` | open a session — `workspace`, `mode`, `allow`, `deny` |
| `prompt` | send a prompt, get the final answer |
| `steer` | redirect a run in progress |
| `usage` | tokens, turns, compactions |
| `export` | the HTML transcript |
| `providers` | what this build supports |
| `quit` | close |

Events stream as they happen rather than only at the end, so a caller can render
progress. `steer` is why this is a persistent process rather than one request
per run.

## Resolving an issue

```bash
abhed resolve https://git.example.com/team/tool/issues/12 -allow 'bash(go test*)'
```

reads the issue, works on it in a branch of its own (`abhed/issue-12`, in a
worktree, so your checkout is untouched), commits what changed, pushes, and
opens a pull request that links the issue and summarises the diff. **GitHub,
GitLab and Gitea/Forgejo** are one command; a self-hosted instance is the
ordinary case: the kind is inferred from the host, `-kind` names it, and
`-ca file.pem` (or `ABHED_FORGE_CA`) trusts your certificate authority.

The token is `GITHUB_TOKEN`, `GITLAB_TOKEN` or `GITEA_TOKEN` from the
environment, or the same name in [`abhed secret`](04-permissions.md#secrets).
It is used here, outside the session — the run sees the issue text and the
checkout, never the token — and it reaches `git push` through the
environment, not the command line.

Opening the request is a mutating action of its own, `forge_pr`, judged by
policy like any other: a deny rule refuses it, an allow rule
(`forge_pr(team/tool)`) or `-y` permits it, and otherwise you are asked at
the terminal. No mode opens a pull request on its own. A run that changes
nothing pushes nothing and exits 2; a run that fails leaves the worktree for
inspection.

Not here: the bot that reacts to labels and comments. That is a multi-user
feature and lives with the server.

## Editors

`abhed acp` speaks the [Agent Client Protocol](https://agentclientprotocol.com)
over stdio, so an editor that supports it — Zed, JetBrains, and others — runs
Abhed as its agent without an extension per product. Point the editor at the
binary:

```json
{"agent_servers": {"Abhed": {"command": "abhed", "args": ["acp"]}}}
```

The editor's approval dialog is the approver: an `ask` decision becomes a
permission request with *Allow once*, *Always allow* the rule policy suggests,
and *Deny*. It can answer an ask; it cannot lift a deny rule, and the sandbox
tier and the workspace boundary are whatever the configuration says, exactly
as from the terminal. The agent's text, its reasoning, every tool call with
its outcome, the plan and the context usage stream to the editor as
`session/update` notifications, and the session is recorded like any other.

Not yet supported: `session/load` (resuming an editor session from the
record) and editor-side modes. A conformance test drives the adapter with a
scripted client, so no editor is needed in CI.

## Server

```bash
abhed serve -addr :8420
```

A web console with an event stream, inline approvals and session history, plus a
JSON API. This is the multi-user path: local accounts or a reverse proxy's
identity headers, and with Postgres storage it enforces tenant isolation in the
database with row-level security. OIDC sign-in is part of the Enterprise
Edition.

```bash
B=http://127.0.0.1:8420
curl -s $B/v1/health
SID=$(curl -s -X POST $B/v1/sessions -d '{"prompt":"...","mode":"plan"}' | jq -r .session_id)
curl -sN $B/v1/sessions/$SID/events     # live
curl -s  $B/v1/sessions/$SID/replay     # the full audit trail
```

Sending a message to a session that is already working **steers** it rather than
being refused.
