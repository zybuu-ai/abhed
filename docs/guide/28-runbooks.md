# Runbooks: approve a plan once, run it step by step

A plan from plan mode can carry a **runbook**: the exact tool calls it will
make, in order. You approve the runbook once, with the plan. After that, a
call the agent makes exactly as a step lists runs without asking you again,
and anything else asks, saying it is **not in the approved plan**. Abhed holds
the agent to the steps you read; the agent cannot widen them.

This is for operational work where the commands are known before they run:
rotating certificates across a fleet, a rolling restart, a patch round. It
works for `ssh`, `k8s_apply`, `bash`, `edit` and `write`.

## What a runbook is

The agent calls `exit_plan` in plan mode with its plan in markdown and,
optionally, a `runbook`: a list of steps.

| Field | For |
|---|---|
| `tool` | `ssh`, `k8s_apply`, `bash`, `edit` or `write` |
| `target` | `ssh`: the host. `k8s_apply`: `cluster/namespace` (the cluster alone for a cluster-wide resource). `edit`, `write`: the path. `bash` takes none |
| `command` | `ssh` and `bash`: the command, exactly as it will run |
| `manifest` | `k8s_apply`: the manifest, for `action: apply` |
| `args` | any other argument of the call, as the tool takes it: `action`, `resource`, `name`, `replicas`; `old_string`, `new_string`, `content`; `timeout_seconds` |
| `note` | a few words on what the step is for |

An `ssh` step must name its host and a `k8s_apply` step its cluster, so a
step cannot come to mean another machine later.

When the plan is proposed, Abhed turns each step into the call it stands for,
exactly as the loop will see it when it is made (canonical arguments, a
credential where a secret's name belongs withheld, a Kubernetes manifest in
its one encoding), and hashes it. The plan's hash covers its text and every
step's hash, in order. A step is refused at once, and the agent told why,
when the tool is not one of the five, is not available in the session, its
arguments cannot be read, or **the policy would refuse it**: a deny rule
outside plan mode refuses the whole runbook, not only that step later.

Plan mode itself stays read-only. Proposing a runbook changes nothing.

## Approving it

The CLI's plan card lists every step under the plan, with the call as it will
run:

```
  Proceed with this plan?
  │ Rotate the TLS certificate on web1 and web2 …
  │ Runbook · 12 steps · plan 9e15e1b9ae66
  │
  │ 1. ssh · web1 — check the current expiry
  │ ```
  │ openssl x509 -enddate -noout -in /etc/nginx/tls/site.crt
  │ ```
  │ …
  proposed by the agent with exit_plan; it changes nothing until you answer; yes approves its
  12 runbook steps: each runs without asking when it is made exactly as listed, and any other change asks
  1. Yes, and accept edits
  2. Yes, ask before each change
  3. No, keep planning (tell it what to change)
```

The three answers are the ones every plan has. Either yes binds the runbook.
**A step that cannot be undone is confirmed a second time**, as each would
have been on its own: a destructive command (`rm -rf`, `git reset --hard`,
the policy's destructive step), the same commands over `ssh`, and a
Kubernetes `delete`. The second question lists them, and its default is no:

```
  2 steps of this runbook cannot be undone
  │ - step 3, ssh web1: recursive/forced delete on web1
  │ - step 7, k8s_apply lab/demo: deletes deployments web from the cluster
  1. No, keep planning
  2. Yes, approve the steps that cannot be undone
```

Accepting records `plan.decided` with the runbook and its hash, changes the
mode as any accepted plan does (`mode.changed`, `via: plan-exit`), and the
conversation goes on with each step's exact call, so the model makes them as
approved.

The same card is in the workbench (`/ide`) and the console, and Abhed Studio
gets it over ACP as a permission request with the CLI's three answers as its
options. In the workbench the answer is `POST /v1/sessions/{id}/plan` with
`decision` (`accept-edits`, `approve` or `keep-planning`) and the `plan_hash`
from `plan.proposed`; `confirm_no_undo: true` is required when a step cannot
be undone. A server session leaves plan mode only for `default`, or for
`accept-edits` when that is the server's configured mode.

## While it runs

- **The next step, exactly, runs on the plan's approval.** Its decision is
  recorded as `action.approved` with `step: plan`, `by: plan`, `plan_hash`
  and `plan_step`, followed by `plan.step`.
- **Steps run in order.** A call matching a later step, before the steps
  ahead of it, asks: "not in the approved plan (it is step 5, out of order:
  step 3 is next)". If you allow it, that step is marked run. Each step runs
  once; the same call again asks.
- **Any other change asks**, saying it is not in the approved plan, even
  where an allow rule, "always allow" or the mode (accept-edits, auto,
  bypass) would have let it run. It offers no "always allow". A read-only
  call is judged by the policy as usual.
- **A changed argument is another call.** One more flag, another host, a
  `timeout_seconds` the step did not list: it asks.
- **Approval only replaces the ask.** Deny rules still refuse an approved
  step, hooks still judge it (a hook on asks may still refuse it), and the
  sandbox, the egress allowlist and the `ssh.connect_hosts` bounds still
  apply. An ask from the supervisor, or for a call that may already have run
  on a process that stopped, still goes to you.
- **Some asks are never the plan's to answer.** The plan answers only the
  asks of the default and mode steps, a destructive command's, and an ask
  rule's from your own or the workspace's configuration. Every other ask
  still asks for each call inside a runbook too: an ask rule the managed
  configuration lists; any ask rule of a policy layer an edition added (a
  Team workspace's); any ask no rule made, which is Abhed's own (a command
  that names workspace trust for a nested run, and any like it added
  later); a command too tangled for the rules to read in full (step
  `screen`); and the asks of the supervisor, of a hook and of a handover.
- **An approval gate decides a runbook as it decides any ask.** On a server
  whose edition has approvers, accepting a plan with a runbook is one answer
  to the gate, for the plan's hash: an answer it refuses (such as approving
  your own runbook) binds nothing, one it holds keeps the plan waiting, and
  the answer that settles it binds the runbook. A waiting runbook is listed
  with the session's other pending approvals as one request, `plan:<hash>`
  (tool `exit_plan`, the plan clipped and one line per step), and approvers
  the gate allows answer it with `POST /v1/sessions/{id}/plan`, or with
  `POST /v1/sessions/{id}/approve` and `request_id: "plan:<hash>"` (add
  `confirm_no_undo: true` when a step cannot be undone; there is no "always
  allow"). `plan.decided` names who answered (`by`). When an approver settles
  it, the plan's message is queued for the owner's next message, so the
  agent carries it out once the owner goes on.
- **The supervisor is never answered by the plan.** When it has a concern
  with a step, the step is put to you (`asked again: supervisor: …`), and a
  refusal of its stays a refusal. What the approved runbook names counts as
  named by you, so the supervisor's rules about paths, hosts and programs
  nobody named are satisfied for the steps you approved. Its credential rule
  still holds: once a step has brought credential content into the session
  (a command that prints a key file, or output that holds key material),
  every later call that can send data out asks. A step that only names a key
  file, as a copy on the host does, brings nothing into the session.
- **Subagents are never steps.** A subagent's change in a session with a
  runbook asks.
- **A new plan lets the runbook go.** Proposing another plan unbinds it,
  and accepting a plan without a runbook leaves none bound.
- **Rewinding or continuing a session lets the runbook go.** A rewound
  conversation, and one continued with `-c` or `-r`, starts unbound: its
  remote steps cannot be taken back, so each asks again. Propose the plan
  again to bind it. A new accepted plan replaces the old runbook.

## Headless runs

With no one to ask, a runbook is approved only by the operator's own
setting. `-runbook` reads a file in `exit_plan`'s own shape, so a plan
proposed in a conversation can be saved and run:

```sh
abhed -p -runbook rotate.json -runbook-sha256 9e15e1b9ae66… "rotate the certificates"
```

`-runbook-sha256` pins the plan hash you reviewed: a file that hashes
otherwise is refused before anything runs. The binding is recorded as
`plan.proposed` and `plan.decided` with `via: operator`. Steps made as
listed run; any other change asks, and a headless run refuses what asks.

The SDK takes the same as `Options.Runbook`, and `abhed rpc` as `runbook`
on `start`.

## Example: rotating a TLS certificate across hosts

Two web servers, `web1` and `web2`, serve a certificate from an internal CA
that renews with the existing certificate (smallstep's `step ca renew`
here; `certbot renew` has the same shape). Every command is known in
advance, so the whole rotation is a runbook, and **the private key never
leaves the host**: it is renewed in place, and nothing secret passes through
the model.

```json
"ssh": { "enabled": true,
  "hosts": [ { "name": "web1", "address": "10.0.0.11", "user": "deploy" },
             { "name": "web2", "address": "10.0.0.12", "user": "deploy" } ] },
"permissions": { "deny": ["ssh(*/rm -rf /*)"] }
```

In plan mode, ask: "rotate the TLS certificate on web1 and web2, one host at
a time, with a backup and a verification". The agent proposes:

```json
{
  "plan": "Rotate the certificate on web1, then web2: back up, renew, check the configuration, reload, verify.",
  "runbook": [
    {"tool": "ssh", "target": "web1", "command": "openssl x509 -enddate -noout -in /etc/nginx/tls/site.crt", "note": "current expiry"},
    {"tool": "ssh", "target": "web1", "command": "cp -p /etc/nginx/tls/site.crt /etc/nginx/tls/site.crt.bak && cp -p /etc/nginx/tls/site.key /etc/nginx/tls/site.key.bak", "note": "back up"},
    {"tool": "ssh", "target": "web1", "command": "step ca renew --force /etc/nginx/tls/site.crt /etc/nginx/tls/site.key", "note": "renew in place"},
    {"tool": "ssh", "target": "web1", "command": "nginx -t", "note": "pre-flight"},
    {"tool": "ssh", "target": "web1", "command": "systemctl reload nginx"},
    {"tool": "ssh", "target": "web1", "command": "echo | openssl s_client -connect localhost:443 2>/dev/null | openssl x509 -noout -enddate", "note": "verify"},
    {"tool": "ssh", "target": "web2", "command": "openssl x509 -enddate -noout -in /etc/nginx/tls/site.crt", "note": "current expiry"},
    {"tool": "ssh", "target": "web2", "command": "cp -p /etc/nginx/tls/site.crt /etc/nginx/tls/site.crt.bak && cp -p /etc/nginx/tls/site.key /etc/nginx/tls/site.key.bak", "note": "back up"},
    {"tool": "ssh", "target": "web2", "command": "step ca renew --force /etc/nginx/tls/site.crt /etc/nginx/tls/site.key", "note": "renew in place"},
    {"tool": "ssh", "target": "web2", "command": "nginx -t", "note": "pre-flight"},
    {"tool": "ssh", "target": "web2", "command": "systemctl reload nginx"},
    {"tool": "ssh", "target": "web2", "command": "echo | openssl s_client -connect localhost:443 2>/dev/null | openssl x509 -noout -enddate", "note": "verify"}
  ]
}
```

You read the twelve commands once and approve. With the supervisor off,
the rotation then runs without twelve separate prompts. With it on, as it is
by default, it runs the same way: step 2 copies `site.key` on the host and
prints none of it, so the supervisor has no concern. Had a step printed the
key (`cat site.key`), every `ssh` call after it would ask, since an `ssh`
call can send data out; the plan does not answer the supervisor's asks. If `nginx -t` fails on `web1`, the agent's
next move is not in the plan: a rollback such as `mv site.crt.bak site.crt
&& systemctl reload nginx` asks you, named as outside the plan, and so does
any extra diagnosis it tries.

Where a step's content only exists at run time, such as a CSR generated on a
host and signed on a CA host, leave that step out of the runbook: it asks,
and you see the real content before it runs.

Afterwards, `abhed hawkeye` lists what reached each host:

```
  Remote changes
  ssh web1
    #14   exit 0    allowed plan    openssl x509 -enddate -noout -in /etc/nginx/tls/site.crt   [plan 9e15e1b9ae66 step 1]
    #19   exit 0    allowed plan    cp -p /etc/nginx/tls/site.crt /etc/nginx/tls/site.crt.bak && cp -p /et…   [plan 9e15e1b9ae66 step 2]
    …
  ssh web2
    …
```

Each line has the decision and who made it, the exit code, and the runbook
step it was; the HTML report and the workbench's HawkEYE panel show the same
per host and cluster ([HawkEYE](15-hawkeye.md)).

## Turning runbooks off

```json
"plan": { "runbooks": false }
```

With `plan.runbooks` false, `exit_plan` refuses a plan that carries a
runbook (the agent is told to present it without one), and `-runbook`,
`Options.Runbook` and `rpc`'s `runbook` are refused. Plans without a runbook
are unchanged, and every call asks as it would. A workspace file may only
turn runbooks off; the managed file's value binds either way.

## What it does not do

- It does not make plan mode run anything. Plan mode still refuses every
  change, over `ssh` as anywhere.
- It does not judge whether a step is wise. You do, once, when you read it.
- It is not a two-person approval: whoever answers the card approves every
  step.
- A step's arguments are matched exactly, not as a pattern: a runbook cannot
  approve "any `openssl` command on web1".
