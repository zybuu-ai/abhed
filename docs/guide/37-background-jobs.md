# Background jobs

`abhed bg` starts an agent on an Abhed server that runs as you, under the
server's policy, and keeps going after this command exits. On a server
people sign in to, it works in a sandbox of its own, as each session there
does. You follow it, answer what it asks and stop it from the same command.

The server side is a Team feature (`server.background`), documented with the
Team and Enterprise edition. A Community server answers that it runs no
background jobs, and a Community `abhed serve` refuses
`server.background.enabled`. Only the operator's configuration sets
`server.background`: a workspace's file never does, trusted or not. This page is the command line and the protocol,
which the Community Edition carries so any client can speak to such a server.

## Signing in

`abhed bg` uses the profiles `abhed login` saves
([TLS and command-line sign-in](26-tls-and-cli-login.md)). Every command
takes `-profile NAME` or `-server URL` (default: the current profile) and
`-o table|json`.

## Commands

```
abhed bg start [-name N] [-mode plan] [-provider P] [-y | -no-snapshot] [-attach] PROMPT
abhed bg list
abhed bg logs ID [-patch]
abhed bg attach ID
abhed bg cancel ID
```

- `start` sends the prompt and, if given, a name, a mode and a provider.
  Who the job runs as is your sign-in, and its sandbox, secrets and network
  are the server's. A mode wider than the server's is refused as it is for
  a session.
- `start` also sends a snapshot of the repository you run it in: its
  tracked files as they are in your working tree, staged and unstaged edits
  included, untracked files not. The job starts with them in a folder of
  its own, which the server removes when the job ends. In a terminal
  `start` first says how many files and how much it would send, and to
  which server, and sends them only on `y`; `-y` sends without asking, and
  without a terminal `-y` or `-no-snapshot` is required. Left out, and named
  as `start` runs: a file your read rules withhold from the agent (deny
  rules, and the ask rules such as the built-in ones for `.env` and keys),
  by every set of rules a session in the repository would follow: the
  root's, those of the folder you run `start` in and every folder between,
  and those of any other folder in the repository with a `.abhed` of its
  own, each loaded with its own folder's trust (read rules name paths, not
  contents: a tracked hard link to a withheld file is sent under its own,
  allowed name, as a session there could read it); a
  file named like a key, holding one by its shape, or holding a value from
  your secrets store; links, submodules and any file over 16 MiB. A
  snapshot is at most 50,000 files, 512 MiB, 64 MiB compressed. When the
  rules or your secrets store cannot be read, no snapshot is sent.
  `-no-snapshot` starts in an empty folder; outside a repository `start`
  asks for it.
- `list` shows your jobs, newest first, with the tool a waiting job asks for.
- `logs` prints the job's record so far; `-patch` prints what it changed
  as one unified diff. A job that started from a snapshot keeps its patch
  when it ends; otherwise the patch is there while the server that ran it
  still holds the originals.
- `attach` prints the record as it grows. When the job waits on you it
  shows the tool, the reason and the call's arguments (a command in full,
  each value cut at 2,000 characters, secrets withheld as the record keeps
  them) and, in a terminal, asks `Allow it? [y/N]`; only `y` allows, and
  the answer names the request shown. Without a terminal it only says what
  waits and until when. Ctrl-C leaves, at the question too; the job runs
  on. It exits 0 only for a job that ended done.
- `cancel` stops the job and every background task it started.

Everything the server sends is shown with hidden characters escaped, so a
job's name or record cannot drive your terminal. Every command is refused
inside an agent's own command: a job runs as you, with your secrets.

Exit status: 0 done, 1 failed (or the job attached to did not end done),
2 usage, 3 not signed in or not allowed, 4 no such job, 5 not in a state for
this or over your share of the server.

## The protocol

Package `github.com/zybuu-ai/abhed/bgjob` has the types, the paths and a Go
client. A job is a server session; its record, changes and answers are the
session's own routes.

| Route | |
|---|---|
| `POST /v1/bg/snapshots` | a snapshot, `application/gzip`: a tar of regular files only, relative slash-separated names with nothing under `.git` or `.abhed`, modes 0644 or 0755, within the limits above; 201 with `{"id", "files", "bytes", "expires"}` |
| `POST /v1/bg/jobs` | start: `{"prompt", "name", "mode", "provider", "workspace": {"kind": "session"}}`, or `"workspace": {"kind": "snapshot", "snapshot": ID}`; 202 with the job |
| `GET /v1/bg/jobs/{id}/patch` | the patch a job kept when it ended (its `result`), as text |
| `GET /v1/bg/jobs` | your jobs: `{"jobs": [...]}` |
| `GET /v1/bg/jobs/{id}` | one job; `waiting` holds the ask it waits on: tool, reason, the redacted `args`, `request_id` and `deadline` |
| `POST /v1/bg/jobs/{id}/cancel` | stop it |
| `GET /v1/bg/approvals` | every ask your jobs wait on |
| `POST /v1/sessions/{session_id}/approve` | answer one: `{"approved", "request_id"}` |
| `GET /v1/sessions/{session_id}/replay`, `/changes` | the record and the changes |

A job's `state` is `running`, `waiting`, or one it does not leave: `done`,
`failed`, `cancelled`, or `timed_out`, a job stopped because an ask went
unanswered past the server's timeout, with nothing it asked for approved.
